// CodeArts Agent 云端客户端。
//
// 纯 HTTP：
//   - 登录：华为云 CodeArts OAuth2（PKCE + 本地回调）→ snap-manager /v1/oauth2/tokens
//     换 STS 临时 AK/SK + security_token；ticket 轮询为兜底通道
//   - 聊天：POST snap-access/v1/chat/chat，header x-auth-token=security_token，SSE 流式
//   - 刷新：POST /v1/oauth2/tokens grant_type=refresh_token
package upstream

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"omnigate2api/internal/auth"
)

// ApiError 带业务 code 的上游错误。
type ApiError struct {
	Code    int
	Status  int
	Message string
	Path    string
}

func (e *ApiError) Error() string {
	return fmt.Sprintf("codearts api code=%d http=%d path=%s msg=%s", e.Code, e.Status, e.Path, e.Message)
}

// Credentials 登录返回的 STS 临时凭证。
type Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	Expiration      string `json:"expiration"`
}

// LegacyCredential ticket 通道（旧登录）返回的凭证字段。
type LegacyCredential struct {
	Access        string `json:"access"`
	Secret        string `json:"secret"`
	SecurityToken string `json:"securitytoken"`
	ExpiresAt     string `json:"expires_at"`
}

// TokenResponse oauth2/tokens 响应（含用户信息与凭证）。
type TokenResponse struct {
	UserID       string           `json:"user_id"`
	UserName     string           `json:"user_name"`
	DomainID     string           `json:"domain_id"`
	RefreshToken string           `json:"refresh_token"`
	Credentials  Credentials      `json:"credentials"`
	Credential   LegacyCredential `json:"credential"`
}

// LoginConfig 登录相关配置。
type LoginConfig struct {
	ClientID      string
	PortalHost    string
	SnapManager   string
	STSHost       string
	RedirectPath  string // 本地回调路径
	PluginName    string
	PluginVersion string
}

// DefaultLoginConfig 生产默认值（逆向自 huaweicloud.authentication 扩展）。
func DefaultLoginConfig() LoginConfig {
	return LoginConfig{
		ClientID:      CLIENT_ID,
		PortalHost:    PortalHost,
		SnapManager:   SnapManagerHost,
		STSHost:       STSHost,
		RedirectPath:  "/oauth/callback",
		PluginName:    "snap_AIIDE",
		PluginVersion: "5.1.0",
	}
}

// Client CodeArts 云 API 客户端。
type Client struct {
	http       *http.Client // 短请求，带总超时
	streamHTTP *http.Client // SSE 长流，无总超时

	baseHost string // 引擎 host（OMNIGATE_UPSTREAM_BASE 可覆盖，测试/实验用）
}

// New 构造客户端。
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := newTransport()
	base := SnapEngineApiHost
	if v := os.Getenv("OMNIGATE_UPSTREAM_BASE"); v != "" {
		base = strings.TrimRight(v, "/")
	}
	return &Client{
		http:       &http.Client{Timeout: timeout, Transport: tr},
		streamHTTP: &http.Client{Transport: tr},
		baseHost:   base,
	}
}

// ---------------------------------------------------------------------------
// 认证
// ---------------------------------------------------------------------------

// BuildAuthorizeURL 构造 portal 登录链接（PKCE）。
// ticketID 客户端生成的随机 hex；port 为本地回调端口。
func (c *Client) BuildAuthorizeURL(cfg LoginConfig, ticketID, codeChallenge, codeChallengeMethod string, port int) string {
	q := url.Values{}
	q.Set("theme", "dark")
	q.Set("locale", "zh-cn")
	q.Set("uri_scheme", cfg.ClientID)
	q.Set("client_id", cfg.ClientID)
	q.Set("port", fmt.Sprint(port))
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", codeChallengeMethod)
	q.Set("ticket_id", ticketID)
	q.Set("plugin-name", cfg.PluginName)
	q.Set("plugin-version", cfg.PluginVersion)
	return cfg.PortalHost + "/authorize?" + q.Encode()
}

