// 腾讯设备流登录能力（SPEC §24.3）：面板 OAuth 与 login-tencent CLI 共用同一
// 实现来源——三端点（state/token/account）的 envelope 解析与 pending 语义
// 只在此处定义，避免双份实现漂移。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DeviceFlowToken 设备流授权结果。
type DeviceFlowToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Domain       string
}

// DeviceFlowState 发起设备流授权：POST {base}/v2/plugin/auth/state?platform=CLI
// （body {}）。返回浏览器授权链接与 state（后续轮询/取账号用）。
func (c *TencentClient) DeviceFlowState() (authURL, state string, err error) {
	base, origin := c.resolve("")
	req, err := http.NewRequest(http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", strings.NewReader("{}"))
	if err != nil {
		return "", "", err
	}
	tencentCommonHeaders(req, origin)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("auth state http %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := parseEnvelopeData(raw, &st); err != nil {
		return "", "", err
	}
	if st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state: missing state or authUrl: %s", truncateStr(string(raw), 200))
	}
	return st.AuthURL, st.State, nil
}

// DeviceFlowToken 轮询授权结果：GET {base}/v2/plugin/auth/token?state=。
// pending（业务 code!=0，如 11217 "login ing"，或 accessToken 为空）→
// (zero, false, nil)；成功 → (token, true, nil)；网络/解析错误 → error。
func (c *TencentClient) DeviceFlowToken(state string) (DeviceFlowToken, bool, error) {
	base, origin := c.resolve("")
	req, err := http.NewRequest(http.MethodGet, base+"/v2/plugin/auth/token?state="+state, nil)
	if err != nil {
		return DeviceFlowToken{}, false, err
	}
	tencentCommonHeaders(req, origin)
	resp, err := c.http.Do(req)
	if err != nil {
		return DeviceFlowToken{}, false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
		Data *struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return DeviceFlowToken{}, false, fmt.Errorf("parse token: %w", err)
	}
	if env.Code != 0 || env.Data == nil || env.Data.AccessToken == "" {
		return DeviceFlowToken{}, false, nil // pending：等用户完成浏览器授权
	}
	return DeviceFlowToken{
		AccessToken:  env.Data.AccessToken,
		RefreshToken: env.Data.RefreshToken,
		ExpiresIn:    env.Data.ExpiresIn,
		Domain:       env.Data.Domain,
	}, true, nil
}

// DeviceFlowAccount 取账号信息：GET {base}/v2/plugin/login/account?state=（Bearer）。
func (c *TencentClient) DeviceFlowAccount(state, accessToken string) (uid, enterpriseID, nickname string, err error) {
	base, origin := c.resolve("")
	req, err := http.NewRequest(http.MethodGet, base+"/v2/plugin/login/account?state="+state, nil)
	if err != nil {
		return "", "", "", err
	}
	tencentCommonHeaders(req, origin)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := parseEnvelopeData(raw, &acct); err != nil {
		return "", "", "", err
	}
	if acct.UID == "" {
		return "", "", "", fmt.Errorf("login/account: missing uid: %s", truncateStr(string(raw), 200))
	}
	return acct.UID, acct.EnterpriseID, acct.Nickname, nil
}

// parseEnvelopeData 解析设备流响应：{code,msg,data:{...}} envelope → data 目标；
// code!=0 或 data 缺失 → 错误（业务失败）。
func parseEnvelopeData(raw []byte, dst any) error {
	var env struct {
		Code int64           `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parse envelope: %w", err)
	}
	if env.Code != 0 {
		if env.Msg == "" {
			env.Msg = "business error"
		}
		return fmt.Errorf("business error code=%d msg=%s", env.Code, truncateStr(env.Msg, 200))
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("missing data: %s", truncateStr(string(raw), 200))
	}
	return json.Unmarshal(env.Data, dst)
}
