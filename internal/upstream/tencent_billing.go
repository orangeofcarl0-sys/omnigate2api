// 腾讯计费/活动面（SPEC §24.2/§32）：每日签到（=Buddy 加油站积分）、积分余额、
// 签到活动状态与成长中心宠物探险（四端点状态机）。对齐多个开源项目实证形态。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"omnigate2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 计费/活动面（SPEC §24.2/§32 落地）
// ---------------------------------------------------------------------------

// BillingAPI 上游计费/活动能力（腾讯实现；华为不实现，调度器/管理按家族断言分发）。
type BillingAPI interface {
	// DailyCheckin 每日签到（Buddy 加油站积分）：已签到（code=10001）按幂等成功，
	// 返回结果含本次获得积分/连续天数（SPEC §32.2）。
	DailyCheckin(acct *auth.Auth) (*CheckinResult, error)
	// CheckinStatus 签到活动状态（主题/连续/今日可得/活动累计/周期）。
	CheckinStatus(acct *auth.Auth) (*CheckinStatus, error)
	// UserResource 返回当前可花费积分余额（多套餐 Cycle 优先聚合，负值钳 0）。
	UserResource(acct *auth.Auth) (remain int64, err error)
	// PetTravelStatus 成长中心宠物探险状态（idle|traveling|arrived）。
	PetTravelStatus(acct *auth.Auth) (*PetTravel, error)
	// PetDepart 派出宠物探险（config 地点 id，原生 json.Number 保类型透传）。
	PetDepart(acct *auth.Auth, locationID json.Number) error
	// PetClaim 领取归来积分（recordID 为 status.record_id，2026-09 契约必需；
	// 空则退化为空 body）；无未领奖励返回 ErrPetNoUnclaimed。
	PetClaim(acct *auth.Auth, recordID string) (int64, error)
	// PetTravelConfig 探险可选地点（首个即默认目的地）。
	PetTravelConfig(acct *auth.Auth) ([]PetLocation, error)
	// PetQuota 宠物盲盒能量额度（affordable/max_open_count，能量为开盒唯一出口）。
	PetQuota(acct *auth.Auth) (*PetQuota, error)
	// PetOpenBox 开宠物盲盒（能量足够时激活/扩充宠物）。
	PetOpenBox(acct *auth.Auth, count int) error
	// GrowthTasks 成长中心任务列表（accept_status/reward_credit/reward_energy）。
	GrowthTasks(acct *auth.Auth) ([]GrowthTask, error)
	// GrowthAcceptTasks 批量接单（≤20/批，返回 task_code → 结果状态）。
	GrowthAcceptTasks(acct *auth.Auth, codes []string) (map[string]string, error)
	// GrowthClaimTask 领取已完成任务奖励；已领 already=true。
	GrowthClaimTask(acct *auth.Auth, code string) (credit, energy int64, already bool, err error)
	// ReportActive 活跃上报（chat_request_send 单事件；宠物领养前置解锁，SPEC §32.7）。
	ReportActive(acct *auth.Auth) error
	// PetAdopt 领养首只宠物：agreement({"agree":true}) → buddy/first；
	// 成功直接发放 credit+energy（社区实证 +300c+8e），幂等（已领养返回既有态）。
	PetAdopt(acct *auth.Auth) (credit, energy int64, err error)
}

// CheckinResult 签到结果（幂等重复签到 Already=true，Credit 为本轮所得）。
// Inactive：签到活动未开启/已过期（2026-09-23 全球版实证：与 CN「已签到」
// 共用业务码 10001，语义按消息文本分流）。
type CheckinResult struct {
	Already    bool
	Inactive   bool
	Reason     string
	Credit     int64
	StreakDays int64
}

