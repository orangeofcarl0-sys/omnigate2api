package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/auth"
	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// adminOverview 面板总览。
func (h *Handler) adminOverview(w http.ResponseWriter, r *http.Request) {
	total, healthy, disabled, cooling, credits := h.cfg.Pool.Stats()
	models := h.modelList()
	ids := make([]map[string]any, 0, len(models))
	for _, m := range models {
		ids = append(ids, map[string]any{"id": m["id"]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "omnigate2api",
		"region":  "cn",
		"stats": map[string]any{
			"total":    total,
			"healthy":  healthy,
			"disabled": disabled,
			"cooling":  cooling,
			"credits":  credits,
		},
		"accounts": h.cfg.Pool.List(),
		"models":   ids,
		"schedule": map[string]any{
			"watch": h.cfg.WatchInfo,
		},
	})
}

type uidBody struct {
	UID     string `json:"uid"`
	Account string `json:"account"`
	Reason  string `json:"reason"`
}

func readUIDBody(r *http.Request) (uidBody, error) {
	var b uidBody
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return b, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return b, nil
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if b.UID == "" {
		b.UID = b.Account
	}
	return b, nil
}

type actionResult struct {
	UID     string `json:"uid"`
	OK      bool   `json:"ok"`
	Credits int64  `json:"credits,omitempty"`
	Message string `json:"message,omitempty"`
}

// adminCredits 按家族分发：华为（无积分面）→ Validate/刷新状态；
// 腾讯 → 真实积分余额查询（SPEC §24.2 落地）。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	targets := h.pickTargets(body.UID)
	results := make([]actionResult, 0, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, uid := range targets {
		uid := uid
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			acct := h.cfg.Pool.Get(uid)
			res := actionResult{UID: uid}
			if acct == nil {
				res.Message = "no account"
			} else if api, ok := acct.Client.(upstream.BillingAPI); ok && acct.ProfileID == "workbuddy" {
				if remain, err := api.UserResource(acct.Auth); err != nil {
					res.Message = err.Error()
				} else {
					acct.SetQuota(pool.AccountQuota{Remain: remain, UpdatedAt: time.Now().Unix()})
					res.OK = true
					res.Message = "积分余额 " + strconv.FormatInt(remain, 10)
					res.Credits = remain
				}
			} else if ok, err := h.cfg.Pool.Validate(acct); err != nil {
				res.Message = err.Error()
			} else if !ok {
				res.Message = "token invalid"
			} else {
				res.OK = true
				res.Message = "ok"
			}
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "message": summaryMsg("刷新状态", results), "results": results,
	})
}

// adminCheckin 无签到；映射为全员保活（refresh）。
func (h *Handler) adminCheckin(w http.ResponseWriter, r *http.Request) {
	h.adminKeepalive(w, r)
}

// adminKeepalive 刷新即将过期的 token。
func (h *Handler) adminKeepalive(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	targets := h.pickTargets(body.UID)
	results := make([]actionResult, 0, len(targets))
	for _, uid := range targets {
		acct := h.cfg.Pool.Get(uid)
		res := actionResult{UID: uid}
		if acct == nil {
			res.Message = "no account"
			results = append(results, res)
			continue
		}
		// Validate 内部会在临近过期时自动 Refresh
		ok, verr := h.cfg.Pool.Validate(acct)
		if verr != nil {
			res.Message = verr.Error()
		} else if !ok {
			res.Message = "token invalid"
		} else {
			res.OK = true
			res.Message = "refreshed/validated"
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "message": summaryMsg("保活", results), "results": results,
	})
}

func (h *Handler) adminReload(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	auths, err := auth.LoadDir(h.cfg.AuthDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	h.cfg.Pool.SyncToDir(auths)
	total, healthy, _, _, _ := h.cfg.Pool.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "已重载 auths", "loaded": len(auths), "total": total, "healthy": healthy,
	})
}

func (h *Handler) adminEnable(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	if !h.cfg.Pool.Enable(body.UID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已启用 " + body.UID})
}

func (h *Handler) adminDisable(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	reason := body.Reason
	if reason == "" {
		reason = "manual disable"
	}
	h.cfg.Pool.Disable(body.UID, reason)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已禁用 " + body.UID})
}

func (h *Handler) adminClearCooldown(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	if !h.cfg.Pool.ClearCooldown(body.UID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已清冷却 " + body.UID})
}

func (h *Handler) pickTargets(uid string) []string {
	if uid != "" {
		if h.cfg.Pool.Get(uid) == nil {
			return nil
		}
		return []string{uid}
	}
	out := make([]string, 0)
	for _, st := range h.cfg.Pool.List() {
		if disabled, _ := st["disabled"].(bool); disabled {
			continue
		}
		if u, ok := st["uid"].(string); ok && u != "" {
			out = append(out, u)
		}
	}
	return out
}

func allOK(results []actionResult) bool {
	if len(results) == 0 {
		return true
	}
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}

func summaryMsg(action string, results []actionResult) string {
	if len(results) == 0 {
		return action + ": 无目标账号"
	}
	ok, fail := 0, 0
	for _, r := range results {
		if r.OK {
			ok++
		} else {
			fail++
		}
	}
	if fail == 0 {
		return action + "完成: " + strconv.Itoa(ok) + " 成功"
	}
	return action + "完成: " + strconv.Itoa(ok) + " 成功 / " + strconv.Itoa(fail) + " 失败"
}

// ---------------------------------------------------------------------------
// 模型/路由管理（SPEC §29.4，Bearer 保护与既有 /admin/api/* 一致）
// ---------------------------------------------------------------------------

// adminRoutesGet 当前路由表（model/family 数组）。
func (h *Handler) adminRoutesGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.routesTable().Routes())
}

// adminRoutesPut 全量替换路由表：校验（家族已注册/表内唯一）→ 落盘 → 热生效。
// 校验失败 409 且保持当前表不变。
func (h *Handler) adminRoutesPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Routes []adapt.ModelRoute `json:"routes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request: " + err.Error()})
		return
	}
	table, err := adapt.NewRouteTable(body.Routes)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if len(body.Routes) > 0 {
		for _, rt := range body.Routes {
			if h.profiles().Get(rt.Family) == nil {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "unknown family " + rt.Family})
				return
			}
		}
	}
	if h.cfg.RoutesFile != "" {
		if err := adapt.SaveRouteTableFile(h.cfg.RoutesFile, table); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save routes: " + err.Error()})
			return
		}
	}
	if err := h.routesTable().Replace(body.Routes); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "routes": h.routesTable().Routes()})
}

// adminModelsGet 模型全貌：?family=codearts|workbuddy → 该家族清单；
// 缺省 → 唯一视图（与 /v1/models 无渠道一致）。
func (h *Handler) adminModelsGet(w http.ResponseWriter, r *http.Request) {
	fam := r.URL.Query().Get("family")
	if fam != "" {
		if h.profiles().Get(fam) == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown family"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"family": fam, "data": h.modelListFor(fam)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": h.unifiedModelList()})
}