// ExchangeCode 用授权码换 token（OAuth2 authorization_code）。
func (c *Client) ExchangeCode(ctx context.Context, cfg LoginConfig, code, codeVerifier string, port int) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", fmt.Sprintf("http://127.0.0.1:%d%s", port, cfg.RedirectPath))
	return c.requestToken(ctx, cfg, form)
}

// RefreshToken 用 refresh_token 换新凭证。
func (c *Client) RefreshToken(ctx context.Context, cfg LoginConfig, refreshToken, codeVerifier, domain string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", cfg.ClientID)
	form.Set("code_verifier", codeVerifier)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	return c.requestToken(ctx, cfg, form)
}

// requestToken 向 STS 令牌端点发 form + DPoP 请求。
func (c *Client) requestToken(ctx context.Context, cfg LoginConfig, form url.Values) (*TokenResponse, error) {
	url := cfg.STSHost + EpOAuthTokens
	kp, err := newDpopKeyPair()
	if err != nil {
		return nil, fmt.Errorf("dpop keypair: %w", err)
	}
	proof, err := signDpopProof(kp, url)
	if err != nil {
		return nil, fmt.Errorf("dpop proof: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpOAuthTokens}
	}
	var out TokenResponse
	if os.Getenv("OMNIGATE_LOGIN_DEBUG") != "" {
		var probe map[string]any
		if err := json.Unmarshal(raw, &probe); err == nil {
			keys := make([]string, 0, len(probe))
			for k := range probe {
				keys = append(keys, k)
			}
			log.Printf("login token response keys: %v has_refresh=%v", keys, probe["refresh_token"] != nil || probe["refreshToken"] != nil)
		}
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse token response: %w body=%s", err, truncateStr(string(raw), 200))
	}
	if out.Credentials.SecurityToken == "" {
		// 兼容 ticket/旧通道字段（legacy）
		if out.Credential.SecurityToken != "" {
			out.Credentials = Credentials{
				AccessKeyID:     out.Credential.Access,
				SecretAccessKey: out.Credential.Secret,
				SecurityToken:   out.Credential.SecurityToken,
				Expiration:      out.Credential.ExpiresAt,
			}
			return &out, nil
		}
		return nil, fmt.Errorf("token response missing credentials: %s", truncateStr(string(raw), 200))
	}
	return &out, nil
}

// PollTicket 轮询登录结果（兜底通道）。
func (c *Client) PollTicket(ctx context.Context, cfg LoginConfig, ticketID, secret string) (*TokenResponse, error) {
	path := EpLoginTicket + "?ticket_id=" + url.QueryEscape(ticketID) + "&secret=" + url.QueryEscape(secret)
	headers := map[string]string{
		"plugin-name":    cfg.PluginName,
		"plugin-version": cfg.PluginVersion,
	}
	var out TokenResponse
	if err := c.doJSON(ctx, http.MethodGet, cfg.SnapManager, path, headers, nil, &out); err != nil {
		return nil, err
	}
	// 登录排查（OMNIGATE_LOGIN_DEBUG）：ticket 通道是否携带 refresh_token
	if os.Getenv("OMNIGATE_LOGIN_DEBUG") != "" {
		log.Printf("login ticket response: has_refresh=%v refresh_len=%d user=%s", out.RefreshToken != "", len(out.RefreshToken), out.UserID)
	}
	// 旧通道凭证归一化
	if out.Credentials.SecurityToken == "" && out.Credential.SecurityToken != "" {
		out.Credentials = Credentials{
			AccessKeyID:     out.Credential.Access,
			SecretAccessKey: out.Credential.Secret,
			SecurityToken:   out.Credential.SecurityToken,
			Expiration:      out.Credential.ExpiresAt,
		}
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// 聊天
// ---------------------------------------------------------------------------

// ChatMessage 统一线格式消息（SPEC §22.1）：text-only 折叠与 roles 透传共用。
// text-only 时 Role="user"、Content=折叠文本；roles 时逐条保留
// system/user/assistant(tool_calls)/tool(tool_call_id) 语义。
// ContentParts 非空（多模态，SPEC §30.5）→ MarshalJSON 输出 content 分片数组
// （文本合并为首片）；否则与历史线格式字节级一致（string content）。
type ChatMessage struct {
	Role         string            `json:"role"`
	Content      string            `json:"content"`
	ContentParts []ChatContentPart `json:"-"`
	ToolCalls    []ChatToolCall    `json:"tool_calls,omitempty"`
	ToolCallID   string            `json:"tool_call_id,omitempty"`
}

// ChatContentPart content 分片（roles 多模态透传，SPEC §30.5）。
type ChatContentPart struct {
	Type     string        `json:"type"` // "text" | "image_url"
	Text     string        `json:"text,omitempty"`
	ImageURL *ChatImageURL `json:"image_url,omitempty"`
}

// ChatImageURL OpenAI 图片分片负载。
type ChatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// MarshalJSON 双形态线格式（唯一序列化点，SPEC §30.5）：无分片 → string content
// （零回归）；有分片 → 数组（文本合首片）。tool_calls 保持 OpenAI 嵌套线格式
// （type/function 包裹，与原手工序列化层等价）；role 空值兜底 user。
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	role := m.Role
	if role == "" {
		role = "user"
	}
	var calls []map[string]any
	if len(m.ToolCalls) > 0 {
		calls = make([]map[string]any, 0, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			calls = append(calls, map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": c.Arguments},
			})
		}
	}
	if len(m.ContentParts) == 0 {
		return json.Marshal(struct {
			Role       string           `json:"role"`
			Content    string           `json:"content"`
			ToolCalls  []map[string]any `json:"tool_calls,omitempty"`
			ToolCallID string           `json:"tool_call_id,omitempty"`
		}{role, m.Content, calls, m.ToolCallID})
	}
	parts := make([]ChatContentPart, 0, len(m.ContentParts)+1)
	if m.Content != "" {
		parts = append(parts, ChatContentPart{Type: "text", Text: m.Content})
	}
	parts = append(parts, m.ContentParts...)
	return json.Marshal(struct {
		Role       string            `json:"role"`
		Content    []ChatContentPart `json:"content"`
		ToolCalls  []map[string]any  `json:"tool_calls,omitempty"`
		ToolCallID string            `json:"tool_call_id,omitempty"`
	}{role, parts, calls, m.ToolCallID})
}

// ChatToolCall 完整工具调用（OpenAI 线格式，arguments 为 JSON 字符串）。
type ChatToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// CanonicalModel 把用户友好模型 ID 映射为 InferHub 注册的模型 ID（区分大小写）。
// 旧版 /v1/chat/chat 用小写 id（glm-5.2 / snap-chat），新 /api/v2/chat/completions
// 按 InferHub 注册名匹配（GLM-5.2 / deepseek-v4-flash / Qwen3-VL-235B）。
func CanonicalModel(id string) string {
	switch id {
	case "snap-chat", "glm-5.2":
		return "GLM-5.2"
	case "glm-5.1":
		return "GLM-5.1"
	case "glm-4.7":
		return "GLM-4.7"
	case "qwen3-vl-235b":
		return "Qwen3-VL-235B"
	case "qwen3.5-397b-a17b-vl":
		return "Qwen3.5-397B-A17B-VL"
	case "qwen3.6-27b-vl":
		return "Qwen3.6-27B-VL"
	default:
		return id
	}
}

// benefitModels 活动（福利）模型集合：ID 来自 MaaS 福利网关
// （GET opengw.developer.huaweicloud.com/api/v1/gateway/config 的 result.models），
// 客户端侧以 isFreeBenefit 标记并在聊天请求加 maas_type: benefit 头。
var benefitModels = map[string]struct{}{
	"glm-5.3-flash":          {},
	"deepseek-v4-flash-0731": {},
	"deepseek-v4-pro-0813":   {},
}

// IsBenefitModel 判断是否为活动（福利）模型。
func IsBenefitModel(model string) bool {
	_, ok := benefitModels[model]
	return ok
}

// ---------------------------------------------------------------------------
// 福利网关（opengw.developer.huaweicloud.com）：活动模型领取/配置/余额。
// 官方客户端登录时自动 POST benefit/claim 领取当日额度（北京时间 24 点重置）；
// 该调用为 AK/SK 签名 + X-Security-Token 的空 body POST，代理可完全复刻，
// 从而脱离官方客户端自动领取。
// ---------------------------------------------------------------------------

const BenefitGatewayHost = "https://opengw.developer.huaweicloud.com"

const (
	EpBenefitClaim  = "/api/v1/benefit/claim"
	EpGatewayConfig = "/api/v1/gateway/config"
	EpTokensBalance = "/api/v1/user/tokens/balance"
)

// BenefitRecord 领取记录。
type BenefitRecord struct {
	Channel    string `json:"channel"`
	UserID     string `json:"user_id"`
	CreateTime int64  `json:"create_time"`
	UpdateTime int64  `json:"update_time"`
}

// GatewayBenefitModel 福利网关下发的活动模型。
type GatewayBenefitModel struct {
	ModelID       string `json:"model_id"`
	ModelName     string `json:"model_name"`
	ContextWindow int64  `json:"context_window"`
	MaxTokens     int64  `json:"max_tokens"`
}

// GatewayConfig 福利网关配置。
type GatewayConfig struct {
	BaseURL string                `json:"base_url"`
	Models  []GatewayBenefitModel `json:"models"`
}

// TokensBalance 福利额度使用情况。
type TokensBalance struct {
	TotalQuota   int64 `json:"total_quota"`
	TotalBalance int64 `json:"total_balance"`
	UsedAmount   int64 `json:"used_amount"`
	ExpireTime   int64 `json:"expire_time"`
}

// gatewayEnvelope opengw 响应包裹层：{error_code, error_msg, result}。
type gatewayEnvelope struct {
	ErrorCode   string          `json:"error_code"`
	ErrorMsg    string          `json:"error_msg"`
	Status_code int             `json:"statusCode"`
	Result      json.RawMessage `json:"result"`
}

// postSigned 发送 AK/SK 签名的空 body POST（opengw 福利接口）。
func (c *Client) postSigned(ctx context.Context, urlStr string, cred SignCredential) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader([]byte("")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: req.URL.Path}
	}
	return raw, nil
}

