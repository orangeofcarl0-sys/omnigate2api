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
	"strconv"
	"strings"
	"sync"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

// CodeArts OAuth（与 cmd/login / 旧 uiLogin 一致）：ticket + PKCE，服务端轮询 snap-manager。
const oauthSessionTTL = 15 * time.Minute

type oauthSession struct {
	ID        string
	TicketID  string
	Secret    string
	Verifier  string
	Port      int
	AuthURL   string
	CreatedAt time.Time
	Done      bool
	Err       string
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

// tencentState 一条进行中的腾讯设备流（state → 过期时间）。
type tencentState struct {
	AuthURL string `json:"auth_url"`
	Expires int64  `json:"expires"` // Unix 秒
}

// getBySecret 按 portal 回调携带的 secret 定位会话（code 通道用）。
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
	port := h.listenPort()
	cfg := upstream.DefaultLoginConfig()
	authURL := upstream.New(10*time.Second).BuildAuthorizeURL(cfg, ticketID, challenge, "S256", port)
	// 可选：把回调端口改写为公网反代地址的端口，让浏览器回调尽量命中 hub；
	// 远端若仍无法回调则回退到 ticket 轮询通道，不影响登录完成。
	if h.cfg.OAuthCallbackHost != "" {
		if p := portOfCallbackHost(h.cfg.OAuthCallbackHost); p != 0 {
			authURL = rewriteAuthURLPort(authURL, p)
		}
	}

	id := newSessionID()
	h.oauth.put(&oauthSession{
		ID: id, TicketID: ticketID, Secret: secret, Verifier: verifier, Port: port,
		AuthURL: authURL, CreatedAt: time.Now(),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": id,
		"auth_url":   authURL,
		"expires_in": int(oauthSessionTTL.Seconds()),
		"message":    "请在浏览器打开授权链接，完成后点「我已授权」或等待自动检测",
	})
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
	if sess.Done {
		if sess.Err != "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": "error", "message": sess.Err})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "done", "message": "登录成功"})
		return
	}

	// 轮询必须用 portal 的 secret（回调可能已通过 updateSecret 换入）。
	cfg := upstream.DefaultLoginConfig()
	tok, err := upstream.New(15*time.Second).PollTicket(context.Background(), cfg, sess.TicketID, h.oauth.secretFor(sess.TicketID))
	if err != nil || tok == nil || tok.UserName == "" || tok.Credentials.SecurityToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": "pending", "message": "等待浏览器完成登录…",
		})
		return
	}
	if err := h.saveLoginResult(tok, sess.Verifier); err != nil {
		sess.Done = true
		sess.Err = err.Error()
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": "error", "message": err.Error()})
		return
	}
	sess.Done = true
	h.oauth.del(req.SessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  "done",
		"message": fmt.Sprintf("登录成功：%s (%s)", nonempty(tok.UserName, "未命名"), shortID(tok.UserID)),
		"account": map[string]any{"uid": tok.UserID, "nickname": tok.UserName},
	})
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

// rewriteAuthURLPort 改写授权链接的 port 参数（华为 portal 据此拼回调地址）。
func rewriteAuthURLPort(authURL string, port int) string {
	u, err := url.Parse(authURL)
	if err != nil {
		return authURL
	}
	q := u.Query()
	q.Set("port", fmt.Sprint(port))
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
				log.Printf("oauth callback: updated secret for ticket=%s", tid)
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
	// 通过 secret 找到对应待登录会话（取 verifier/port）
	sess := h.oauth.getBySecret(secret)
	if sess == nil {
		// 没有匹配（浏览器在远端时 code 通道不可用），提示用 ticket 通道
		_, _ = w.Write([]byte("<h3>登录已提交，请回到 WebUI 等待结果。</h3>"))
		return
	}
	cfg := upstream.DefaultLoginConfig()
	tok, err := upstream.New(15*time.Second).ExchangeCode(r.Context(), cfg, code, sess.Verifier, sess.Port)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>换取凭证失败：" + err.Error() + "</h3>"))
		return
	}
	if err := h.saveLoginResult(tok, sess.Verifier); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>保存账号失败：" + err.Error() + "</h3>"))
		return
	}
	_, _ = w.Write([]byte("<h3>登录成功，可关闭此页面并回到 WebUI。</h3>"))
}

// saveLoginResult 落盘 auth 并加入账号池。
func (h *Handler) saveLoginResult(tok *upstream.TokenResponse, codeVerifier string) error {
	if h.cfg.AuthDir == "" {
		return errors.New("auth_dir not configured")
	}
	cred := tok.Credentials
	a := auth.New(tok.UserID, tok.UserName, tok.DomainID,
		cred.SecurityToken, cred.AccessKeyID, cred.SecretAccessKey,
		cred.Expiration, tok.RefreshToken, codeVerifier)
	if err := auth.SaveNew(h.cfg.AuthDir, a); err != nil {
		return err
	}
	h.cfg.Pool.AddAccount(a)
	log.Printf("webui login success user_id=%s name=%s", tok.UserID, tok.UserName)
	return nil
}
