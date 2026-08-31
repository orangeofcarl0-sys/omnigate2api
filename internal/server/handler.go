// Package server 暴露 OpenAI 兼容接口：/v1/chat/completions、/v1/models、/status、WebUI。
package server

import (
	crand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// profiles 请求所属 Profile 注册表：nil 时使用内置 codearts。
func (h *Handler) profiles() *adapt.Registry {
	if h.cfg.Profiles != nil {
		return h.cfg.Profiles
	}
	return adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)
}

// indexFor 请求所属 Profile 的会话指纹表（惰性创建，隔离于其它上游）。
func (h *Handler) indexFor(p *adapt.UpstreamProfile) *sessionIndex {
	h.indexMu.Lock()
	defer h.indexMu.Unlock()
	idx, ok := h.indexes[p.ID]
	if !ok {
		idx = newSessionIndex(p.ReanchorEvery())
		h.indexes[p.ID] = idx
	}
	return idx
}

// breakerFor 请求所属 Profile 的熔断器（惰性创建，隔离于其它上游）。
func (h *Handler) breakerFor(p *adapt.UpstreamProfile) *adapt.CircuitBreaker {
	h.breakMu.Lock()
	defer h.breakMu.Unlock()
	cb, ok := h.breakers[p.ID]
	if !ok {
		cb = adapt.NewCircuitBreaker(0, 0, 0)
		h.breakers[p.ID] = cb
	}
	return cb
}

// routeResult 会话路由结果（C4 结构化，避免 7 元返回）。
type routeResult struct {
	Continue   bool
	Explicit   bool
	TailMsgs   []openAIMessage
	ChatID     string
	StickyAcct string
	RouteKey   string // 本回合最终消息数组哈希（供注册）
	MatchedKey string // 命中条目的键（供失败清理）
}

// routeSession 决定续接模式：显式 conversation_id 优先；其次按
// Profile+override 决定 incremental（指纹续接），受熔断器约束。
// forceNative：toolchain.project 有损 ⇒ 本回合强制全量折叠（§14.1 互斥）。
func (h *Handler) routeSession(profile *adapt.UpstreamProfile, req *chatRequest, forceNative bool) routeResult {
	if req.ConversationID != "" && validChatID(req.ConversationID) {
		h.convMu.Lock()
		defer h.convMu.Unlock()
		return routeResult{Continue: true, Explicit: true, StickyAcct: h.convAcct[req.ConversationID]}
	}
	if forceNative {
		return routeResult{}
	}
	breaker := h.breakerFor(profile)
	now := time.Now()
	if breaker.Open(now) {
		log.Printf("circuit open profile=%s: force native (last=%s)", profile.ID, breaker.LastEvent())
		return routeResult{}
	}
	// incremental 是显式 opt-in：override=incremental 且 Profile 声明 implicit。
	effIncr := h.cfg.SessionMode == "incremental" && profile.Session.Kind == "implicit"
	if effIncr {
		idx := h.indexFor(profile)
		chain := fingerprint(req.Messages)
		routeKey := chain[len(chain)-1]
		if ref, k, key, reason, ok := idx.match(req.Messages, chain); ok {
			if h.cfg.Pool.Healthy(ref.account) {
				return routeResult{
					Continue: true, TailMsgs: req.Messages[tailAnchorIndex(req.Messages, k):],
					ChatID: ref.chatID, StickyAcct: ref.account, RouteKey: routeKey, MatchedKey: key,
				}
			}
			h.recordGuard(profile, adapt.GuardMiss, now)
			idx.drop(key)
		} else {
			ev := adapt.GuardMiss
			switch reason {
			case MatchAmbiguous, MatchContended:
				ev = adapt.GuardCross
			}
			h.recordGuard(profile, ev, now)
		}
		// 匹配失败/不健康：回退全量折叠，但仍登记本回合最终哈希供下一轮
		// 前缀命中（SPEC §5.4：全量与增量回合均注册最新哈希）。
		return routeResult{RouteKey: routeKey}
	}
	return routeResult{}
}

// recordGuard 记录守卫事件；触发熔断阈值时告警（SPEC §5.3）。
func (h *Handler) recordGuard(profile *adapt.UpstreamProfile, ev adapt.GuardEvent, now time.Time) {
	if h.breakerFor(profile).Record(ev, now) {
		log.Printf("FUNDAMENTAL_GUARD_BREACH profile=%s event=%s threshold=%d", profile.ID, ev, adapt.CircuitThreshold)
	}
}