// parseGatewayEnvelope 校验 error_code 并解出 result。
func parseGatewayEnvelope(raw []byte) (json.RawMessage, error) {
	var env gatewayEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse envelope: %w body=%s", err, truncateStr(string(raw), 200))
	}
	if env.ErrorCode != "0000" {
		return nil, fmt.Errorf("gateway error_code=%s msg=%s", env.ErrorCode, env.ErrorMsg)
	}
	return env.Result, nil
}

// ClaimBenefit 领取（激活）当日福利额度。幂等：已领取时返回既有记录。
func (c *Client) ClaimBenefit(ctx context.Context, acct *auth.Auth) (*BenefitRecord, error) {
	cred := SignCredential{AccessKeyID: acct.AccessKeyID, SecretAccessKey: acct.SecretAccessKey, SecurityToken: acct.CloudDragonTok}
	raw, err := c.postSigned(ctx, BenefitGatewayHost+EpBenefitClaim, cred)
	if err != nil {
		return nil, err
	}
	result, err := parseGatewayEnvelope(raw)
	if err != nil {
		return nil, err
	}
	var rec BenefitRecord
	if err := json.Unmarshal(result, &rec); err != nil {
		return nil, fmt.Errorf("parse claim result: %w body=%s", err, truncateStr(string(result), 200))
	}
	return &rec, nil
}