// CheckinStatus 签到活动状态（checkin-activity-status 响应 data）。
// Active 为指针：字段缺失（CN 形态）= nil → 不做预检跳过；显式 false → 活动未开启。
type CheckinStatus struct {
	Active         *bool  `json:"active"`
	ThemeName      string `json:"theme_name"`
	TodayCheckedIn bool   `json:"today_checked_in"`
	StreakDays     int64  `json:"streak_days"`
	TodayCredit    int64  `json:"today_credit"`
	TotalCredits   int64  `json:"total_credits"`
	StartTime      string `json:"start_time"`
	EndTime        string `json:"end_time"`
}

// PetTravel 宠物探险状态（travel/status 响应 data）。
type PetTravel struct {
	State             string      `json:"state"` // idle | traveling | arrived
	DailyLimitReached bool        `json:"daily_limit_reached"`
	ArriveAt          int64       `json:"arrive_at"`
	ServerNow         int64       `json:"server_now"`
	RewardCredit      int64       `json:"reward_credit"`
	RecordID          json.Number `json:"record_id"` // 归来领取凭据（claim body）
	Location          struct {
		Name string `json:"name"`
	} `json:"location"`
}

// PetLocation 探险地点（travel/config locations 元素）。id 真实形态为数字
// （活测实证 2026-09-06），json.Number 保类型透传进 depart body。
type PetLocation struct {
	ID               json.Number `json:"id"`
	Name             string      `json:"name"`
	DurationHoursMin int         `json:"duration_hours_min"`
	DurationHoursMax int         `json:"duration_hours_max"`
}

// ErrPetNoUnclaimed 宠物无未领取奖励（幂等：已领过）。
var ErrPetNoUnclaimed = errors.New("pet: no unclaimed reward")

// activityBaseFor 活动域（SPEC §32.2/§32.6 域感知）：global（.workbuddy.ai）→
// 全球 base（活测实证 2026-09-21：成长中心/任务 200、契约与 CN 同源），
// CN → copilot.tencent.com；OMNIGATE_ACTIVITY_BASE 覆盖（测试/实验）。
func (c *TencentClient) activityBaseFor(domain string) string {
	if v := os.Getenv("OMNIGATE_ACTIVITY_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if tencentRegion(domain) {
		return tencentBaseGlobal
	}
	return "https://copilot.tencent.com"
}

// billingBaseFor 计费域：global（.workbuddy.ai）→ www.workbuddy.ai，否则
// www.codebuddy.cn；OMNIGATE_BILLING_BASE 覆盖（测试/实验）。
func (c *TencentClient) billingBaseFor(domain string) string {
	if v := os.Getenv("OMNIGATE_BILLING_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if tencentRegion(domain) {
		return "https://www.workbuddy.ai"
	}
	return "https://www.codebuddy.cn"
}

// billingHeaders 计费请求头：Bearer + 账号标识（含 X-Tenant-Id=EnterpriseID），
// 对齐参考实现 BillingHeaders。
func billingHeaders(req *http.Request, cred SignCredential) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", TencentClientUA)
	if cred.SecurityToken != "" {
		req.Header.Set("Authorization", "Bearer "+cred.SecurityToken)
	}
	if cred.UserID != "" {
		req.Header.Set("X-User-Id", cred.UserID)
	}
	if cred.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
		req.Header.Set("X-Tenant-Id", cred.EnterpriseID)
	}
	if cred.Domain != "" {
		req.Header.Set("X-Domain", cred.Domain)
	}
}

// billingCred 从 auth 组装计费凭证。
func billingCred(acct *auth.Auth) SignCredential {
	return SignCredential{
		SecurityToken: acct.CloudDragonTok,
		UserID:        acct.UserID,
		EnterpriseID:  acct.EnterpriseID,
		Domain:        acct.Domain,
	}
}