// turnSuccess 回合成功收尾：结算账号健康 + 登记指纹（供下一轮匹配）。
func (h *Handler) turnSuccess(profile *adapt.UpstreamProfile, acct *pool.Account, chatID, routeKey string, msgsLen int) {
	h.cfg.Pool.NoteSuccess(acct.Name)
	h.registerSession(profile, routeKey, msgsLen, acct.Name, chatID)
}

// turnFailure 回合失败收尾：清除指纹（上游会话状态不可信，回退全量）。
func (h *Handler) turnFailure(profile *adapt.UpstreamProfile, matchedKey string) {
	h.indexFor(profile).drop(matchedKey)
}

// registerSession 成功回合后登记指纹（全量与续接均登记最新状态，供下一轮匹配）。
func (h *Handler) registerSession(profile *adapt.UpstreamProfile, routeKey string, msgsLen int, acct, chatID string) {
	if routeKey == "" {
		return
	}
	h.indexFor(profile).register(routeKey, msgsLen, acct, chatID)
}

// Config handler 依赖。
type Config struct {
	Pool          *pool.Pool
	Upstream      *upstream.Client
	APIKey        string
	MaxRotate     int
	SoftCooldown  time.Duration
	ErrThreshold  int
	ErrCooldown   time.Duration
	DefaultModel  string
	ConvStateFile string
	WatchInfo     map[string]any
	AuthDir       string
	Listen        string
	// OAuthCallbackHost 可选：覆盖授权链接回调 host（如 https://oneapi.example.com/codearts）。
	OAuthCallbackHost string
	// DebugPromptDir 折叠提示词落盘目录（OMNIGATE_DEBUG_PROMPTS），空为关闭。
	DebugPromptDir string
	// SessionMode 会话模式："native"= 原生等价(默认,全量折叠、零上游记忆
	// 依赖、信息可验证);"incremental"= 增量加速(依赖上游会话记忆,opt-in,
	// 守卫:重锚定/串线锁/指令锚点)。
	SessionMode string
	// ToolchainOverride 工具层覆盖（OMNIGATE_TOOLCHAIN）：none|project|sanitize|
	// project,sanitize；空 = 按 Profile.toolchain 声明。
	ToolchainOverride string
	// Profiles 上游注册表（SPEC §4.4）；nil 时使用内置 codearts。
	Profiles *adapt.Registry
	// Routes 裸模型名路由表（SPEC §29，nil → 内置默认表）；RoutesFile 供 WebUI 保存。
	Routes     *adapt.RouteTable
	RoutesFile string
}

const maxBodyBytes = 8 << 20