// FetchGatewayConfig 拉取福利网关配置（base_url + 活动模型清单）。
func (c *Client) FetchGatewayConfig(ctx context.Context, acct *auth.Auth) (*GatewayConfig, error) {
	cred := SignCredential{AccessKeyID: acct.AccessKeyID, SecretAccessKey: acct.SecretAccessKey, SecurityToken: acct.CloudDragonTok}
	raw, err := c.getSigned(ctx, BenefitGatewayHost+EpGatewayConfig, cred, false)
	if err != nil {
		return nil, err
	}
	result, err := parseGatewayEnvelope(raw)
	if err != nil {
		return nil, err
	}
	var cfg GatewayConfig
	if err := json.Unmarshal(result, &cfg); err != nil {
		return nil, fmt.Errorf("parse gateway config: %w body=%s", err, truncateStr(string(result), 200))
	}
	return &cfg, nil
}

// FetchTokensBalance 查询福利额度余额。
func (c *Client) FetchTokensBalance(ctx context.Context, acct *auth.Auth) (*TokensBalance, error) {
	cred := SignCredential{AccessKeyID: acct.AccessKeyID, SecretAccessKey: acct.SecretAccessKey, SecurityToken: acct.CloudDragonTok}
	raw, err := c.getSigned(ctx, BenefitGatewayHost+EpTokensBalance, cred, false)
	if err != nil {
		return nil, err
	}
	result, err := parseGatewayEnvelope(raw)
	if err != nil {
		return nil, err
	}
	var bal TokensBalance
	if err := json.Unmarshal(result, &bal); err != nil {
		return nil, fmt.Errorf("parse balance: %w body=%s", err, truncateStr(string(result), 200))
	}
	return &bal, nil
}