// DailyCheckin 每日签到（幂等：code=10001 已签到按成功，解析 credit/streak）。
func (c *TencentClient) DailyCheckin(acct *auth.Auth) (*CheckinResult, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for checkin")
	}
	raw, status, err := c.billingPost(acct, "/v2/billing/meter/daily-checkin", []byte("{}"))
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit     int64 `json:"credit"`
			Reward     int64 `json:"reward_credit"`
			StreakDays int64 `json:"streak_days"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	// 真实形态：已签到为 HTTP 400 + code=10001（8f 实测）——业务码优先于状态码判定。
	// 10001 双语义（2026-09-23 全球版实证）：CN「今天已签到」= 幂等成功；
	// 全球「签到活动未开启或已过期」= 活动态，非成功非错误，如实上报。
	if env.Code == 10001 {
		if strings.Contains(env.Msg, "已签到") || strings.Contains(strings.ToLower(env.Msg), "already") {
			return &CheckinResult{Already: true, Credit: env.Data.Credit, StreakDays: env.Data.StreakDays}, nil
		}
		return &CheckinResult{Inactive: true, Reason: truncateStr(env.Msg, 120)}, nil
	}
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("daily-checkin failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	credit := env.Data.Credit
	if credit == 0 {
		credit = env.Data.Reward
	}
	return &CheckinResult{Credit: credit, StreakDays: env.Data.StreakDays}, nil
}

// billingPost 计费域 POST：返回（原始体, HTTP 状态, 传输错误）——业务码判定
// 归调用方（HTTP 4xx 也可能是幂等成功，如签到 10001）。
func (c *TencentClient) billingPost(acct *auth.Auth, path string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodPost, c.billingBaseFor(acct.Domain)+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	billingHeaders(req, billingCred(acct))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// CheckinStatus 查询签到活动状态（SPEC §32.2）。
func (c *TencentClient) CheckinStatus(acct *auth.Auth) (*CheckinStatus, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for checkin status")
	}
	raw, status, err := c.billingPost(acct, "/v2/billing/meter/checkin-activity-status", []byte("{}"))
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int64         `json:"code"`
		Msg  string        `json:"msg"`
		Data CheckinStatus `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("status parse: %w", uerr)
	}
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("checkin-status failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	return &env.Data, nil
}

