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

// DeviceFlowRealmGlobal 设备流区域标识：空/其它 = 国内，此值 = 国际。
const DeviceFlowRealmGlobal = "global"

// DeviceFlowDomain 该区域的默认登录域（凭证 domain 的兜底值）。区域判定靠
// domain 后缀（TencentRegion），设备流没回 domain 或与所选区域矛盾时用它补齐，
// 否则账号会被按相反区域路由（区域由 base 决定，HANDOFF §6.6）。
func DeviceFlowDomain(realm string) string {
	if strings.EqualFold(realm, DeviceFlowRealmGlobal) {
		return "www.workbuddy.ai"
	}
	return "www.codebuddy.cn"
}

// deviceFlowBase 设备流登录的 (base, origin)：与请求路径不同，设备流**没有凭证**
// 可依（domain 是登录的产出），区域必须由调用方显式给出——国内 base
// copilot.tencent.com（登录页 codebuddy.cn），国际 base www.workbuddy.ai
// （登录页含 Google/GitHub）。OMNIGATE_TENCENT_BASE 覆盖 base（测试/实验）。
func (c *TencentClient) deviceFlowBase(realm string) (base, origin string) {
	if strings.EqualFold(realm, DeviceFlowRealmGlobal) {
		base, origin = tencentBaseGlobal, tencentOriginGlobal
	} else {
		base, origin = tencentBaseCN, tencentOriginCN
	}
	if c.base != "" {
		base = c.base
	}
	return base, origin
}

// DeviceFlowState 发起设备流授权：POST {base}/v2/plugin/auth/state?platform=CLI
// （body {}）。返回浏览器授权链接与 state（后续轮询/取账号用）。
// realm 选定区域（本地 state/token/account 三段必须同区域，state 只在对应 base 有效）。
func (c *TencentClient) DeviceFlowState(realm string) (authURL, state string, err error) {
	base, origin := c.deviceFlowBase(realm)
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
func (c *TencentClient) DeviceFlowToken(realm, state string) (DeviceFlowToken, bool, error) {
	base, origin := c.deviceFlowBase(realm)
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
func (c *TencentClient) DeviceFlowAccount(realm, state, accessToken string) (uid, enterpriseID, nickname string, err error) {
	base, origin := c.deviceFlowBase(realm)
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
