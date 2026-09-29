package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

// CodeArts OAuth（与 cmd/login / 旧 uiLogin 一致）：授权码 + PKCE + DPoP，服务端轮询 snap-manager。
const oauthSessionTTL = 15 * time.Minute

// 频次闸的渠道键（SPEC §24.6）：华为与腾讯两区各自独立计频。
const (
	guardChanHuawei = "huawei"
	guardChanTencCN = "tencent:cn"
	guardChanTencGL = "tencent:global"
)

type oauthSession struct {
	ID        string
	TicketID  string
	Secret    string
	Verifier  string
	Port      int
	AuthURL   string
	CreatedAt time.Time
	// DpopKey 本会话的 DPoP 私钥（JWK JSON）。登录成功后随凭证落盘——refresh_token 与
	// DPoP 公钥绑定，刷新必须复用同一把（SPEC §24.5）。
	DpopKey string

	// 完成态：授权码由回调消费（回调 goroutine 写，面板轮询 goroutine 读）→ 加锁。
	mu     sync.Mutex
	done   bool
	errMsg string
	added  bool
	uid    string
	name   string
}

// markDone 记录登录结果（回调或 ticket 轮询成功后调用）。
func (s *oauthSession) markDone(uid, name string, added bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done, s.errMsg, s.added, s.uid, s.name = true, "", added, uid, name
}

// markErr 记录登录失败原因。
func (s *oauthSession) markErr(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done, s.errMsg = true, msg
}

// state 快照（面板轮询读取）。
func (s *oauthSession) state() (done bool, errMsg string, added bool, uid, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done, s.errMsg, s.added, s.uid, s.name
}

type oauthStore struct {
	mu   sync.Mutex
	byID map[string]*oauthSession
}

func newOAuthStore() *oauthStore {
	return &oauthStore{byID: map[string]*oauthSession{}}
}

func (s *oauthStore) put(sess *oauthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.byID[sess.ID] = sess
}

func (s *oauthStore) get(id string) *oauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	return s.byID[id]
}

// tencentState 一条进行中的腾讯设备流（state → 过期时间 + 区域）。
// 区域随 state 记住：token/account 两段只在发起时选定的 base 上有效。
type tencentState struct {
	AuthURL string `json:"auth_url"`
	Expires int64  `json:"expires"` // Unix 秒
	Realm   string `json:"realm"`   // ""/"cn" 国内；"global" 国际
}

// getBySecret 按 portal 下发的 secret 定位会话（旧 ticket 流程的一阶段握手用）。
// 注意：授权码回调**不带 secret**，那条路用 byStateOrSole。
func (s *oauthStore) getBySecret(secret string) *oauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	for _, sess := range s.byID {
		if sess.Secret == secret {
			return sess
		}
	}
	return nil
}

// updateSecret 换入 portal 下发的 secret（ticket 轮询必须用它，而非本地生成的）。
func (s *oauthStore) updateSecret(tid, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.byID {
		if sess.TicketID == tid {
			sess.Secret = secret
			return
		}
	}
}

// byStateOrSole 按 state 定位会话。**授权码回调不带 secret**（secret 是旧 ticket 流程的
// 一阶段握手产物），所以不能用 getBySecret：优先 state 命中会话 ID / ticket_id，
// 退化到"唯一未完成的会话"。
func (s *oauthStore) byStateOrSole(state string) *oauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	if state != "" {
		if sess, ok := s.byID[state]; ok {
			return sess
		}
		for _, sess := range s.byID {
			if sess.TicketID == state {
				return sess
			}
		}
	}
	var only *oauthSession
	seen := 0
	for _, sess := range s.byID {
		if done, _, _, _, _ := sess.state(); done {
			continue
		}
		seen++
		if seen > 1 {
			return nil // 多个待完成会话，无法判定是哪一次
		}
		only = sess
	}
	return only
}

// secretFor 返回会话当前的 secret（加锁读取，供轮询取用）。
func (s *oauthStore) secretFor(tid string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.byID {
		if sess.TicketID == tid {
			return sess.Secret
		}
	}
	return ""
}