// Handler 主路由。
type Handler struct {
	cfg   Config
	mux   *http.ServeMux
	oauth *oauthStore

	convMu sync.Mutex
	chats  map[string]string // account → 最近 chat_id
	// 腾讯设备流进行中状态（面板 OAuth 轮询用，state → 过期时间）
	tencentMu     sync.Mutex
	tencentStates map[string]tencentState
	// 黏性路由：conversation_id → account_name（多轮续接锁定同一账号，减少上游并发会话占用）。
	convAcct map[string]string

	// 会话指纹索引（路线 D）：按 Profile 隔离（不同上游互不串号）。
	indexMu sync.Mutex
	indexes map[string]*sessionIndex
	// 守卫熔断器（SPEC §5.3）：按 Profile 隔离。
	breakMu  sync.Mutex
	breakers map[string]*adapt.CircuitBreaker
	// 裸模型名路由表（SPEC §29，构造时固化；热更新走 Replace 线程安全）。
	routes *adapt.RouteTable
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "glm-5.2"
	}
	h := &Handler{
		cfg: cfg, mux: http.NewServeMux(), oauth: newOAuthStore(),
		chats:         map[string]string{},
		convAcct:      map[string]string{},
		tencentStates: map[string]tencentState{},
		indexes:       map[string]*sessionIndex{},
		breakers:      map[string]*adapt.CircuitBreaker{},
	}
	// SPEC §29：路由表 = 外部文件加载（main 已 fail-fast）或内置默认表。
	if cfg.Routes != nil {
		h.routes = cfg.Routes
	} else if rt, err := adapt.NewRouteTable(buildDefaultRoutes()); err == nil {
		h.routes = rt
	}
	h.loadChats()
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/messages", h.withAuth(h.anthropicMessages))
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withAuth(h.anthropicCountTokens))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responsesCall))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// WorkBuddy 风格控制台（页面不鉴权，API 走 Bearer）
	h.mux.HandleFunc("GET /{$}", h.servePanel)
	h.mux.HandleFunc("GET /admin", h.servePanel)
	h.mux.HandleFunc("GET /panel", h.servePanel)
	h.mux.HandleFunc("GET /panel/", h.servePanel)
	h.mux.HandleFunc("GET /admin/api/overview", h.withAuth(h.adminOverview))
	h.mux.HandleFunc("POST /admin/api/credits", h.withAuth(h.adminCredits))
	h.mux.HandleFunc("POST /admin/api/checkin", h.withAuth(h.adminCheckin))
	h.mux.HandleFunc("POST /admin/api/keepalive", h.withAuth(h.adminKeepalive))
	h.mux.HandleFunc("POST /admin/api/reload", h.withAuth(h.adminReload))
	h.mux.HandleFunc("POST /admin/api/accounts/enable", h.withAuth(h.adminEnable))
	h.mux.HandleFunc("GET /admin/api/routes", h.withAuth(h.adminRoutesGet))
	h.mux.HandleFunc("PUT /admin/api/routes", h.withAuth(h.adminRoutesPut))
	h.mux.HandleFunc("GET /admin/api/models", h.withAuth(h.adminModelsGet))
	h.mux.HandleFunc("POST /admin/api/oauth/tencent/start", h.withAuth(h.adminTencentOAuthStart))
	h.mux.HandleFunc("POST /admin/api/oauth/tencent/poll", h.withAuth(h.adminTencentOAuthPoll))
	h.mux.HandleFunc("POST /admin/api/accounts/disable", h.withAuth(h.adminDisable))
	h.mux.HandleFunc("POST /admin/api/accounts/clear-cooldown", h.withAuth(h.adminClearCooldown))
	h.mux.HandleFunc("POST /admin/api/oauth/start", h.withAuth(h.adminOAuthStart))
	h.mux.HandleFunc("POST /admin/api/oauth/poll", h.withAuth(h.adminOAuthPoll))
	h.mux.HandleFunc("GET /oauth/callback", h.oauthCallback)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 默认本地：空/change-me 视为未配置鉴权（本机单用户免密，SPEC §29 配套
		// 部署语义）；开放方案（7866:7866）必须设置真实 OMNIGATE_API_KEY。
		apiKey := strings.TrimSpace(h.cfg.APIKey)
		if apiKey != "" && !strings.EqualFold(apiKey, "change-me") {
			authz := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			key := authz[len(prefix):]
			if subtle.ConstantTimeCompare([]byte(key), []byte(apiKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.cfg.Pool.List()})
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

// chatCompletions OpenAI chat 端点（/v1/chat/completions）。
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	h.serveCompletion(w, r, protoChat)
}

// anthropicMessages Anthropic 端点（/v1/messages）。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	h.serveCompletion(w, r, protoAnthropic)
}

// responsesCall OpenAI Responses 端点（/v1/responses）。
func (h *Handler) responsesCall(w http.ResponseWriter, r *http.Request) {
	h.serveCompletion(w, r, protoResponses)
}

