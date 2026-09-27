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
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"omnigate2api/internal/auth"
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
	tr := newTransport()
	return &TencentClient{
		http:       &http.Client{Timeout: timeout, Transport: tr},
		streamHTTP: &http.Client{Transport: tr},
		base:       strings.TrimRight(os.Getenv("OMNIGATE_TENCENT_BASE"), "/"),
	}
}

// ---------------------------------------------------------------------------
// 统一请求路径（SPEC §32：计费/活动/chat/market 四域共用一个执行机制）
// ---------------------------------------------------------------------------

// tencentHTTPOpts 统一请求参数。base 由调用方按域选择（计费域/活动域/chat 域）。
type tencentHTTPOpts struct {
	base      string
	method    string
	path      string
	body      []byte
	desktopUA bool // 埋点上报形态：桌面 UA + X-Request-ID（服务端按 UA 归因任务）
	platform  bool // 活动域客户端平台标识（OMNIGATE_ACTIVITY_PLATFORM，实证用）
}

// tencentDo 统一执行：设头 → Do → 限长读体；返回（原始体, HTTP 状态, 传输错误）。
// 业务码判定留给调用方——HTTP 4xx 也可能是幂等成功（如签到 code=10001）。
func (c *TencentClient) tencentDo(acct *auth.Auth, o tencentHTTPOpts) ([]byte, int, error) {
	var rd io.Reader
	if o.body != nil {
		rd = bytes.NewReader(o.body)
	}
	req, err := http.NewRequest(o.method, o.base+o.path, rd)
	if err != nil {
		return nil, 0, err
	}
	billingHeaders(req, billingCred(acct))
	if o.desktopUA {
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("User-Agent", desktopUserAgent)
		req.Header.Set("X-Request-ID", deriveDeviceID(acct.UserID, "req")+strconv.FormatInt(time.Now().UnixMilli()%1000000, 10))
	}
	if o.platform {
		if plat := os.Getenv("OMNIGATE_ACTIVITY_PLATFORM"); plat != "" {
			req.Header.Set("X-Client-Platform", plat)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// tencentChatUAGlobal chat 出站 UA（global 域）：官方**国际版**桌面端三段式，
// 平台段必须是 `WorkBuddy AI`——送错平台段（`WorkBuddy`）会被上游判 403 code 11140
// "request illegal" 风控（社区三个同目标项目一致实证：linguo headers.go 注释点名、
// ithtelab realm.py 按 realm 切品牌段、hub wb_accounts.py 的 chat_ua 常量；见 HANDOFF §11.11）。
// 版本沿用我们已实证的桌面端值（5.5.6 / 2.137.1），只换平台段——最小改动。
const tencentChatUAGlobal = "WorkBuddy/5.5.6 WorkBuddy AI/5.5.6 CLI/2.137.1"

// tencentChatUA chat 路径生效的出站 UA：OMNIGATE_TENCENT_UA 显式覆盖 > 按区域默认。
// CN 暂维持既有 CLI 形态（`CLI/2.63.2 CodeBuddy/2.63.2`，当前实测可用）；global 用
// 国际版桌面形态。CN 是否也要切桌面形态待观察（上游收紧是渐进的，见 HANDOFF §11.11）。
func tencentChatUA(domain string) string {
	if v := strings.TrimSpace(os.Getenv("OMNIGATE_TENCENT_UA")); v != "" {
		return v
	}
	if TencentRegion(domain) {
		return tencentChatUAGlobal
	}
	return TencentClientUA
}

// desktopUserAgent 桌面客户端 UA（SPEC §32.8：服务端按 UA 归因桌面任务的硬门控）。
const desktopUserAgent = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"

// billingDo 计费域请求（签到/余额）。
func (c *TencentClient) billingDo(acct *auth.Auth, method, path string, body []byte) ([]byte, int, error) {
	return c.tencentDo(acct, tencentHTTPOpts{base: c.billingBaseFor(acct.Domain), method: method, path: path, body: body})
}

// activityDo 活动域请求（宠物/任务/市场）。
func (c *TencentClient) activityDo(acct *auth.Auth, method, path string, body []byte) ([]byte, int, error) {
	return c.tencentDo(acct, tencentHTTPOpts{base: c.activityBaseFor(acct.Domain), method: method, path: path, body: body, platform: true})
}

// chatDo chat 域请求（market 列表/埋点上报；desktop=true 走桌面 UA 形态）。
func (c *TencentClient) chatDo(acct *auth.Auth, method, path string, body []byte, desktop bool) ([]byte, int, error) {
	base, _ := c.resolve(acct.Domain)
	return c.tencentDo(acct, tencentHTTPOpts{base: base, method: method, path: path, body: body, desktopUA: desktop})
}

// TencentRegion 按凭证 domain 后缀判定区域：.workbuddy.ai → global 域，
// 否则 CN 默认（base/Origin/Referer/计费域共用同一判定，单点收敛）。
func TencentRegion(domain string) bool {
	return strings.HasSuffix(strings.TrimSpace(domain), ".workbuddy.ai")
}

// resolve 按凭证 domain 后缀解析 (base, origin)：区域判定见 TencentRegion；
// OMNIGATE_TENCENT_BASE 仅覆盖 base（测试/实验，Origin 仍按域规则）。
func (c *TencentClient) resolve(domain string) (base, origin string) {
	global := TencentRegion(domain)
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
	// X-CodeBuddy-Request 是官方客户端的**风控闸门头**（社区实证：上游 2026-09-14
	// 给自家出站加上的那批头之一，注释原文「所有 API 请求必带」）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language 按区域切（同一批改动 D5）：国际站 en-US、国内 zh-CN。
	req.Header.Set("Accept-Language", tencentAcceptLanguage(origin))
}

// tencentAcceptLanguage 按 Origin 判区域返回 Accept-Language（global → en-US）。
func tencentAcceptLanguage(origin string) string {
	if strings.Contains(origin, "workbuddy.ai") {
		return "en-US"
	}
	return "zh-CN"
}

// tencentChatHeaders chat 请求头：Bearer + 账号标识 + 产品约定；空字段按官方
// CLI 约定发 X-No-*: 1 占位（保持请求形态完整，SPEC §28.4 决策 C）。
func tencentChatHeaders(req *http.Request, cred SignCredential, origin string) {
	tencentCommonHeaders(req, origin)
	// chat 出站 UA 按区域切（global 必须国际版平台段，否则 403/11140）
	req.Header.Set("User-Agent", tencentChatUA(cred.Domain))
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
	// 账号级设备指纹头（社区实证：上游 2026-09-13 起要求设备风控标识；值按 uid
	// 稳定派生——随机机器码反而会被判异常，见 HANDOFF §11.11）。
	if cred.UserID != "" {
		req.Header.Set("X-Machine-ID", deriveDeviceID(cred.UserID, "machine"))
		req.Header.Set("X-Session-ID", deriveDeviceID(cred.UserID, "session"))
	}
}

// ChatStream 发送 /v2/chat/completions：roles 消息保真透传（tools 原样拼入 body），
// 模型名不做映射（上游按真实 ID 匹配，参考实现实证）；toolChoice 已按上游
// string 语义归一化（§28.4 决策 D：none 在上层已删 tools，此处仅拼非空值）。
func (c *TencentClient) ChatStream(ctx context.Context, chatID string, messages []ChatMessage, traceID string, cred SignCredential, userName string, model string, tools []map[string]any, toolChoice string, gen map[string]any) (io.ReadCloser, error) {
	// 全球域要求首条为 system prompt（区域契约差异，见 tencent_realm.go）：
	// 缺失则上游 400 + code=11128，且会把账号连续错误推入冷却。
	if TencentRegion(cred.Domain) {
		messages = ensureLeadingSystem(messages)
	}
	// gen 先铺底（客户端生成参数：temperature/top_p/stop/seed/max_tokens/…），
	// 随后由权威字段覆盖（§23.1）——上游实测全部接受这些参数。
	body := map[string]any{}
	applyGen(body, gen)
	body["model"] = model
	body["stream"] = true
	body["messages"] = messages // ChatMessage.MarshalJSON 双形态（§30.5：分片数组透传）
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
		// 上游响应头里有对账标识（Traceid / X-Request-Id / EO-LOG-UUID / EO-Cache-Status）：
		// 失败时记下来，复盘时能直接拿去找上游；成功路径不记（避免每请求一条日志噪音）。
		log.Printf("tencent chat failed status=%d trace=%s edge=%s req=%s",
			resp.StatusCode, resp.Header.Get("Traceid"), resp.Header.Get("EO-Cache-Status"),
			resp.Header.Get("X-Request-Id"))
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
	TencentErrOther          TencentErrKind = iota
	TencentErrHardCredit                    // 402 / 积分不足 / 14018 额度已用尽：冷却至次日 04:00
	TencentErrSessionDead                   // 12153 / Offline user session not found：永久禁用
	TencentErrModelRateLimit                // 429 + code 6004：**按模型**限流，解封时刻由上游给出
	// TencentErrAccountBanned 上游授权封禁（`11140` + msg `request illegal`）：账号级、
	// 换号无用、**不会自愈**——必须禁用并提示重新登录（见 tencentAccountBannedMarkers）。
	TencentErrAccountBanned
	// TencentErrSoftRate 限流文案（含 `11140` 的 rate-limiting 变体、无模型级证据的裸 429）：
	// 带「将在…重置」就冷却到该时刻，否则按配置的软冷却。
	TencentErrSoftRate
	// TencentErrClientSide 请求侧拒绝（内容策略/参数类）：不罚账号、原样透传
	// （社区拍板：单次违规请求不该毒化整个号池）。
	TencentErrClientSide
)

// tencentAccountBannedMarkers 上游授权封禁文案。
//
// **只能按文案判、不能按 code 判**：同一个 `11140` 还承载限流文案
// （见 tencentSoftRate11140Markers），按 code 会把两种语义混为一谈。
//
// 实测（2026-09-27，本机三个全球号）：`403 {"code":11140,"msg":"request illegal",
// "displayMsg":{"zh":"内容未通过安全审核，请调整后重试"}}` —— displayMsg 只是通用文案，
// 真语义是"请求非法"：同一条无害内容换到国内号能过、全局号全拒，且**任何模型都拒**
// （而签到/任务/埋点等其它端点正常）→ 账号级而非内容级。
// 社区两个同目标项目一致：`request illegal` 判账号级授权封禁，且**到期不自愈、
// 必须重新授权登录**（ithtelab CHANGELOG：「上游对 11140 request illegal 是硬禁用」）。
var tencentAccountBannedMarkers = []string{"request illegal"}

// tencentSoftRate11140Markers `11140` 的**限流**变体文案（社区实测形态，勿与封禁混判）：
//
//	{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}
var tencentSoftRate11140Markers = []string{"rate-limiting requests", "rate limiting requests"}

// tencentClientSideMarkers 请求侧拒绝文案（内容策略误杀/非法调用形态）：不罚账号。
// 取自社区同目标项目的实测表（内容审核误报、非官方渠道、非法调用）。
var tencentClientSideMarkers = []string{
	"blocked by security policy", "unapproved channel", "illegal api invocation",
	"unmarshal chat params failed", "prompt is too long",
}

// tencentHardCreditMarkers 额度耗尽标记（中英双通道，对齐参考实现）。
var tencentHardCreditMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	// 实测文案：14018「额度已用尽」/「Credits exhausted」——注意「额度已用尽」并不含
	// 子串「额度用尽」（中间隔着「已」），旧表因此漏判，必须逐条列出。
	"额度已用尽", "credits exhausted", "quota exhausted", "usage quota exceeded",
}

// tencentSessionDeadMarkers 会话死亡标记（离线会话/账号失效）。
var tencentSessionDeadMarkers = []string{"12153", "offline user session not found"}

// ClassifyTencent 按状态码 + 响应体判定腾讯侧错误语义（仅硬额度/会话死亡两类
// 需要专属动作；429/5xx 由通用映射继续处理）。
func ClassifyTencent(status int, body string) TencentErrKind {
	low := strings.ToLower(body)
	// 账号级授权封禁最先判（`11140 request illegal`）：换号无用、且**不会自愈**，
	// 必须先于一切"限流/额度"分支——否则会被当成可自愈的限流反复送死
	// （2026-09-27 事故：每 10 分钟一轮假冷却，客户端 12s 重试一轮，号池形同虚设）。
	for _, m := range tencentAccountBannedMarkers {
		if strings.Contains(low, m) {
			return TencentErrAccountBanned
		}
	}
	// 模型级限流优先于通用 429：上游明确「您也可以切换其他模型继续使用」，
	// 按整账号冷却会白白减少可用容量（见 tencent_limits.go）。
	if IsModelRateLimit(body) {
		return TencentErrModelRateLimit
	}
	// 11140 的限流变体（rate-limiting 文案）与裸限流文案：按软冷却处理
	for _, m := range tencentSoftRate11140Markers {
		if strings.Contains(low, m) {
			return TencentErrSoftRate
		}
	}
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
	// 请求侧拒绝（内容策略/参数类）：不罚账号（放在最后，避免抢走上面更具体的语义）
	for _, m := range tencentClientSideMarkers {
		if strings.Contains(low, m) {
			return TencentErrClientSide
		}
	}
	return TencentErrOther
}