func (s *oauthStore) del(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

func (s *oauthStore) gcLocked() {
	now := time.Now()
	for id, sess := range s.byID {
		if now.Sub(sess.CreatedAt) > oauthSessionTTL {
			delete(s.byID, id)
		}
	}
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *Handler) listenPort() int {
	s := h.cfg.Listen
	if i := strings.LastIndex(s, ":"); i >= 0 && i+1 < len(s) {
		if p, err := strconv.Atoi(s[i+1:]); err == nil && p > 0 {
			return p
		}
	}
	return 7866
}

// adminOAuthStart 发起 CodeArts 授权。
func (h *Handler) adminOAuthStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	// 频次闸：连点/失败重试是 2026-09-28 那次"账号访问受限"的直接诱因（SPEC §24.6）。
	if d := h.loginGuard.allow(guardChanHuawei, time.Now()); !d.OK {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "throttled": true,
			"retry_after_seconds": int(d.RetryAfter.Seconds()) + 1,
			"message":             d.Reason,
		})
		return
	}
	ticketID, err := upstream.RandomHex(16)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	secret, err := upstream.RandomHex(16)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	verifier, challenge, err := upstream.PKCE()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	// DPoP 私钥在**发起时**就生成并与会话绑定：授权码换发与随后的 refresh 必须用同一把
	// （SPEC §24.5——换密钥刷新会被上游拒 InvalidDPoPHeader）。
	dpopKey, err := upstream.NewDpopKeyJSON()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "dpop key: " + err.Error()})
		return
	}
	port := h.listenPort()
	cfg := upstream.DefaultLoginConfig()
	// state = 会话 ID：授权码回调不带 secret，用它定位是哪个会话（门户若回显 state 最稳，
	// 不回显时退化为"唯一未完成会话"，见 byStateOrSole）。
	id := newSessionID()
	// code_challenge_method 用官方的 SHA-256 + 显式 auth_callback_url：这两条决定门户
	// 回**授权码**而不是旧 ticket 握手（只有前者换发的响应里才有 refresh_token）。
	authURL := upstream.New(10*time.Second).BuildAuthorizeURL(cfg, ticketID, challenge, upstream.CodeChallengeMethod, id, port)
	// 可选：把回调端口改写为公网反代地址的端口，让浏览器回调尽量命中 hub；
	// 远端若仍无法回调则回退到 ticket 轮询通道，不影响登录完成。
	if h.cfg.OAuthCallbackHost != "" {
		if p := portOfCallbackHost(h.cfg.OAuthCallbackHost); p != 0 {
			authURL = rewriteAuthURLPort(authURL, p)
		}
	}

	h.oauth.put(&oauthSession{
		ID: id, TicketID: ticketID, Secret: secret, Verifier: verifier, Port: port,
		AuthURL: authURL, CreatedAt: time.Now(), DpopKey: dpopKey,
	})
	h.loginGuard.recordStart(guardChanHuawei, oauthSessionTTL, time.Now())

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": id,
		"auth_url":   authURL,
		"expires_in": int(oauthSessionTTL.Seconds()),
		"message":    h.oauthStartMessage(guardChanHuawei, "请在浏览器打开授权链接，完成后点「我已授权」或等待自动检测"),
	})
}

// oauthStartMessage 在标准提示后追加"最近刚登过"的提醒（同一渠道内）。
// 用户看不到上游风控在看什么，只能靠我们把话说在前面。
func (h *Handler) oauthStartMessage(channel, base string) string {
	if uid, ago, ok := h.loginGuard.recentLogin(channel, time.Now()); ok && ago < 10*time.Minute {
		return fmt.Sprintf("%s（提醒：%s 前刚为账号 %s 完成过登录，重复授权会被上游判为异常，非必要请停止）",
			base, humanDur(ago), shortID(uid))
	}
	return base
}

// adminOAuthPoll 轮询 ticket。
func (h *Handler) adminOAuthPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	if req.SessionID == "" {
		req.SessionID = r.URL.Query().Get("session_id")
	}
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "status": "error", "message": "session_id required"})
		return
	}
	sess := h.oauth.get(req.SessionID)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "status": "error", "message": "会话不存在或已过期，请重新发起授权"})
		return
	}
	// 完成态优先：授权码由**回调**消费（回调拿到 code 就换凭证并记 done），这里只读结果。
	if done, errMsg, added, uid, name := sess.state(); done {
		if errMsg != "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": "error", "message": errMsg})
			return
		}
		h.oauth.del(req.SessionID)
		h.initAccountAsync(uid) // 额度快照 + 当日福利领取（异步，失败不影响登录）
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": "done", "added": added,
			"message": loginSuccessMsg(name, uid, added),
			"account": map[string]any{"uid": uid, "nickname": name},
		})
		return
	}

	// 旧 ticket 通道兜底（门户对老客户端只做 secret+redirect 握手时才会走到这里；
	// 新流程下门户直接回授权码，不会绑定 ticket）。
	cfg := upstream.DefaultLoginConfig()
	tok, err := upstream.New(15*time.Second).PollTicket(context.Background(), cfg, sess.TicketID, h.oauth.secretFor(sess.TicketID))
	if err != nil || tok == nil || tok.UserName == "" || tok.Credentials.SecurityToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": "pending", "message": "等待浏览器完成登录…",
		})
		return
	}
	// 同一 uid 已在池中 = 这次是更新凭证而不是新增账号（与腾讯渠道同一口径，
	// 面板据此提示"想新增账号请换一个账号登录"）。
	added, err := h.saveLoginResult(tok, sess.Verifier, sess.TicketID, h.oauth.secretFor(sess.TicketID), "")
	if err != nil {
		h.loginGuard.recordFailed(guardChanHuawei, err.Error(), time.Now())
		sess.markErr(err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": "error", "message": err.Error()})
		return
	}
	sess.markDone(tok.UserID, tok.UserName, added)
	h.loginGuard.recordDone(guardChanHuawei, tok.UserID, time.Now())
	h.oauth.del(req.SessionID)
	if !added {
		log.Printf("huawei login uid=%s 已在池中 → 仅更新凭证（未新增账号）", shortID(tok.UserID))
	}
	h.initAccountAsync(tok.UserID) // 额度快照 + 当日福利领取（异步，失败不影响登录）
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  "done",
		"added":   added,
		"message": loginSuccessMsg(tok.UserName, tok.UserID, added),
		"account": map[string]any{"uid": tok.UserID, "nickname": tok.UserName},
	})
}

