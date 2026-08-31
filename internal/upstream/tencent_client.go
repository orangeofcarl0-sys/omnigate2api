// 腾讯（WorkBuddy/CodeBuddy，copilot.tencent.com）上游客户端（SPEC §23.2/§28.4）。
//
// 与华为签发的差异：无 AK/SK 签名，Bearer accessToken + X-User-Id/X-Enterprise-Id/
// X-Domain/X-Product 系列头（空字段按官方 CLI 约定发 X-No-*: 1 占位）；refresh 走
// /v2/plugin/auth/token/refresh（X-Refresh-Token 头，无 body，响应为
// {code,msg,data:{accessToken,refreshToken,expiresIn,domain}} envelope）；聊天端点
// /v2/chat/completions。请求头对齐官方 CLI（workbuddy2api 逆向实证）：UA
// "CLI/2.63.2 CodeBuddy/2.63.2"、Accept "application/json, text/plain, */*"，
// 并按凭证 domain 后缀切换区域（.workbuddy.ai → global base/Origin/Referer）。
// 会话无 chat_id 语义（全量发送，CHE session.kind=none）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// 官方 CLI 头保真常量（workbuddy2api headers.go 实证值，SPEC §28.4 决策 C）。
// TencentClientUA 导出：cmd/login-tencent 等腾讯家族工具复用同一值，避免多份
// 常量漂移（A2）。
const (
	TencentClientUA     = "CLI/2.63.2 CodeBuddy/2.63.2"
	tencentClientAccept = "application/json, text/plain, */*"
	tencentBaseCN       = "https://copilot.tencent.com"
	tencentBaseGlobal   = "https://www.workbuddy.ai"
	tencentOriginCN     = "https://www.codebuddy.cn"
	tencentOriginGlobal = "https://www.workbuddy.ai"
)

// TencentClient 腾讯 copilot 上游客户端。
type TencentClient struct {
	http       *http.Client
	streamHTTP *http.Client
	base       string // OMNIGATE_TENCENT_BASE 覆盖（测试/实验）；空 = 按 domain 区域解析
}

// NewTencent 构造腾讯客户端；base 可由 OMNIGATE_TENCENT_BASE 覆盖（测试/实验），
// 缺省按凭证 domain 后缀区域解析（global: www.workbuddy.ai / CN: copilot.tencent.com）。
func NewTencent(timeout time.Duration) *TencentClient {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := &http.Transport{
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
	}
	return &TencentClient{
		http:       &http.Client{Timeout: timeout, Transport: tr},
		streamHTTP: &http.Client{Transport: tr},
		base:       strings.TrimRight(os.Getenv("OMNIGATE_TENCENT_BASE"), "/"),
	}
}

// tencentRegion 按凭证 domain 后缀判定区域：.workbuddy.ai → global 域，
// 否则 CN 默认（base/Origin/Referer/计费域共用同一判定，单点收敛）。
func tencentRegion(domain string) bool {
	return strings.HasSuffix(strings.TrimSpace(domain), ".workbuddy.ai")
}

// resolve 按凭证 domain 后缀解析 (base, origin)：区域判定见 tencentRegion；
// OMNIGATE_TENCENT_BASE 仅覆盖 base（测试/实验，Origin 仍按域规则）。
func (c *TencentClient) resolve(domain string) (base, origin string) {
	global := tencentRegion(domain)
	if global {
		base = tencentBaseGlobal
		origin = tencentOriginGlobal
	} else {
		base = tencentBaseCN
		origin = tencentOriginCN
	}
	if c.base != "" {
		base = c.base
	}
	return base, origin
}

// tencentCommonHeaders 通用浏览器厂商头（对齐官方 CLI：UA/Accept/Origin/Referer）。
func tencentCommonHeaders(req *http.Request, origin string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", tencentClientAccept)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", TencentClientUA)
}

