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
	total, healthy, disabled, cooling := h.cfg.Pool.Stats()
	// 模型卡展示唯一视图（不是华为单渠道清单）：否则面板顶部「模型」与「模型与
	// 路由」两处口径不一致，运维会以为腾讯模型不存在。
	models := h.unifiedModelList()
	ids := make([]map[string]any, 0, len(models))
	for _, m := range models {
		ids = append(ids, map[string]any{
			"id": m["id"], "family": m["family"],
			"access": m["access"], "access_label": m["access_label"],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "omnigate2api",
		"region":  "cn",
		"stats": map[string]any{
			"total":    total,
			"healthy":  healthy,
			"disabled": disabled,
			"cooling":  cooling,
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

// runForTargets 按 uid 批量并发执行账号动作（并发上限 5，顺序无关）：
// accounts 类批量接口（查余额/保活）共用。fn 返回 actionResult（UID 由本函数补）。
func (h *Handler) runForTargets(body uidBody, action string, fn func(*pool.Account) actionResult) ([]actionResult, string) {
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
			res := fn(h.cfg.Pool.Get(uid))
			res.UID = uid
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results, summaryMsg(action, results)
}

// singleAction 单账号动作：uid 必填、账号存在性校验，enable/disable/clear 共用。
func (h *Handler) singleAction(w http.ResponseWriter, r *http.Request, action string, fn func(acct *pool.Account) (bool, string)) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	acct := h.cfg.Pool.Get(body.UID)
	if acct == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	ok, msg := fn(acct)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "message": msg})
}

// adminCredits 按家族分发：华为（无积分面）→ Validate/刷新状态；
// 腾讯 → 真实积分余额查询（SPEC §24.2 落地）。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	results, msg := h.runForTargets(body, "刷新状态", func(acct *pool.Account) actionResult {
		res := actionResult{}
		if acct == nil {
			res.Message = "no account"
			return res
		}
		if api, ok := acct.Client.(upstream.BillingAPI); ok && acct.ProfileID == "workbuddy" {
			if bal, err := api.UserResource(acct.Auth); err != nil {
				res.Message = err.Error()
			} else {
				acct.SetQuota(pool.AccountQuota{Remain: bal.Remain, Total: bal.Total, Used: bal.Used, UpdatedAt: time.Now().Unix()})
				res.OK = true
				res.Message = "积分余额 " + strconv.FormatInt(bal.Remain, 10) +
					"（套餐共 " + strconv.FormatInt(bal.Total, 10) + " · 已用 " + strconv.FormatInt(bal.Used, 10) + "）"
				res.Credits = bal.Remain
			}
			return res
		}
		if ok, verr := h.cfg.Pool.Validate(acct); verr != nil {
			res.Message = verr.Error()
		} else if !ok {
			res.Message = "token invalid"
		} else {
			res.OK = true
			res.Message = "ok"
		}
		return res
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "message": msg, "results": results,
	})
}

// adminKeepalive 手动触发 Validate：校验账号、token 临近过期自动续期、清除误禁用/冷却。
// 与调度器自动保活（心跳）不同——这是按需的手动刷新入口。
func (h *Handler) adminKeepalive(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	results, msg := h.runForTargets(body, "刷新状态", func(acct *pool.Account) actionResult {
		res := actionResult{}
		if acct == nil {
			res.Message = "no account"
			return res
		}
		if ok, verr := h.cfg.Pool.Validate(acct); verr != nil {
			res.Message = verr.Error()
		} else if !ok {
			res.Message = "token invalid"
		} else {
			res.OK = true
			res.Message = "已校验（token 有效）"
		}
		return res
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "message": msg, "results": results,
	})
}

// adminEnable / adminDisable / adminClearCooldown：单账号动作（singleAction）。
func (h *Handler) adminEnable(w http.ResponseWriter, r *http.Request) {
	h.singleAction(w, r, "启用", func(acct *pool.Account) (bool, string) {
		return h.cfg.Pool.Enable(acct.UID), "已启用 " + acct.UID
	})
}

func (h *Handler) adminDisable(w http.ResponseWriter, r *http.Request) {
	h.singleAction(w, r, "禁用", func(acct *pool.Account) (bool, string) {
		reason := "manual disable"
		if qr := r.URL.Query().Get("reason"); qr != "" {
			reason = qr
		}
		h.cfg.Pool.Disable(acct.UID, reason)
		return true, "已禁用 " + acct.UID
	})
}

func (h *Handler) adminClearCooldown(w http.ResponseWriter, r *http.Request) {
	h.singleAction(w, r, "清冷却", func(acct *pool.Account) (bool, string) {
		return h.cfg.Pool.ClearCooldown(acct.UID), "已清冷却 " + acct.UID
	})
}

// adminCheckin 无签到；映射为全员保活（refresh）。
func (h *Handler) adminCheckin(w http.ResponseWriter, r *http.Request) {
	h.adminKeepalive(w, r)
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
	total, healthy, _, _ := h.cfg.Pool.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "已重载 auths", "loaded": len(auths), "total": total, "healthy": healthy,
	})
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

// adminRoutesGet 当前路由表与禁用集（C1）。
func (h *Handler) adminRoutesGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"routes":  h.routesTable().Routes(),
		"blocked": h.routesTable().Blocked(),
	})
}

// adminRoutesPut 全量替换路由表：校验（家族已注册/表内唯一）→ 落盘 → 热生效。
// 校验失败 409 且保持当前表不变。
func (h *Handler) adminRoutesPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Routes  []adapt.ModelRoute `json:"routes"`
		Blocked []string           `json:"blocked"`
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
	table.SetBlocked(body.Blocked) // 全量语义：空列表清空全部禁用
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
	h.routesTable().SetBlocked(body.Blocked)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "routes": h.routesTable().Routes(), "blocked": h.routesTable().Blocked(),
	})
}