// loginSuccessMsg 登录成功文案（新增 vs 更新既有账号，口径与腾讯渠道一致）。
func loginSuccessMsg(name, uid string, added bool) string {
	if added {
		return fmt.Sprintf("登录成功：%s (%s)——已触发额度/福利领取初始化", nonempty(name, "未命名"), shortID(uid))
	}
	return fmt.Sprintf("登录成功：%s (%s) 已在账号池中——已更新其凭证。想新增账号请换一个账号登录",
		nonempty(name, "未命名"), shortID(uid))
}

func nonempty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func shortID(u string) string {
	if len(u) <= 12 {
		return u
	}
	return u[:8] + "…"
}

// portOfCallbackHost 从 host[:port] 提取端口，支持带 scheme 的地址。
func portOfCallbackHost(host string) int {
	trimmed := strings.TrimSpace(host)
	if i := strings.LastIndex(trimmed, ":"); i >= 0 {
		// 去掉可能存在的 scheme（https://）
		s := trimmed[i+1:]
		s = strings.TrimRight(s, "/")
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// rewriteAuthURLPort 改写授权链接的 port 与 auth_callback_url（华为 portal 据此拼回调
// 地址）。两者必须一起改：换码时的 redirect_uri 要与之逐字一致。
func rewriteAuthURLPort(authURL string, port int) string {
	u, err := url.Parse(authURL)
	if err != nil {
		return authURL
	}
	q := u.Query()
	q.Set("port", fmt.Sprint(port))
	if cb := q.Get("auth_callback_url"); cb != "" {
		if cu, err := url.Parse(cb); err == nil {
			cu.Host = fmt.Sprintf("127.0.0.1:%d", port)
			q.Set("auth_callback_url", cu.String())
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// oauthCallback 本地回调：浏览器同机时由 portal 携带 code 跳到这里。
func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	secret := r.URL.Query().Get("secret")
	redirect := r.URL.Query().Get("redirect")
	// 只记长度不记值：code/secret 是 portal 下发的一次性凭据，且 redirect 查询串
	// 也携带 secret——值进 stdout/docker 日志即泄露进日志采集面（发布安全审查 F1）。
	log.Printf("oauth callback hit: code_bytes=%d secret_bytes=%d redirect_len=%d", len(code), len(secret), len(redirect))
	// portal 第一次回调：带 secret + redirect，要求 307 跳转（登录页链路的一部分）。
	if secret != "" && redirect != "" {
		// 用 redirect 里的 ticket_id 定位待登录会话，并把华为云下发的 secret 换进去
		// （ticket 轮询必须用 portal 的 secret，而不是本地生成的）。
		if u, err := url.Parse(redirect); err == nil {
			if tid := u.Query().Get("ticket_id"); tid != "" {
				h.oauth.updateSecret(tid, secret)
				// 只记 host+path（查询串含 secret，不入日志）：用于判断门户把浏览器
				// 引向何处（http 回调 vs uri_scheme 自定义协议），排查 code 通道为何不来。
				log.Printf("oauth callback: updated secret for ticket=%s redirect_to=%s%s", tid, u.Host, u.Path)
			}
		}
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<h3>登录失败：缺少 code</h3><p>请回到 WebUI 重新发起登录。</p>"))
		return
	}
	// 授权码流程不带 secret，用 state 定位会话（门户不一定回显 state，退化到
	// "唯一未完成会话"）。排查用：只打参数名，不打值（code/state 都是一次性凭据）。
	keys := make([]string, 0, 4)
	for k := range r.URL.Query() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	log.Printf("oauth callback query keys: %v", keys)
	sess := h.oauth.byStateOrSole(r.URL.Query().Get("state"))
	if sess == nil {
		// 没有匹配（浏览器在远端、或会话已过期）：提示回到面板用 ticket 通道兜底。
		_, _ = w.Write([]byte("<h3>登录已提交，请回到 WebUI 等待结果。</h3>"))
		return
	}
	cfg := upstream.DefaultLoginConfig()
	// redirect_uri 必须与 authorize 时的 auth_callback_url 逐字一致，否则换码被拒。
	redirectURI := upstream.New(15*time.Second).CallbackURL(cfg, sess.Port)
	tok, err := upstream.New(15*time.Second).ExchangeCode(r.Context(), cfg, code, sess.Verifier, redirectURI, sess.DpopKey)
	if err != nil {
		sess.markErr("换取凭证失败：" + err.Error())
		h.loginGuard.recordFailed(guardChanHuawei, "换取凭证失败", time.Now())
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>换取凭证失败：" + err.Error() + "</h3>"))
		return
	}
	added, err := h.saveLoginResult(tok, sess.Verifier, sess.TicketID, secret, sess.DpopKey)
	if err != nil {
		sess.markErr("保存账号失败：" + err.Error())
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>保存账号失败：" + err.Error() + "</h3>"))
		return
	}
	sess.markDone(tok.UserID, tok.UserName, added)
	h.loginGuard.recordDone(guardChanHuawei, tok.UserID, time.Now())
	_, _ = w.Write([]byte("<h3>登录成功，可关闭此页面并回到 WebUI。</h3>"))
}

// initAccountAsync 登录成功后的账号初始化（SPEC §24.3）：校验凭证 + 补跑该账号
// 当日的每日动作（华为福利领取 / 腾讯签到 + 额度快照）。
//
// 异步、best-effort：不阻塞登录响应（浏览器端还会延迟回读一次面板）；失败只记
// 日志——账号已入池，调度器 Tick（默认 30 分钟）与面板手动入口仍兜底。
// 不这么做的话：新账号的额度快照要等下一个 Tick，当日已跑过的签到/福利（按动作
// 去重）更是要等次日，面板就一直是"未查询"。
func (h *Handler) initAccountAsync(uid string) {
	if h.cfg.AccountInit == nil || uid == "" {
		return
	}
	acct := h.cfg.Pool.Get(uid)
	if acct == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		h.cfg.AccountInit(ctx, acct)
	}()
}

// saveLoginResult 落盘 auth 并加入账号池，返回 added（true=新增账号，false=更新既有 uid）。
//
// ticketID/secret 非空时一并持久化（旧 ticket 通道）；dpopKey 非空时持久化 DPoP 私钥
// （授权码通道，SPEC §24.5——它与 refresh_token 绑定，refresh 必须复用同一把）。
func (h *Handler) saveLoginResult(tok *upstream.TokenResponse, codeVerifier, ticketID, ticketSecret, dpopKey string) (bool, error) {
	if h.cfg.AuthDir == "" {
		return false, errors.New("auth_dir not configured")
	}
	// 身份缺失时拒绝落盘：会写出 `codearts-unknown.json` 并在池里多一个空名账号
	// （授权码响应不带 user_id，靠 identity.go 补；补不上就当场失败让用户重试）。
	if strings.TrimSpace(tok.UserID) == "" {
		return false, errors.New("登录响应缺少账号身份（user_id 为空），已放弃落盘；请重试授权登录")
	}
	// 同一 uid 已在池中 = 这次是更新凭证，不是新增账号。
	added := h.cfg.Pool.Get(tok.UserID) == nil
	cred := tok.Credentials
	a := auth.New(tok.UserID, tok.UserName, tok.DomainID,
		cred.SecurityToken, cred.AccessKeyID, cred.SecretAccessKey,
		cred.Expiration, tok.RefreshToken, codeVerifier)
	a.SetTicketCreds(ticketID, ticketSecret)
	a.SetCredentials(tok.RefreshToken, codeVerifier, dpopKey)
	if err := auth.SaveNew(h.cfg.AuthDir, a); err != nil {
		return false, err
	}
	h.cfg.Pool.AddAccount(a)
	renew := "ticket"
	if a.Refresh() != "" {
		renew = "refresh_token"
	}
	log.Printf("webui login success user_id=%s name=%s added=%v renewal=%s",
		tok.UserID, tok.UserName, added, renew)
	return added, nil
}
