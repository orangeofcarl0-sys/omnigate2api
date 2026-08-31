// 腾讯设备流登录的管理端入口（SPEC §24.3）：面板「授权登录」的腾讯渠道。
// start 发起授权并暂存 state；poll 轮询授权结果，成功后落盘凭证并加入账号池。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

const tencentDeviceFlowTTL = 10 * time.Minute

// tencentDeviceClient 无状态设备流客户端（面板专用；池内账号实例不参与）。
func tencentDeviceClient() *upstream.TencentClient {
	return upstream.NewTencent(30 * time.Second)
}

// adminTencentOAuthStart 发起腾讯设备流：返回授权链接（浏览器打开）+ state。
func (h *Handler) adminTencentOAuthStart(w http.ResponseWriter, r *http.Request) {
	authURL, state, err := tencentDeviceClient().DeviceFlowState()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	h.tencentMu.Lock()
	h.tencentStates[state] = tencentState{AuthURL: authURL, Expires: time.Now().Add(tencentDeviceFlowTTL).Unix()}
	h.tencentMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth_url": authURL, "state": state})
}

// adminTencentOAuthPoll 轮询授权结果：pending → waiting；成功 → 落盘凭证 +
// 加入账号池（热生效，无需重启）。
func (h *Handler) adminTencentOAuthPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.State == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "state required"})
		return
	}
	h.tencentMu.Lock()
	st, ok := h.tencentStates[body.State]
	if ok && st.Expires < time.Now().Unix() {
		delete(h.tencentStates, body.State)
		ok = false
	}
	h.tencentMu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "state expired, please re-start"})
		return
	}

	c := tencentDeviceClient()
	tok, done, err := c.DeviceFlowToken(body.State)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "waiting": true,
			"message": "等待浏览器完成授权…（打开授权链接并登录）"})
		return
	}
	uid, ent, nickname, err := c.DeviceFlowAccount(body.State, tok.AccessToken)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	a := &auth.Auth{
		UserID:         uid,
		UserName:       nickname,
		Profile:        "workbuddy",
		CloudDragonTok: tok.AccessToken,
		RefreshToken:   tok.RefreshToken,
		EnterpriseID:   ent,
		Domain:         tok.Domain,
		UpdatedAt:      time.Now().Unix(),
	}
	if tok.ExpiresIn > 0 {
		a.Expiration = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	if err := auth.SaveNew(h.cfg.AuthDir, a); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "save auth: " + err.Error()})
		return
	}
	h.cfg.Pool.AddAccount(a) // 热加入账号池（与 CLI 落盘后重启等效）
	h.tencentMu.Lock()
	delete(h.tencentStates, body.State)
	h.tencentMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "nickname": nickname})
}