// ChatHeadersV2 组装 /api/v2/chat/completions 请求头。
// 与官方 AgentKernel 一致：x-auth-token 鉴权 + AK/SK 签名，无 Agent-Type。
func ChatHeadersV2(token, traceID, language string) map[string]string {
	if traceID == "" {
		traceID = fmt.Sprintf("%x", time.Now().UnixNano())
	}
	if language == "" {
		language = "zh-cn"
	}
	return map[string]string{
		"Content-Type":    "application/json",
		"Accept":          "text/event-stream",
		"x-auth-token":    token,
		"x-snap-traceid":  traceID,
		"X-Language":      language,
		"app-id":          "CodeAgent3.0",
		"is_confidential": "false",
	}
}

// ChatStream 发送 /api/v2/chat/completions（OpenAI 兼容，AK/SK 签名 + x-auth-token）
// 并返回 SSE 流（调用方负责 Close）。
// ChatStream 发送聊天请求：华为签名风格；tools 为本机透传的工具定义
// （华为 text-only 模拟层不使用，roles 上游直接拼入 body）；toolChoice
// 为腾讯 string 语义归一化结果，华为路径忽略。
func (c *Client) ChatStream(ctx context.Context, chatID string, messages []ChatMessage, traceID string, cred SignCredential, userName string, model string, tools []map[string]any, toolChoice string) (io.ReadCloser, error) {
	// messages 直接序列化（ChatMessage.MarshalJSON 双形态：string/分片数组，§30.5）
	body := map[string]any{
		"model":    CanonicalModel(model),
		"stream":   true,
		"messages": messages,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	return c.SendChatV2(ctx, body, traceID, cred, cred.SecurityToken)
}

// SendChatV2 发送自定义 OpenAI 兼容 body 到 /api/v2/chat/completions。
// 鉴权：x-auth-token（STS security token）+ 华为云 SDK-HMAC-SHA256 AK/SK 签名。
func (c *Client) SendChatV2(ctx context.Context, body map[string]any, traceID string, cred SignCredential, userToken string) (io.ReadCloser, error) {
	raw, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseHost+EpChatV2, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range ChatHeadersV2(userToken, traceID, "zh-cn") {
		httpReq.Header.Set(k, v)
	}
	// 活动（福利）模型走同一端点同一鉴权，仅多一个 maas_type 头——后端据此
	// 路由到 MaaS 福利网关；缺失则报 InferHub.002002009.404 model not registered。
	if IsBenefitModel(fmt.Sprint(body["model"])) {
		httpReq.Header.Set("maas_type", "benefit")
	}
	signRequest(httpReq, raw, cred)
	resp, err := c.streamHTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(rawBody), 200), Path: EpChatV2}
	}
	return resp.Body, nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

func (c *Client) doJSON(ctx context.Context, method, baseURL, path string, headers map[string]string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: path}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("parse %s: %w body=%s", path, err, truncateStr(string(raw), 200))
		}
	}
	return nil
}