// serveCompletion 三协议共用管线：解析（按协议归一化）→ 会话路由 → 折叠 →
// 账号轮转 → 出站（流式走 streamOut，非流式走 completeChat，均按协议重建）。
func (h *Handler) serveCompletion(w http.ResponseWriter, r *http.Request, proto streamProtocol) {
	body, tooLarge, err := readBody(r)
	if err != nil {
		writeProtoError(proto, w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if tooLarge {
		writeProtoError(proto, w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	var req *chatRequest
	switch proto {
	case protoChat:
		req, err = parseChatRequest(body)
	case protoAnthropic:
		req, err = parseAnthropicRequest(body, nil)
	case protoResponses:
		req, err = parseResponsesRequest(body, nil)
	}
	if err != nil {
		writeProtoError(proto, w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.ConversationID == "" {
		req.ConversationID = r.Header.Get("X-Codearts-Chat-Id")
	}

	// SPEC §29.2：显式渠道（X-Provider/body.provider）优先，否则按裸模型名
	// 查路由表；未命中 → codearts 缺省。
	explicit := r.Header.Get("X-Provider")
	if explicit == "" {
		explicit = req.Provider
	}
	model := h.cfg.DefaultModel
	if req.Model != "" && req.Model != "auto" {
		model = req.Model
	}
	profile := h.resolveProfile(explicit, model)
	if profile == nil {
		// 模型在路由表禁用集（C1）：明确拒绝，不回落缺省渠道
		writeProtoError(proto, w, http.StatusNotFound, "model_not_found",
			"model "+model+" is disabled")
		return
	}
	// Profile 非文本块占位模板（§13.3）：parse 期用内置默认，此处统一替换
	if ph := h.mediaPlaceholder(profile); ph != nil {
		req.Messages = reapplyMediaPlaceholder(req.Messages, ph)
	}
	if !profile.Inbound.Allows(string(proto)) {
		writeProtoError(proto, w, http.StatusNotFound, "not_found",
			"protocol "+string(proto)+" not enabled (profile "+profile.ID+")")
		return
	}

	toolsOn := toolsActive(req)

	// 工具层（SPEC §14）：默认全关；project 有损 ⇒ forceNative（与增量互斥）。
	// 指纹计算输入 = toolchain 变换后的最终消息数组（§15.1）。
	projectOn, sanitizeOn := toolchainEnabled(profile, h.cfg.ToolchainOverride)
	if projectOn {
		msgs, stats := projectRequest(req.Messages, req.Tools, projectCfg(profile))
		log.Printf("toolchain project profile=%s mode=%s chars=%d->%d dropped=%d summarized=%d anchor=%v",
			profile.ID, stats.Mode, stats.OriginalChars, stats.ProjectedChars,
			stats.DroppedHarness, stats.SummarizedCount, stats.AnchorPreserved)
		req.Messages = msgs
	}
	if sanitizeOn {
		msgs, hits := sanitizeRequest(req.Messages, sanitizeCfg(profile))
		log.Printf("toolchain sanitize profile=%s hits=%d", profile.ID, hits)
		req.Messages = msgs
	}

	// 会话路由：显式 conversation_id 或指纹续接（路线 D）。
	// 指纹续接：前缀命中 → tail 增量折叠 + 上游会话粘性。
	rr := h.routeSession(profile, req, projectOn)
	chatID, stickyAcct := rr.ChatID, rr.StickyAcct

	msgs := buildUpstreamMessages(req, profile, toolsOn, rr.Continue, rr.TailMsgs)
	h.logFold(model, req.Messages, msgs, toolsOn, rr.Continue)

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.pickAccount(profile.Auth.Family(), &stickyAcct, tried)
		if acct == nil {
			break
		}
		// 阻塞等待并发槽位（上游单账号并发会话释放慢，串行最稳）。
		// 超时 180s 排队（5 个请求 × ~30s 上限），避免高并发立即失败跳号。
		if !h.cfg.Pool.AcquireLockWait(acct.Name, 180*time.Second) {
			log.Printf("account %s concurrent limit reached after wait, trying next", acct.Name)
			continue
		}
		ok, verr := h.cfg.Pool.Validate(acct)
		if !ok || verr != nil {
			h.cfg.Pool.ReleaseLock(acct.Name)
			if verr != nil {
				lastErr = verr
			} else {
				lastErr = errors.New("token invalid")
			}
			continue
		}
		// 检查是否需要保活
		if h.cfg.Pool.NeedKeepalive(acct.Name) {
			h.cfg.Pool.PingKeepalive(acct.Name)
		}
		// 指纹续接已带 chatID；否则用会话显式 id 或现场新建。
		// 上游要求 chat_id 为 32 位十六进制（UUID 去连字符）；无显式 id 时每次新建。
		if !validChatID(chatID) {
			chatID = req.ConversationID
			if !validChatID(chatID) {
				chatID = randHex(32)
			}
		}

		cred := upstream.SignCredential{
			AccessKeyID:     acct.Auth.AccessKeyID,
			SecretAccessKey: acct.Auth.SecretAccessKey,
			SecurityToken:   acct.Auth.CloudDragonTok,
			UserID:          acct.Auth.UserID,
			EnterpriseID:    acct.Auth.EnterpriseID,
			Domain:          acct.Auth.Domain,
		}
		// tool_choice 上游 string 语义（§28.4 决策 D）：仅腾讯家族归一化，
		// "none" 语义 = 不传 tool_choice 且连 tools 一并删除；华为路径不动。
		tools := req.Tools
		toolChoice, dropTools := "", false
		if profile.Auth.Family() == "workbuddy" {
			toolChoice, dropTools = tencentToolChoice(req.ToolChoice)
			if dropTools {
				tools = nil
			}
		}
		rc, serr := h.openStream(r, acct, chatID, msgs, cred, model, tools, toolChoice)
		if serr != nil {
			h.cfg.Pool.ReleaseLock(acct.Name) // 释放槽位再换号
			if isClientCancel(serr) {
				writeProtoError(proto, w, http.StatusServiceUnavailable, "client_cancelled", "client disconnected")
				return
			}
			lastErr = serr
			h.handleUpstreamError(acct, serr)
			continue
		}

		w.Header().Set("X-Codearts-Chat-Id", chatID)

		storeChat := func() {
			// 仅缓存显式会话，便于同 conversation_id 续聊；不把一次性测连写进账号默认会话。
			if !rr.Explicit {
				return
			}
			h.convMu.Lock()
			h.chats[acct.Name] = chatID
			h.convAcct[req.ConversationID] = acct.Name // 黏性路由：续接锁定同账号
			h.convMu.Unlock()
			h.saveChats()
		}
		if req.Stream {
			sink := newSink(proto, w, model, profile.IsRateLimit)
			if h.streamOut(sink, acct, model, profile, rr.MatchedKey, rc) {
				storeChat()
				h.turnSuccess(profile, acct, chatID, rr.RouteKey, len(req.Messages))
			}
			return
		}
		if done, aerr := h.completeChat(w, acct, rc, req, profile, proto, model, chatID, rr.RouteKey, rr.MatchedKey, msgs, storeChat); aerr != nil {
			lastErr = aerr
			if done {
				return
			}
		} else if done {
			return
		}
	}
	msg := "all accounts unavailable (disabled/cooldown)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeProtoError(proto, w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// readBody 读取并限制请求体大小；tooLarge=true 表示超出 8MB 上限。
func readBody(r *http.Request) (body []byte, tooLarge bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > maxBodyBytes {
		return nil, true, nil
	}
	return body, false, nil
}

// pickAccount 取本回合账号：续接会话优先原账号（若仍健康），否则按客户端
// 家族（AuthProfile.ClientFamily）轮换一个未试账号（SPEC §24.1：不同上游
// 家族池内并存互不混用；同家族多 Profile 共享账号）。
func (h *Handler) pickAccount(family string, stickyAcct *string, tried map[string]bool) *pool.Account {
	if *stickyAcct != "" {
		// 续接会话：锁定原账号（若仍健康）。
		acct := h.cfg.Pool.Get(*stickyAcct)
		if acct != nil && h.cfg.Pool.Healthy(*stickyAcct) {
			tried[acct.Name] = true
			return acct
		}
		*stickyAcct = ""
	}
	var acct *pool.Account
	if acct = h.cfg.Pool.PickFor(family, tried); acct != nil {
		tried[acct.Name] = true
	}
	return acct
}

// openStream 发起上游聊天流。并发会话上限（TM.00001041）是瞬时的：
// 已完成的会话槽位释放较慢（实测 >15s），遇到时等待后重试同一账号，
// 最多 10 次（每次 5s，共 50s）。等待期间释放并发锁，让排队请求也能
// 尝试（避免死锁式串行等待）。
func (h *Handler) openStream(r *http.Request, acct *pool.Account, chatID string, msgs []upstream.ChatMessage, cred upstream.SignCredential, model string, tools []map[string]any, toolChoice string) (io.ReadCloser, error) {
	ctx := r.Context()
	var lastErr error
	for retry := 0; retry < 10; retry++ {
		rc, serr := acct.Client.ChatStream(ctx, chatID, msgs, "", cred, acct.UserName, model, tools, toolChoice)
		if serr == nil {
			return rc, nil
		}
		lastErr = serr
		var ae *upstream.ApiError
		if errors.As(serr, &ae) && ae.Status == 400 && isConcurrentLimitError(ae.Message) {
			log.Printf("upstream concurrent limit retry=%d account=%s, waiting 5s", retry+1, acct.Name)
			// 释放锁让其他请求有机会，等待后重新获取
			h.cfg.Pool.ReleaseLock(acct.Name)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if !h.cfg.Pool.AcquireLockWait(acct.Name, 30*time.Second) {
				return nil, errors.New("concurrent limit: could not reacquire lock after wait")
			}
			continue
		}
		return nil, serr // 非并发错误，立即返回
	}
	return nil, lastErr
}

// completeChat 非流式收尾：聚合上游、提取工具调用、结算回合并回写响应。
// 返回（响应已结束, 本回合错误）。错误分两类：可换号重试（限流冷却），
// 或直接结束（客户端断开）。响应按 proto 重建为各协议对象。
func (h *Handler) completeChat(w http.ResponseWriter, acct *pool.Account, rc io.ReadCloser, req *chatRequest, profile *adapt.UpstreamProfile, proto streamProtocol, model, chatID, routeKey, matchedKey string, msgs []upstream.ChatMessage, storeChat func()) (bool, error) {
	comp, aerr := upstream.AggregateRaw(rc)
	rc.Close()
	h.cfg.Pool.ReleaseLock(acct.Name) // 释放锁
	if aerr != nil {
		// 客户端主动断开不是账号故障，不冷却（直接结束本次请求）。
		if isClientCancel(aerr) {
			log.Printf("chat account=%s client canceled upstream wait; no penalty", acct.Name)
			h.turnFailure(profile, matchedKey)
			return true, nil
		}
		// 限流是瞬时的：短软冷却退避，不计入错误阈值。
		if profile.IsRateLimit(aerr.Error()) {
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, 45*time.Second, aerr.Error())
		} else {
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolErr, h.cfg.ErrCooldown, aerr.Error())
		}
		return false, aerr
	}

	content, finish := comp.Content, comp.Finish
	var calls []openAIToolCall
	if toolsActive(req) {
		if len(comp.ToolCalls) > 0 {
			// 原生 tool_calls 路径（roles 上游）：聚合结果直取，文本不剥离
			// （§28.4 决策 D 单帧语义）；缺 id 补 call_xxx 兜底。
			for _, tc := range comp.ToolCalls {
				calls = append(calls, openAIToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
			}
			assignCallIDs(calls)
			finish = "tool_calls"
		} else if c, rest, found := extractToolCalls(comp.Content); found {
			calls = c
			assignCallIDs(calls)
			content = rest
			finish = "tool_calls"
		}
	}
	storeChat()
	h.turnSuccess(profile, acct, chatID, routeKey, len(req.Messages))
	if transcriptEcho(content) {
		log.Printf("TRANSCRIPT_ECHO model=%s chars=%d", model, len([]rune(content)))
		h.dumpEcho(model, content)
	}
	usage := usageEstimate(msgs, comp)
	switch proto {
	case protoAnthropic:
		writeJSON(w, http.StatusOK, buildAnthropicMessage(model, content, calls, finish, usage))
	case protoResponses:
		writeJSON(w, http.StatusOK, buildResponsesResponse(model, content, calls, finish, usage))
	default:
		writeJSON(w, http.StatusOK, buildCompletion(model, comp.Reasoning, content, calls, finish, chatID, usage))
	}
	return true, nil
}

// buildUpstreamMessages 按 Profile.message.model 渲染（SPEC §22.2）：
// roles → 归一化数组逐条映射原生 OpenAI messages（不经折叠）；
// text-only → 现有折叠（system 提前/转录渲染/护栏），单条 user 消息。
func buildUpstreamMessages(req *chatRequest, profile *adapt.UpstreamProfile, toolsOn bool, continueChat bool, tailMsgs []openAIMessage) []upstream.ChatMessage {
	if profile.Message.Model == "roles" {
		msgs := req.Messages
		if continueChat && tailMsgs != nil {
			msgs = tailMsgs
		}
		return renderRolesMessages(msgs)
	}
	if continueChat {
		// 指纹续接（路线 D）传入精确增量切片；显式会话沿用旧的「最后一条 user 起」启发式。
		tail := tailMsgs
		if tail == nil {
			tail = req.Messages
			for i := len(req.Messages) - 1; i >= 0; i-- {
				if req.Messages[i].Role == "user" {
					tail = req.Messages[i:]
					break
				}
			}
		}
		return []upstream.ChatMessage{{Role: "user", Content: renderTailPrompt(tail, toolsBlockFor(req, toolsOn))}}
	}
	return []upstream.ChatMessage{{Role: "user", Content: renderFullPrompt(req.Messages, toolsBlockFor(req, toolsOn))}}
}

// toolsBlockFor 工具注入块（text-only 模拟层用；roles 上游不经折叠无此块）。
func toolsBlockFor(req *chatRequest, toolsOn bool) string {
	if !toolsOn {
		return ""
	}
	return buildToolsPrompt(normalizeTools(req.Tools), req.ToolChoice)
}

// tencentToolChoice 归一化 tool_choice 为上游 string 语义（§28.4 决策 D，
// 对齐 workbuddy2api normalizeToolChoice 表）：
//
//	none     → 不传 choice 且删除 tools（上游语义：禁用工具）
//	auto     → "auto"（等价缺省）
//	required → "required"
//	function → 函数名（"x"）
//
// 返回（choice, 是否删 tools）。空 choice 表示上游 body 不携带 tool_choice。
func tencentToolChoice(tc toolChoiceOpenAI) (string, bool) {
	switch tc.Mode {
	case "none":
		return "", true
	case "required":
		return "required", false
	case "function":
		if tc.Function != "" {
			return tc.Function, false
		}
		return "", false
	default: // "auto"（含未指定）
		return "auto", false
	}
}

// renderRolesMessages roles 渲染：归一化数组 → 原生 OpenAI messages（§22.2）。
func renderRolesMessages(msgs []openAIMessage) []upstream.ChatMessage {
	out := make([]upstream.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		cm := upstream.ChatMessage{Role: m.Role, Content: m.Text, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, upstream.ChatToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
		}
		out = append(out, cm)
	}
	return out
}

// buildCompletion 组装非流式响应。
func buildCompletion(model, reasoning, content string, calls []openAIToolCall, finish, chatID string, usage map[string]int) map[string]any {
	message := map[string]any{"role": "assistant"}
	if len(calls) > 0 {
		message["tool_calls"] = toOpenAIToolCalls(calls)
		if content == "" {
			message["content"] = nil
		} else {
			message["content"] = content
		}
	} else {
		message["content"] = content
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"chat_id": chatID,
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// tokensApprox 文本 token 估算基础函数（len/4+1，全项目 usage 估算同源）。
func tokensApprox(s string) int {
	return len([]rune(s))/4 + 1
}

func usageEstimate(msgs []upstream.ChatMessage, comp *upstream.RawCompletion) map[string]int {
	var pt int
	for _, m := range msgs {
		pt += tokensApprox(m.Content)
	}
	ct := tokensApprox(comp.Content) + tokensApprox(comp.Reasoning)
	return map[string]int{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error", "code": code},
	})
}

// writeProtoError 各协议错误信封（SPEC §13.4）：anthropic 顶层 type:error、
// responses OpenAI 风格 error 信封、chat 沿用现有。
func writeProtoError(proto streamProtocol, w http.ResponseWriter, status int, code, msg string) {
	switch proto {
	case protoAnthropic:
		writeJSON(w, status, map[string]any{
			"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": msg},
		})
	case protoResponses:
		writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "code": code}})
	default:
		writeOpenAIError(w, status, code, msg)
	}
}

// randHex 生成 n 个十六进制字符（失败以时间种子兜底，绝不返回错误）。
//
// 刻意不与 upstream.RandomHex 合并：两者 n 语义不同（这里是「字符」，
// 那边是「字节」=2n 字符），且错误处理方式相反（这里永不失败）。
// 合并需重定义语义并改动全部调用点，收益为零、改错即 chat_id/工具调用
// id 长度错乱。触发条件：出现第三个随机 hex 生成器时，统一为一个共享
// 工具包（含统一语义与错误策略）。
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := crand.Read(b); err != nil {
		seed := fmt.Sprintf("%x", time.Now().UnixNano())
		for len(seed) < n {
			seed += seed
		}
		return seed[:n]
	}
	return hex.EncodeToString(b)[:n]
}

func validChatID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func (h *Handler) loadChats() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	raw, err := os.ReadFile(h.cfg.ConvStateFile)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, &h.chats)
}

func (h *Handler) saveChats() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	raw, _ := json.MarshalIndent(h.chats, "", "  ")
	if err := os.WriteFile(h.cfg.ConvStateFile+".tmp", raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(h.cfg.ConvStateFile+".tmp", h.cfg.ConvStateFile)
}