// adminModelsGet 模型全貌：?family=codearts|workbuddy → 该家族清单；
// 缺省 → 唯一视图（与 /v1/models 无渠道一致）。query 参数 refresh=1 → 同步等待
// 一次上游拉取（面板「刷新目录」按钮；缺省只读缓存，不阻塞）。
func (h *Handler) adminModelsGet(w http.ResponseWriter, r *http.Request) {
	fam := r.URL.Query().Get("family")
	if fam != "" {
		if h.profiles().Get(fam) == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown family"})
			return
		}
		cat := h.catalog(fam, adminWait(r))
		writeJSON(w, http.StatusOK, map[string]any{
			"family": cat.Family, "label": cat.Label, "source": cat.Source,
			"source_account": cat.SourceAccount, "source_realm": cat.SourceRealm,
			"realms": cat.Realms, "warming": cat.Warming, "error": cat.Error,
			"data": cat.Models,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": h.unifiedModelList()})
}

// adminWait 面板同步等待策略：refresh=1 → 等待上游；否则 0（只读缓存）。
func adminWait(r *http.Request) time.Duration {
	if r.URL.Query().Get("refresh") == "1" {
		return modelFetchPanelWait
	}
	return 0
}

// adminRoutesOverview 面板单请求数据源（SPEC §29.7）：两家族目录（含来源与失败
// 原因）+ 当前路由表 + 禁用集 + 内置默认表。面板据此渲染唯一主表；拆成多个
// 请求会让"渠道全貌/路由状态/默认值"三份数据在时间上不一致。
// 缺省只读缓存（打开面板必然秒开），refresh=1 才等待上游。
func (h *Handler) adminRoutesOverview(w http.ResponseWriter, r *http.Request) {
	wait := adminWait(r)
	cats := make([]familyCatalog, 0, 2)
	for _, fam := range []string{"codearts", "workbuddy"} {
		if h.profiles().Get(fam) == nil {
			continue
		}
		cats = append(cats, h.catalog(fam, wait))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"families": cats,
		"routes":   h.routesTable().Routes(),
		"blocked":  h.routesTable().Blocked(),
		"defaults": buildDefaultRoutes(),
	})
}

// adminModelsScan 逐账号模型扫描（面板「扫描各账号」按需触发，?family=）：
// 账号间可见模型集可能不同（付费/免费档差异），逐账号往返必要，故不做缺省拉取。
func (h *Handler) adminModelsScan(w http.ResponseWriter, r *http.Request) {
	fam := r.URL.Query().Get("family")
	if fam == "" {
		fam = "workbuddy"
	}
	if h.profiles().Get(fam) == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown family"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"family": fam, "label": familyLabel(fam), "accounts": h.scanModelAccounts(fam),
	})
}

// adminGrowth 腾讯成长中心状态（SPEC §32 观测面）：按账号拉取积分/能量/签到/任务/宠物。
// 只读调用，失败按字段暴露（活动面故障不得影响面板可用性）。
// 查询参数 uid 可只刷新单账号（面板行内刷新用）。
func (h *Handler) adminGrowth(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	rows := make([]map[string]any, 0, 4)
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct.ProfileID != "workbuddy" {
			continue
		}
		if uid != "" && acct.UID != uid {
			continue
		}
		api, ok := acct.Client.(upstream.BillingAPI)
		if !ok {
			continue
		}
		row := map[string]any{
			"uid":    acct.UID,
			"nick":   acct.UserName,
			"domain": acct.Auth.Domain,
		}
		if st, err := api.CheckinStatus(acct.Auth); err == nil && st != nil {
			row["checkin"] = map[string]any{
				"theme":            st.ThemeName,
				"active":           st.Active,
				"today_checked_in": st.TodayCheckedIn,
				"streak_days":      st.StreakDays,
				"total_credits":    st.TotalCredits,
				"daily_credit":     st.DailyCredit,
			}
		} else if err != nil {
			row["checkin_error"] = truncateText(err.Error(), 120)
		}
		if bal, err := api.UserResource(acct.Auth); err == nil && bal != nil {
			row["credits"] = bal.Remain
			row["credits_total"] = bal.Total
			row["credits_used"] = bal.Used
			// 顺带刷新账号池额度快照：面板「余额」列与成长中心口径保持一致。
			acct.SetQuota(pool.AccountQuota{Remain: bal.Remain, Total: bal.Total, Used: bal.Used, UpdatedAt: time.Now().Unix()})
		} else if err != nil {
			row["credits_error"] = truncateText(err.Error(), 120)
		}
		if q, err := api.PetQuota(acct.Auth); err == nil && q != nil {
			row["energy"] = q.Balance
		}
		if tasks, err := api.GrowthTasks(acct.Auth); err == nil {
			var total, claimed, completed, accepted int
			for _, t := range tasks {
				total++
				switch {
				case t.TaskStatus() == "claimed":
					claimed++
				case t.Completed():
					completed++
				default:
					accepted++
				}
			}
			row["tasks"] = map[string]any{
				"total": total, "claimed": claimed, "completed": completed, "accepted": accepted,
			}
		}
		if pet, err := api.PetTravelStatus(acct.Auth); err == nil && pet != nil {
			eta := pet.ArriveAt - pet.ServerNow
			if eta < 0 {
				eta = 0
			}
			state := pet.State
			if state == "" {
				state = "none" // 无宠物（全球版空 data 形态）
			}
			row["pet"] = map[string]any{
				"state": state, "location": pet.Location.Name, "arrive_in_min": eta / 60,
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": rows})
}