// UserResource 查询可花费积分余额（多套餐聚合规则对齐参考实现）。
func (c *TencentClient) UserResource(acct *auth.Auth) (int64, error) {
	if acct == nil {
		return 0, fmt.Errorf("account required for balance")
	}
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.billingBaseFor(acct.Domain)+"/v2/billing/meter/get-user-resource", bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	billingHeaders(req, billingCred(acct))
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("get-user-resource http %d: %s", resp.StatusCode, truncateStr(string(data), 200))
	}
	var r struct {
		Response struct {
			Data struct {
				Accounts []struct {
					CapacityRemain      int64 `json:"CapacityRemain"`
					CapacityUsed        int64 `json:"CapacityUsed"`
					CycleCapacitySize   int64 `json:"CycleCapacitySize"`
					CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return 0, fmt.Errorf("resource parse: %w", err)
	}
	var remain int64
	for _, a := range r.Response.Data.Accounts {
		v := a.CapacityRemain
		switch {
		case a.CycleCapacitySize > 0:
			v = a.CycleCapacityRemain
		case a.CycleCapacityRemain > 0 || a.CycleCapacityUsed > 0:
			v = a.CycleCapacityRemain
		}
		if v < 0 {
			v = 0
		}
		remain += v
	}
	return remain, nil
}

// ---------------------------------------------------------------------------
// 成长中心宠物探险（SPEC §32.2 拍板：四端点状态机，只走 API 直连）
// ---------------------------------------------------------------------------

// petRequest 活动域请求（Bearer + X-User-Id，参数与 billingHeaders 同源）。
func (c *TencentClient) petRequest(acct *auth.Auth, method, path string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.activityBaseFor(acct.Domain)+path, rd)
	if err != nil {
		return nil, 0, err
	}
	billingHeaders(req, billingCred(acct))
	if len(body) == 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// PetTravelStatus 查询宠物探险状态。
func (c *TencentClient) PetTravelStatus(acct *auth.Auth) (*PetTravel, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for pet status")
	}
	raw, status, err := c.petRequest(acct, http.MethodGet, "/activity/growth/buddy/travel/status", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int64     `json:"code"`
		Msg  string    `json:"msg"`
		Data PetTravel `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("pet status parse: %w", uerr)
	}
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("pet status failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	return &env.Data, nil
}

// PetTravelConfig 探险可选地点（首个为默认目的地）。
func (c *TencentClient) PetTravelConfig(acct *auth.Auth) ([]PetLocation, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for pet config")
	}
	raw, status, err := c.petRequest(acct, http.MethodGet, "/activity/growth/buddy/travel/config", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Locations []PetLocation `json:"locations"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("pet config parse: %w", uerr)
	}
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("pet config failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	return env.Data.Locations, nil
}

// PetDepart 派出宠物前往地点（locationID 为 config 原生 id，数字/字符串保类型）。
func (c *TencentClient) PetDepart(acct *auth.Auth, locationID json.Number) error {
	if acct == nil {
		return fmt.Errorf("account required for pet depart")
	}
	body, _ := json.Marshal(map[string]any{"location_id": locationID})
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/travel/depart", body)
	if err != nil {
		return err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &env)
	if status >= 400 || env.Code != 0 {
		return fmt.Errorf("pet depart failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	return nil
}

// PetClaim 领取归来奖励（recordID 非空时按 2026-09 契约带 record_id）；
// 已领/无未领（400 no unclaimed）→ ErrPetNoUnclaimed（幂等）。
func (c *TencentClient) PetClaim(acct *auth.Auth, recordID string) (int64, error) {
	if acct == nil {
		return 0, fmt.Errorf("account required for pet claim")
	}
	body := []byte("{}")
	if recordID != "" && recordID != "0" {
		body, _ = json.Marshal(map[string]any{"record_id": json.RawMessage(recordID)})
	}
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/travel/claim", body)
	if err != nil {
		return 0, err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit int64 `json:"credit"`
			Reward int64 `json:"reward_credit"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Code == 400 && strings.Contains(strings.ToLower(env.Msg), "no unclaimed") {
		return 0, ErrPetNoUnclaimed
	}
	if status >= 400 || env.Code != 0 {
		return 0, fmt.Errorf("pet claim failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	credit := env.Data.Credit
	if credit == 0 {
		credit = env.Data.Reward
	}
	return credit, nil
}

// PetQuota 宠物盲盒能量额度（能量为开盒唯一出口）。Balance 为能量余额
// （全球版实测存在 2026-09-23）。
type PetQuota struct {
	Affordable   int `json:"affordable"`
	MaxOpenCount int `json:"max_open_count"`
	CostPerOpen  int `json:"cost_per_open"`
	Balance      int `json:"balance"`
}

// PetQuota 查询宠物盲盒能量额度。
func (c *TencentClient) PetQuota(acct *auth.Auth) (*PetQuota, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for pet quota")
	}
	raw, status, err := c.petRequest(acct, http.MethodGet, "/activity/growth/buddy/quota", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Code  int64    `json:"code"`
		Msg   string   `json:"msg"`
		Quota PetQuota `json:"-"`
		Data  PetQuota `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("pet quota parse: %w", uerr)
	}
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("pet quota failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	// 字段可能在顶层或 data 内（两版契约兼容，quota 优先 data）
	q := env.Data
	if q.Affordable == 0 && q.MaxOpenCount == 0 {
		var top struct {
			Affordable   int `json:"affordable"`
			MaxOpenCount int `json:"max_open_count"`
		}
		_ = json.Unmarshal(raw, &top)
		q.Affordable, q.MaxOpenCount = top.Affordable, top.MaxOpenCount
	}
	return &q, nil
}

// PetOpenBox 开宠物盲盒（能量足够时激活宠物；count 由调用方按 quota 决定）。
func (c *TencentClient) PetOpenBox(acct *auth.Auth, count int) error {
	if acct == nil {
		return fmt.Errorf("account required for pet open")
	}
	if count < 1 {
		count = 1
	}
	tok, _ := RandomHex(16)
	body, _ := json.Marshal(map[string]any{"count": count, "client_token": tok})
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/open", body)
	if err != nil {
		return err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &env)
	if status >= 400 || env.Code != 0 {
		return fmt.Errorf("pet open failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	return nil
}

// ---------------------------------------------------------------------------
// 成长中心任务（SPEC §32 阶段 3：积分/能量主来源，宠物盲盒能量的唯一入口）
// ---------------------------------------------------------------------------

// GrowthTask 成长中心任务（真实契约，活测 2026-09-20 实证：字段为 code/status，
// 首态 available；88lin 快照的 task_code/accept_status 是旧一代命名）。
type GrowthTask struct {
	Code         string `json:"code"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	LevelName    string `json:"level_name"`
	Locked       bool   `json:"locked"`
	Status       string `json:"status"` // available|accepted|in_progress|completed|claimed
	RewardCredit int64  `json:"reward_credit"`
	RewardEnergy int64  `json:"reward_energy"`
}

// TaskCode 兼容两代命名的任务编码（code 优先）。
func (t GrowthTask) TaskCode() string {
	if t.Code != "" {
		return t.Code
	}
	return ""
}

// TaskStatus 兼容两代命名的状态（status 优先）。
func (t GrowthTask) TaskStatus() string {
	return t.Status
}

// GrowthTasks 查询任务列表。
func (c *TencentClient) GrowthTasks(acct *auth.Auth) ([]GrowthTask, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for growth tasks")
	}
	raw, status, err := c.petRequest(acct, http.MethodGet, "/activity/growth/tasks", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Code  int64        `json:"code"`
		Msg   string       `json:"msg"`
		Tasks []GrowthTask `json:"tasks"`
		Data  struct {
			Tasks []GrowthTask `json:"tasks"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("growth tasks parse: %w", uerr)
	}
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("growth tasks failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	if len(env.Tasks) > 0 {
		return env.Tasks, nil
	}
	return env.Data.Tasks, nil
}

// GrowthAcceptTasks 批量接单（≤20/批）；返回 task_code → status/message。
func (c *TencentClient) GrowthAcceptTasks(acct *auth.Auth, codes []string) (map[string]string, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for task accept")
	}
	if len(codes) == 0 {
		return map[string]string{}, nil
	}
	if len(codes) > 20 {
		codes = codes[:20]
	}
	body, _ := json.Marshal(map[string]any{"task_codes": codes})
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/tasks/accept", body)
	if err != nil {
		return nil, err
	}
	type acceptResult struct {
		TaskCode string `json:"task_code"`
		Status   string `json:"status"`
		Message  string `json:"message"`
	}
	var env struct {
		Code    int64          `json:"code"`
		Msg     string         `json:"msg"`
		Results []acceptResult `json:"results"`
		Data    struct {
			Results []acceptResult `json:"results"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("task accept failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	// 真实嵌套为 data.results（2026-09-23 实证）；顶层形态兼容保留。
	results := env.Results
	if len(results) == 0 {
		results = env.Data.Results
	}
	out := make(map[string]string, len(results))
	for _, r := range results {
		msg := r.Status
		if r.Message != "" {
			msg += ": " + r.Message
		}
		out[r.TaskCode] = msg
	}
	return out, nil
}

// GrowthClaimTask 领取任务奖励（路径带 task_code，空 body）。
func (c *TencentClient) GrowthClaimTask(acct *auth.Auth, code string) (int64, int64, bool, error) {
	if acct == nil {
		return 0, 0, false, fmt.Errorf("account required for task claim")
	}
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/tasks/"+code+"/claim", []byte("{}"))
	if err != nil {
		return 0, 0, false, err
	}
	var env struct {
		Code           int64  `json:"code"`
		Msg            string `json:"msg"`
		Credit         int64  `json:"credit"`
		Energy         int64  `json:"energy"`
		AlreadyClaimed bool   `json:"already_claimed"`
	}
	_ = json.Unmarshal(raw, &env)
	if status >= 400 || env.Code != 0 {
		return 0, 0, false, fmt.Errorf("task claim failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	return env.Credit, env.Energy, env.AlreadyClaimed, nil
}

// DebugGet 活动域 GET 原样返回（cmd/probe 实证用；生产路径不经此）。
func (c *TencentClient) DebugGet(acct *auth.Auth, path string) ([]byte, int, error) {
	return c.petRequest(acct, http.MethodGet, path, nil)
}

// DebugPost 计费域 POST {} 原样返回（cmd/probe 实证用；生产路径不经此）。
func (c *TencentClient) DebugPost(acct *auth.Auth, path string) ([]byte, int, error) {
	return c.billingPost(acct, path, []byte("{}"))
}

// DebugPostBody 活动域 POST 指定 body 原样返回（cmd/probe 实证用）。
func (c *TencentClient) DebugPostBody(acct *auth.Auth, path, body string) ([]byte, int, error) {
	if body == "" {
		body = "{}"
	}
	return c.petRequest(acct, http.MethodPost, path, []byte(body))
}

// ---------------------------------------------------------------------------
// 活跃上报与宠物领养（SPEC §32.7；社区实证：report 前置 → agreement → buddy/first）
// ---------------------------------------------------------------------------

// ReportActive 上报一条 chat_request_send 活跃事件（领养链路的前置解锁；
// 事件形状对齐官方客户端埋点，缺 userId 服务端静默丢弃）。
func (c *TencentClient) ReportActive(acct *auth.Auth) error {
	if acct == nil {
		return fmt.Errorf("account required for report")
	}
	now := time.Now().UnixMilli()
	cid := fmt.Sprintf("wgw-%d", now)
	ev := map[string]any{
		"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
		"mode": "craft", "conversationId": cid, "requestId": cid,
		"inputLength": 12, "requestModelId": "glm-5.2", "requestModelName": "GLM-5.2",
		"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
		"codebaseEnable": false, "maxToken": 0, "maxSteps": 0, "temperature": 0,
		"maxRetries": 0, "mentionContexts": []any{}, "knowledgeId": []any{},
		"knowledgeName": []any{}, "codebaseId": "", "mentionContextCount": 0,
		"command": "", "expertId": "", "recommendId": "", "skillId": "",
		"skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
		"traceId": "", "rootRequestId": cid, "parentConversationId": cid,
		"agentName": "default", "agentType": "conversation", "userId": acct.UserID,
	}
	body, _ := json.Marshal([]any{ev})
	req, err := http.NewRequest(http.MethodPost, c.billingBaseFor(acct.Domain)+"/v2/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	billingHeaders(req, billingCred(acct))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("report failed http=%d: %s", resp.StatusCode, truncateStr(string(raw), 160))
	}
	return nil
}

// PetAdopt 领养首只宠物：先同意领养协议，再领养；成功直接发放 credit+energy。
// 幂等：已领养时两步接口均返回既有态（不视为错误）。
func (c *TencentClient) PetAdopt(acct *auth.Auth) (int64, int64, error) {
	if acct == nil {
		return 0, 0, fmt.Errorf("account required for pet adopt")
	}
	agree, _ := json.Marshal(map[string]any{"agree": true})
	if _, st, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/agreement", agree); err != nil {
		return 0, 0, fmt.Errorf("buddy agreement: %w", err)
	} else if st >= 400 {
		return 0, 0, fmt.Errorf("buddy agreement http=%d", st)
	}
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/first", []byte("{}"))
	if err != nil {
		return 0, 0, err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit       int64 `json:"credit"`
			Energy       int64 `json:"energy"`
			RewardCredit int64 `json:"reward_credit"`
			RewardEnergy int64 `json:"reward_energy"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	if status >= 400 || env.Code != 0 {
		return 0, 0, fmt.Errorf("buddy first failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	credit, energy := env.Data.Credit, env.Data.Energy
	if credit == 0 {
		credit = env.Data.RewardCredit
	}
	if energy == 0 {
		energy = env.Data.RewardEnergy
	}
	return credit, energy, nil
}
