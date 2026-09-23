// 腾讯计费面（SPEC §24.2/§32）：每日签到（=Buddy 加油站积分）、签到活动状态、
// 积分余额，以及 BillingAPI 契约与计费域/base 选择。
package upstream

import (
	"bytes"
	"encoding/json"
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
	// ReportDesktopChat 上报桌面端成功对话六连事件链（宠物领养前置解锁，
	// SPEC §32.8；含桌面指纹，chat 域 /v2/report）。
	ReportDesktopChat(acct *auth.Auth) error
	// ReportTaskEvents 上报任务事件包（六连 + chat×5 + GLM + canvas + automation
	// + 夜猫窗口 black_cat，SPEC §32.8；任务完成引擎）。
	ReportTaskEvents(acct *auth.Auth) error
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
	DailyCredit    int64  `json:"daily_credit"`
	TotalCredits   int64  `json:"total_credits"`
	StartTime      string `json:"start_time"`
	EndTime        string `json:"end_time"`
}

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
	if err := checkBiz("daily-checkin", status, env.Code, env.Msg); err != nil {
		return nil, err
	}
	credit := env.Data.Credit
	if credit == 0 {
		credit = env.Data.Reward
	}
	return &CheckinResult{Credit: credit, StreakDays: env.Data.StreakDays}, nil
}

// billingPost 计费域 POST（统一机制·SPEC §32）。
func (c *TencentClient) billingPost(acct *auth.Auth, path string, body []byte) ([]byte, int, error) {
	return c.billingDo(acct, http.MethodPost, path, body)
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
	if err := checkBiz("checkin-status", status, env.Code, env.Msg); err != nil {
		return nil, err
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
