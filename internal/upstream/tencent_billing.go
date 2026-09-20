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
	// PetDepart 派出宠物探险（config 地点 id）。
	PetDepart(acct *auth.Auth, locationID string) error
	// PetClaim 领取归来积分；无未领奖励（code=400 no unclaimed）返回 ErrPetNoUnclaimed。
	PetClaim(acct *auth.Auth) (int64, error)
	// PetTravelConfig 探险可选地点（首个即默认目的地）。
	PetTravelConfig(acct *auth.Auth) ([]PetLocation, error)
}

// CheckinResult 签到结果（幂等重复签到 Already=true，Credit 为本轮所得）。
type CheckinResult struct {
	Already    bool
	Credit     int64
	StreakDays int64
}

// CheckinStatus 签到活动状态（checkin-activity-status 响应 data）。
type CheckinStatus struct {
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
	State             string `json:"state"` // idle | traveling | arrived
	DailyLimitReached bool   `json:"daily_limit_reached"`
	ArriveAt          int64  `json:"arrive_at"`
	ServerNow         int64  `json:"server_now"`
	RewardCredit      int64  `json:"reward_credit"`
	Location          struct {
		Name string `json:"name"`
	} `json:"location"`
}

// PetLocation 探险地点（travel/config locations 元素）。
type PetLocation struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	DurationHoursMin int    `json:"duration_hours_min"`
	DurationHoursMax int    `json:"duration_hours_max"`
}

// ErrPetNoUnclaimed 宠物无未领取奖励（幂等：已领过）。
var ErrPetNoUnclaimed = errors.New("pet: no unclaimed reward")

// activityBaseFor 活动域（SPEC §32.2）：copilot.tencent.com（脚本实证），
// OMNIGATE_ACTIVITY_BASE 覆盖（测试/实验）。
func (c *TencentClient) activityBaseFor() string {
	if v := os.Getenv("OMNIGATE_ACTIVITY_BASE"); v != "" {
		return strings.TrimRight(v, "/")
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
	// 真实形态：已签到为 HTTP 400 + code=10001（8f 实测）——业务码优先于状态码判定
	if env.Code == 10001 {
		return &CheckinResult{Already: true, Credit: env.Data.Credit, StreakDays: env.Data.StreakDays}, nil
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
	req, err := http.NewRequest(method, c.activityBaseFor()+path, rd)
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

// PetDepart 派出宠物前往地点。
func (c *TencentClient) PetDepart(acct *auth.Auth, locationID string) error {
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

// PetClaim 领取归来奖励；已领/无未领（400 no unclaimed）→ ErrPetNoUnclaimed（幂等）。
func (c *TencentClient) PetClaim(acct *auth.Auth) (int64, error) {
	if acct == nil {
		return 0, fmt.Errorf("account required for pet claim")
	}
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/travel/claim", []byte("{}"))
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
