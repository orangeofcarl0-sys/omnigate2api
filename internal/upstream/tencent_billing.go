// 腾讯计费面（SPEC §24.2 落地）：每日签到与积分余额，对齐 workbuddy2api 实证形态。
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
// 计费面：每日签到与积分余额（SPEC §24.2 落地，对齐 workbuddy2api 实证形态）
// ---------------------------------------------------------------------------

// BillingAPI 上游计费能力（腾讯实现；华为不实现，调度器/管理按家族断言分发）。
type BillingAPI interface {
	// DailyCheckin 每日签到：已签到（code=10001）视为成功（幂等）；
	// 其它业务错误 code!=0 返回错误。
	DailyCheckin(acct *auth.Auth) error
	// UserResource 返回当前可花费积分余额（多套餐 Cycle 优先聚合，负值钳 0）。
	UserResource(acct *auth.Auth) (remain int64, err error)
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

// DailyCheckin 每日签到（幂等：code=10001 已签到视为成功）。
func (c *TencentClient) DailyCheckin(acct *auth.Auth) error {
	if acct == nil {
		return fmt.Errorf("account required for checkin")
	}
	req, err := http.NewRequest(http.MethodPost, c.billingBaseFor(acct.Domain)+"/v2/billing/meter/daily-checkin", strings.NewReader("{}"))
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
		return fmt.Errorf("daily-checkin http %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Code != 0 && env.Code != 10001 {
		return fmt.Errorf("daily-checkin failed code=%d msg=%s", env.Code, truncateStr(env.Msg, 200))
	}
	return nil
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