// tencentChatHeaders chat 请求头：Bearer + 账号标识 + 产品约定；空字段按官方
// CLI 约定发 X-No-*: 1 占位（保持请求形态完整，SPEC §28.4 决策 C）。
func tencentChatHeaders(req *http.Request, cred SignCredential, origin string) {
	tencentCommonHeaders(req, origin)
	if cred.SecurityToken != "" {
		req.Header.Set("Authorization", "Bearer "+cred.SecurityToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if cred.UserID != "" {
		req.Header.Set("X-User-Id", cred.UserID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if cred.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if cred.Domain != "" {
		req.Header.Set("X-Domain", cred.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", "SaaS")
}

// ChatStream 发送 /v2/chat/completions：roles 消息保真透传（tools 原样拼入 body），
// 模型名不做映射（上游按真实 ID 匹配，参考实现实证）；toolChoice 已按上游
// string 语义归一化（§28.4 决策 D：none 在上层已删 tools，此处仅拼非空值）。
func (c *TencentClient) ChatStream(ctx context.Context, chatID string, messages []ChatMessage, traceID string, cred SignCredential, userName string, model string, tools []map[string]any, toolChoice string) (io.ReadCloser, error) {
	body := map[string]any{
		"model":    model,
		"stream":   true,
		"messages": marshalChatMessages(messages),
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if toolChoice != "" {
		body["tool_choice"] = toolChoice
	}
	raw, _ := json.Marshal(body)
	base, origin := c.resolve(cred.Domain)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	tencentChatHeaders(req, cred, origin)
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode,
			Message: truncateStr(string(rb), 200), Path: "/v2/chat/completions"}
	}
	return resp.Body, nil
}

// RefreshToken 通过 /v2/plugin/auth/token/refresh 换新凭证（X-Refresh-Token 头，
// 无 body，对齐参考实现）。响应为 {code,msg,data:{accessToken,refreshToken,
// expiresIn,domain}} envelope（code==0 成功）；data 缺省时回退扁平结构以兼容
// 测试上游。返回 TokenResponse：SecurityToken=accessToken、Expiration=now+expiresIn。
func (c *TencentClient) RefreshToken(ctx context.Context, cfg LoginConfig, refreshToken, codeVerifier, domain string) (*TokenResponse, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("no refresh_token available")
	}
	base, origin := c.resolve(domain)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return nil, err
	}
	tencentCommonHeaders(req, origin)
	req.Header.Set("X-Refresh-Token", refreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode,
			Message: truncateStr(string(raw), 200), Path: "/v2/plugin/auth/token/refresh"}
	}
	tok, err := parseTencentTokenEnvelope(raw)
	if err != nil {
		return nil, err
	}
	exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339)
	if tok.ExpiresIn <= 0 {
		exp = "" // 缺 expiresIn：保留旧过期时间（上层处理）
	}
	return &TokenResponse{
		UserName:     "",
		RefreshToken: tok.RefreshToken,
		Credentials:  Credentials{SecurityToken: tok.AccessToken, Expiration: exp},
	}, nil
}

// tencentTokenData refresh 响应的令牌三元组。
type tencentTokenData struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
}

// parseTencentTokenEnvelope 解析 refresh 响应：优先 envelope
// {code,msg,data:{...}}（code==0 成功，非 0 → 业务错误）；data 缺省回退扁平
// 结构（兼容既有假上游测试）。accessToken 缺失 → refresh_failed。
func parseTencentTokenEnvelope(raw []byte) (*tencentTokenData, error) {
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
		Data *struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) == nil && (env.Code != 0 || env.Data != nil) {
		if env.Code != 0 {
			if env.Msg == "" {
				env.Msg = "business error"
			}
			return nil, fmt.Errorf("refresh_failed: %s", truncateStr(env.Msg, 200))
		}
		if env.Data.AccessToken == "" {
			return nil, fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
		}
		return &tencentTokenData{AccessToken: env.Data.AccessToken, RefreshToken: env.Data.RefreshToken, ExpiresIn: env.Data.ExpiresIn}, nil
	}
	var flat struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(raw, &flat); err != nil || flat.AccessToken == "" {
		return nil, fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	return &tencentTokenData{AccessToken: flat.AccessToken, RefreshToken: flat.RefreshToken, ExpiresIn: flat.ExpiresIn}, nil
}

// ---------------------------------------------------------------------------
// 腾讯错误语义（SPEC §28.4 决策 B，对齐 workbuddy2api Classify 实证形态）
// ---------------------------------------------------------------------------

// TencentErrKind 腾讯错误语义分类。
type TencentErrKind int

const (
	TencentErrOther       TencentErrKind = iota
	TencentErrHardCredit                 // 402 / 积分不足：冷却至次日 04:00
	TencentErrSessionDead                // 12153 / Offline user session not found：永久禁用
)

// tencentHardCreditMarkers 额度耗尽标记（中英双通道，对齐参考实现）。
var tencentHardCreditMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// tencentSessionDeadMarkers 会话死亡标记（离线会话/账号失效）。
var tencentSessionDeadMarkers = []string{"12153", "offline user session not found"}

// ClassifyTencent 按状态码 + 响应体判定腾讯侧错误语义（仅硬额度/会话死亡两类
// 需要专属动作；429/5xx 由通用映射继续处理）。
func ClassifyTencent(status int, body string) TencentErrKind {
	low := strings.ToLower(body)
	if status == http.StatusPaymentRequired {
		return TencentErrHardCredit
	}
	for _, m := range tencentHardCreditMarkers {
		if strings.Contains(low, m) {
			return TencentErrHardCredit
		}
	}
	for _, m := range tencentSessionDeadMarkers {
		if strings.Contains(low, m) {
			return TencentErrSessionDead
		}
	}
	return TencentErrOther
}