// PKCE 生成 code_verifier / code_challenge（S256）。
func PKCE() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err := crand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// RandomHex 生成 n 字节的 hex 随机串。
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ModelInfo 模型目录条目（SPEC §29.7）：华为/腾讯共用。元数据仅腾讯目录下发，
// 华为侧（含静态表）留零值，由调用方按 AccessFor 补访问类别。
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
	MaxTokens     int64  `json:"maxTokens,omitempty"`
	// 以下为腾讯目录元数据（/console/enterprises/personal/models）。
	Vendor         string   `json:"vendor,omitempty"`
	Modes          []string `json:"modes,omitempty"`  // 非 badge 标签（如 craft）
	Access         string   `json:"access,omitempty"` // 访问类别（见 access* 常量）
	AccessLabel    string   `json:"accessLabel,omitempty"`
	SupportsImages bool     `json:"supportsImages,omitempty"`
	SupportsTools  bool     `json:"supportsTools,omitempty"`
	IsDefault      bool     `json:"isDefault,omitempty"`
	Description    string   `json:"description,omitempty"`
}

// FetchModels 从 agent-center 拉取当前账号可用的模型列表（动态缓存 1h）。
// 链路：useragents 找默认 CodeAgent → detail 的 gpts.models 返回精确模型 ID。
func (c *Client) FetchModels(acct *auth.Auth) ([]ModelInfo, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for model fetch")
	}
	cred := SignCredential{
		AccessKeyID:     acct.AccessKeyID,
		SecretAccessKey: acct.SecretAccessKey,
		SecurityToken:   acct.CloudDragonTok,
	}
	agentID, err := c.defaultAgentID(cred)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, fmt.Errorf("no default agent found")
	}
	raw, err := c.getSigned(context.Background(), c.baseHost+EpAgentDetail+"?agent_id="+url.QueryEscape(agentID), cred, true)
	if err != nil {
		return nil, err
	}
	var detail struct {
		Gpts struct {
			Models []struct {
				ModelAlias string `json:"model_alias"`
				ModelName  string `json:"model_name"`
				Params     struct {
					ContextWindow int64 `json:"context_window"`
					MaxTokens     int64 `json:"max_tokens"`
				} `json:"model_parameters"`
			} `json:"models"`
		} `json:"gpts"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	if len(detail.Gpts.Models) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	out := make([]ModelInfo, 0, len(detail.Gpts.Models))
	for _, m := range detail.Gpts.Models {
		id := firstNonEmpty(m.ModelName, m.ModelAlias)
		if id == "" {
			continue
		}
		// 华为目录不下发付费/免费标注：活动（福利）模型由福利网关清单单独判定。
		access, accessLabel := AccessForStatic(id)
		out = append(out, ModelInfo{
			ID:            id,
			Name:          id,
			ContextWindow: m.Params.ContextWindow,
			MaxTokens:     m.Params.MaxTokens,
			Access:        access,
			AccessLabel:   accessLabel,
		})
	}
	return out, nil
}

// DebugGetSignedRaw 调试用：按账号凭证发起签名 GET，返回原始响应体。
// 用于排查 agent/model 目录（如活动模型注册名）。
func (c *Client) DebugGetSignedRaw(ctx context.Context, urlStr string, acct *auth.Auth, agentCenter bool) ([]byte, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required")
	}
	cred := SignCredential{
		AccessKeyID:     acct.AccessKeyID,
		SecretAccessKey: acct.SecretAccessKey,
		SecurityToken:   acct.CloudDragonTok,
	}
	return c.getSigned(ctx, urlStr, cred, agentCenter)
}

// defaultAgentID 拉取用户 agent 列表，返回默认 CodeAgent 的 agent_id。
func (c *Client) defaultAgentID(cred SignCredential) (string, error) {
	raw, err := c.getSigned(context.Background(), c.baseHost+EpAgentList+"?offset=0&limit=100", cred, true)
	if err != nil {
		return "", err
	}
	var out struct {
		Agents []struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
			Primary   bool   `json:"is_primary_agent"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse agents: %w", err)
	}
	for _, a := range out.Agents {
		if a.Primary {
			return a.AgentID, nil
		}
	}
	if len(out.Agents) > 0 {
		return out.Agents[0].AgentID, nil
	}
	return "", nil
}

// getSigned 发送带 Agent-Type 的 AK/SK 签名 GET（用于 agent-center 等管理接口）。
func (c *Client) getSigned(ctx context.Context, urlStr string, cred SignCredential, agentCenter bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if agentCenter {
		req.Header.Set("Agent-Type", "AgentCenter")
	}
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: req.URL.Path}
	}
	return raw, nil
}
