// 腾讯设备流登录的管理端入口（SPEC §24.3）：面板「授权登录」的腾讯渠道。
// start 发起授权并暂存 state；poll 轮询授权结果，成功后落盘凭证并加入账号池。
//
// 区域（realm）由**发起时**显式选择并随 state 记住：国内 copilot.tencent.com
// （登录页 codebuddy.cn）/ 国际 www.workbuddy.ai（登录页含 Google/GitHub）。
// state 只在对应 base 上有效，所以 token/account 两段必须与 start 同区域
// （HANDOFF §6.6：区域由 base 决定，不由账号决定）。
package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

const tencentDeviceFlowTTL = 10 * time.Minute

// tencentDeviceClient 无状态设备流客户端（面板专用；池内账号实例不参与）。
func tencentDeviceClient() *upstream.TencentClient {
	return upstream.NewTencent(30 * time.Second)
}

// normTencentRealm 归一区域入参：只认 "global"（其它一律国内）。
func normTencentRealm(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), upstream.DeviceFlowRealmGlobal) {
		return upstream.DeviceFlowRealmGlobal
	}
	return "cn"
}

// adminTencentOAuthStart 发起腾讯设备流：返回授权链接（浏览器打开）+ state。
// body 可选 {"realm":"cn"|"global"}（缺省国内）。
func (h *Handler) adminTencentOAuthStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) // body 空 = 国内
	realm := normTencentRealm(body.Realm)

	authURL, state, err := tencentDeviceClient().DeviceFlowState(realm)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	h.tencentMu.Lock()
	h.tencentStates[state] = tencentState{AuthURL: authURL, Expires: time.Now().Add(tencentDeviceFlowTTL).Unix(), Realm: realm}
	h.tencentMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth_url": authURL, "state": state, "realm": realm})
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
		// 终端态而不是"再等等"：授权超时（上游 state 约 10 分钟）或网关期间重启过
		// （state 只在内存里）。必须让面板停下轮询并提示重开，否则会一直空转。
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "expired": true,
			"message": "state 已失效（授权超过 10 分钟，或网关重启过）——请重新点击「发起授权」"})
		return
	}
	realm := normTencentRealm(st.Realm)

	c := tencentDeviceClient()
	tok, done, err := c.DeviceFlowToken(realm, body.State)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "waiting": true,
			"message": "等待浏览器完成授权…（打开授权链接并登录）"})
		return
	}
	uid, ent, nickname, err := c.DeviceFlowAccount(realm, body.State, tok.AccessToken)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	// 区域落账：凭证 domain 是后续所有请求（chat/计费/活动）的区域判据，必须与
	// 本次登录所选区域一致；上游没回或与所选区域矛盾时按所选区域补齐并记日志。
	domain := strings.TrimSpace(tok.Domain)
	if want := upstream.DeviceFlowDomain(realm); domain == "" ||
		upstream.TencentRegion(domain) != upstream.TencentRegion(want) {
		log.Printf("tencent login realm=%s upstream domain=%q → 按所选区域落账 %q", realm, tok.Domain, want)
		domain = want
	}
	a := &auth.Auth{
		UserID:         uid,
		UserName:       nickname,
		Profile:        "workbuddy",
		CloudDragonTok: tok.AccessToken,
		RefreshToken:   tok.RefreshToken,
		EnterpriseID:   ent,
		Domain:         domain,
		UpdatedAt:      time.Now().Unix(),
	}
	if tok.ExpiresIn > 0 {
		a.Expiration = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	// 同一 uid 已在池中 = 这次是**更新凭证**而不是新增账号（浏览器多半仍登录着同一
	// 账号，授权被自动通过）。必须区分并告诉用户，否则就是"显示登录成功但账号数没变"。
	existed := h.cfg.Pool.Get(uid) != nil
	if err := auth.SaveNew(h.cfg.AuthDir, a); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "save auth: " + err.Error()})
		return
	}
	h.cfg.Pool.AddAccount(a) // 热加入账号池（与 CLI 落盘后重启等效）
	h.tencentMu.Lock()
	delete(h.tencentStates, body.State)
	h.tencentMu.Unlock()
	msg := "登录成功（" + realmLabel(realm) + "）：新增账号 " + nonempty(nickname, shortID(uid)) +
		"——已触发额度/签到初始化"
	if existed {
		msg = "登录成功（" + realmLabel(realm) + "）：uid " + shortID(uid) + " 已在账号池中——已更新其凭证。" +
			"想新增账号请在站点退出登录，或换一个 Google 账号 / 无痕窗口"
	}
	// 落账日志：新增/更新都要留痕——"登录成功但账号数没变"这类疑问靠日志一眼定位。
	if existed {
		log.Printf("tencent login realm=%s uid=%s 已在池中 → 仅更新凭证（未新增账号）", realm, shortID(uid))
	} else {
		log.Printf("tencent login realm=%s uid=%s 新增账号 nickname=%s", realm, shortID(uid), nickname)
	}
	h.initAccountAsync(uid) // 额度/签到当日初始化（异步，失败不影响登录）
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "done",
		"uid": uid, "nickname": nickname, "realm": realm, "added": !existed, "message": msg})
}

// realmLabel 区域中文名（面板提示文案）。
func realmLabel(realm string) string {
	if realm == upstream.DeviceFlowRealmGlobal {
		return "国际版 workbuddy.ai"
	}
	return "国内版 codebuddy.cn"
}
