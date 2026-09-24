// 模型目录（SPEC §29.7）：/v1/models 端点、家族静态回落表、面板/管理视图组装。
//
// 数据来源两层：动态清单（上游实时拉取，缓存 1h）与静态回落表（上游不可达时
// 的面板可用性保证）。条目携带 source 字段，面板据此标注数据是否为回落值——
// 静态表只保证"能看见"，不保证与上游一致，绝不静默冒充实时数据。
package server

import (
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// ── 家族与来源元信息 ──────────────────────────────────────────────────────

// familyLabels 家族展示名（面板/管理 API 共用）。
var familyLabels = map[string]string{
	"codearts":  "华为 codearts",
	"workbuddy": "腾讯 workbuddy",
}

const (
	sourceLive   = "live"   // 上游实时清单
	sourceStatic = "static" // 静态回落表
)

// ── 静态回落表 ────────────────────────────────────────────────────────────

// staticModelSpec 静态条目（typed：字段增删由编译器兜底，避免 map 字面量漂移）。
type staticModelSpec struct {
	id, name, vendor                 string
	access, accessLabel, description string
	context, maxOut                  int64
	images, tools, isDefault         bool
	modes                            []string
}

// entry 转为对外条目（/v1/models 与面板共用形状）。
func (s staticModelSpec) entry(family string) map[string]any {
	e := map[string]any{
		"id": s.id, "object": "model", "created": modelCreated,
		"owned_by": family, "family": family,
		"name": s.name, "source": sourceStatic,
		"access": s.access, "access_label": s.accessLabel,
	}
	if s.context > 0 {
		e["context_length"] = s.context
	}
	if s.maxOut > 0 {
		e["max_output_tokens"] = s.maxOut
	}
	if s.vendor != "" {
		e["vendor"] = s.vendor
	}
	if s.description != "" {
		e["description"] = s.description
	}
	if len(s.modes) > 0 {
		e["modes"] = s.modes
	}
	if s.images {
		e["supports_images"] = true
	}
	if s.tools {
		e["supports_tools"] = true
	}
	if s.isDefault {
		e["is_default"] = true
	}
	return e
}

const modelCreated = 1753600000

// staticModels 华为家族静态回落表（MaaS 注册名；活动/福利模型需 maas_type: benefit
// 头，见 upstream.SendChatV2）。access 由 upstream.AccessForStatic 判定语义一致。
var staticModels = []staticModelSpec{
	{id: "glm-5.2", access: upstream.AccessPaid, context: 202752, maxOut: 131072, images: true, tools: true},
	{id: "glm-5.1", access: upstream.AccessPaid, context: 202752, images: true, tools: true},
	{id: "deepseek-v4-flash", access: upstream.AccessPaid, context: 131072, images: true, tools: true},
	{id: "qwen3-vl-235b", access: upstream.AccessPaid, context: 131072, images: true, tools: true},
	{id: "glm-5.3-flash", access: upstream.AccessBenefit, context: 1048576, maxOut: 131072, images: true, tools: true},
	{id: "deepseek-v4-flash-0731", access: upstream.AccessBenefit, context: 1048576, maxOut: 393216, images: true, tools: true},
	{id: "deepseek-v4-pro-0813", access: upstream.AccessBenefit, context: 1048576, maxOut: 393216, images: true, tools: true},
}

// staticTencentModels workbuddy 家族静态回落表：取自 CN 账号实测的 cli 代理绑定
// 清单（16 条，2026-09 实证）。旧表（8 条）是参考实现遗留，已失效——它宣称的
// minimax-m3-pay/deepseek-v4-flash 在实测清单中并不存在，且漏掉 10 个真实模型；
// minimax-m3-pay 作为付费档变体保留（付费账号可见，实测付费/免费档清单不同，
// 面板「扫描各账号」可看各账号真实可见集）。
var staticTencentModels = []staticModelSpec{
	{id: "auto", name: "Auto", vendor: "f", access: upstream.AccessPaid, context: 256000, maxOut: 32000, images: true, tools: true, isDefault: true, modes: []string{"craft"}},
	{id: "glm-5.3", name: "GLM-5.3", vendor: "e", access: upstream.AccessPaid, context: 1000000, maxOut: 64000, images: true, tools: true, modes: []string{"craft"}},
	{id: "glm-5.3-flash", name: "GLM-5.3-Flash", vendor: "f", access: upstream.AccessPaid, context: 1000000, maxOut: 131072, images: true, tools: true},
	{id: "glm-5.2", name: "GLM-5.2", vendor: "e", access: upstream.AccessDiscount, accessLabel: "夜间折扣", context: 1000000, maxOut: 64000, images: true, tools: true, modes: []string{"craft"}},
	{id: "glm-5.1", name: "GLM-5.1", vendor: "e", access: upstream.AccessPaid, context: 200000, maxOut: 48000, tools: true},
	{id: "glm-5v-turbo", name: "GLM-5v-Turbo", vendor: "e", access: upstream.AccessPaid, context: 200000, maxOut: 64000, images: true, tools: true, modes: []string{"craft"}},
	{id: "hy4-preview", name: "Hy4 preview", vendor: "j", access: upstream.AccessNightFree, accessLabel: "夜间免费", context: 1000000, maxOut: 64000, images: true, tools: true, modes: []string{"craft"}},
	{id: "hy3", name: "Hy3", vendor: "j", access: upstream.AccessLimitedFree, accessLabel: "限时免费", context: 192000, maxOut: 64000, images: true, tools: true, modes: []string{"craft"}},
	{id: "hy3-x", name: "Hy3", vendor: "j", access: upstream.AccessPaid, context: 192000, maxOut: 64000, images: true, tools: true, modes: []string{"craft"}},
	{id: "deepseek-v4.1-flash", name: "Deepseek-V4.1-Flash", vendor: "f", access: upstream.AccessPaid, context: 1000000, maxOut: 128000, images: true, tools: true, modes: []string{"craft"}},
	{id: "deepseek-v4-pro", name: "Deepseek-V4-Pro", vendor: "f", access: upstream.AccessPaid, context: 1000000, maxOut: 128000, images: true, tools: true},
	{id: "kimi-k3-1", name: "Kimi-K3", vendor: "f", access: upstream.AccessPaid, context: 1000000, maxOut: 32000, images: true, tools: true},
	{id: "kimi-k2.8-preview", name: "Kimi-K2.8-Preview", vendor: "f", access: upstream.AccessPaid, context: 1000000, maxOut: 64000, images: true, tools: true},
	{id: "kimi-k2.7", name: "Kimi-K2.7-Code", vendor: "f", access: upstream.AccessPaid, context: 256000, maxOut: 32000, images: true, tools: true},
	{id: "kimi-k2.6", name: "Kimi-K2.6", vendor: "f", access: upstream.AccessPaid, context: 256000, maxOut: 32000, images: true, tools: true, modes: []string{"craft"}},
	{id: "minimax-m3", name: "MiniMax-M3", vendor: "f", access: upstream.AccessPaid, context: 512000, maxOut: 64000, images: true, tools: true},
	{id: "minimax-m3-pay", name: "MiniMax-M3 (付费档)", vendor: "f", access: upstream.AccessPaid, context: 512000, maxOut: 64000, images: true, tools: true},
}

// staticFor 家族静态表。
func staticFor(family string) []staticModelSpec {
	if family == "workbuddy" {
		return staticTencentModels
	}
	return staticModels
}

// staticIDs 家族静态表模型名（默认路由表组装用）。
func staticIDs(models []staticModelSpec) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.id)
	}
	return out
}

// ── 动态清单缓存 ──────────────────────────────────────────────────────────

// modelCache 单家族模型缓存：正缓存 1h + 失败负冷却 5min + 单飞刷新。
type modelCache struct {
	sync.RWMutex
	ids           []upstream.ModelInfo
	fetched       time.Time
	lastFail      time.Time
	lastErrMsg    string
	inflight      bool                 // 单飞：并发调用只触发一次上游拉取
	lastGood      string               // 上次成功的账号名（优先序，避开已知不可达账号）
	lastGoodRealm string               // 上次成功账号的区域（面板标注元数据来源用）
	badAt         map[string]time.Time // 账号 → 最近一次清单拉取失败时刻（降序用）
}

var (
	dynamicModelsCache = &modelCache{}
	tencentModelsCache = &modelCache{}
)

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
	// 单账号清单拉取等待上限：某账号端点不可达（如全球域网络不通）时放弃转下一
	// 账号——上游客户端的传输层超时不该成为面板/客户端的等待时间。后台刷新路径
	// 与面板打开解耦，故此处可以从容（成功账号会被记入 lastGood 优先序）。
	modelFetchAttemptTimeout = 25 * time.Second
	// 同步等待上限：面板「刷新目录」/ 客户端 /v1/models。超时即返回当前可得
	// 数据（静态表），后台拉取完成后写入缓存。
	modelFetchPanelWait  = 15 * time.Second
	modelFetchClientWait = 12 * time.Second
	// 清单拉取失败账号的降序窗口：窗口内该账号排到健康账号之后，避免每次刷新
	// 都在已知不可达的账号上耗掉一个等待窗口（只降序，不剔除）。
	modelFetchBadAccountTTL = 5 * time.Minute
)

func cacheFor(family string) *modelCache {
	if family == "workbuddy" {
		return tencentModelsCache
	}
	return dynamicModelsCache
}

// ── 家族目录组装 ──────────────────────────────────────────────────────────

// familyCatalog 家族模型目录（含来源与失败原因，面板/管理 API 共用）。
//
// SourceAccount/SourceRealm 不可省：目录元数据（含付费/免费标注）只能从**某一个
// 账号**拉到，而同一家族可能横跨区域（CN + 全球）——区域间模型清单与计费标注
// 未必一致（全球域的目录端点实测返回 500，根本读不到）。把来源标出来，面板才
// 不会把 CN 账号的标注冒充成全家族事实。
type familyCatalog struct {
	Family        string                    `json:"family"`
	Label         string                    `json:"label"`
	Source        string                    `json:"source"` // live | static
	SourceAccount string                    `json:"source_account,omitempty"`
	SourceRealm   string                    `json:"source_realm,omitempty"`
	Realms        []string                  `json:"realms,omitempty"`        // 家族账号覆盖的区域集合
	AccessConfig  map[string]map[string]any `json:"access_config,omitempty"` // 各区域配置背书状态
	Error         string                    `json:"error,omitempty"`
	Warming       bool                      `json:"warming,omitempty"` // 后台刷新进行中（当前为回落数据）
	Models        []map[string]any          `json:"models"`
}

// catalog 组装家族目录：动态优先（附静态补漏），动态不可用时全线回落静态表
// 并带上失败原因（面板显示"回落"提示，不冒充实时）。
// wait <= 0 → 绝不等待上游：仅用缓存，冷缓存时后台拉起刷新后立即返回静态表
// （面板打开不该被上游延迟绑架）；wait > 0 → 最多等待该时长。
func (h *Handler) catalog(family string, wait time.Duration) familyCatalog {
	c := familyCatalog{
		Family: family, Label: familyLabel(family), Source: sourceStatic,
		Realms: h.familyRealms(family), AccessConfig: promoStatusOf(),
	}
	cache := cacheFor(family)
	infos := h.modelsCached(cache)
	if infos == nil && wait > 0 {
		infos = h.waitModels(cache, family, wait)
	}
	if wait > 0 {
		// 手动刷新（面板「刷新目录」/ ?refresh=1）：一并重试标注配置——模型缓存
		// 尚热时 refreshModels 会走早退分支，不重试的话失败过的区域要等满 TTL。
		go h.refreshPromos(h.modelAccountCandidates(family))
	}
	if infos == nil && wait <= 0 {
		cache.RLock()
		warming := cache.inflight
		cache.RUnlock()
		if !warming {
			warming = h.startRefresh(cache, family) // 冷缓存：后台预热，下次即为实时
		}
		c.Warming = warming
	}
	if len(infos) > 0 {
		c.Source = sourceLive
		cache.RLock()
		c.SourceAccount, c.SourceRealm = cache.lastGood, cache.lastGoodRealm
		cache.RUnlock()
		c.Models = mergeStatic(infos, staticFor(family), family)
		h.overlayAccess(c.Models, c.SourceRealm)
		return c
	}
	cache.RLock()
	c.Error = cache.lastErrMsg
	cache.RUnlock()
	c.Models = staticEntries(family)
	return c
}

// familyRealms 家族账号覆盖的区域集合（cn|global，去重排序）。
func (h *Handler) familyRealms(family string) []string {
	seen := map[string]bool{}
	for _, a := range h.cfg.Pool.Accounts() {
		if a.ProfileID != family {
			continue
		}
		seen[regionOf(a.Auth)] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// modelsCached 读缓存（新鲜才返回，不触发拉取）。
func (h *Handler) modelsCached(c *modelCache) []upstream.ModelInfo {
	c.RLock()
	defer c.RUnlock()
	if len(c.ids) > 0 && time.Since(c.fetched) < dynamicModelsTTL {
		return c.ids
	}
	return nil
}

// waitModels 同步等待一次拉取（负冷却期内直接返回 nil）。
func (h *Handler) waitModels(c *modelCache, family string, wait time.Duration) []upstream.ModelInfo {
	c.RLock()
	cooling := !c.lastFail.IsZero() && time.Since(c.lastFail) < modelsFetchFailCooldown
	c.RUnlock()
	if cooling {
		return nil
	}
	done := make(chan []upstream.ModelInfo, 1)
	go func() { done <- h.refreshModels(c, family) }()
	select {
	case infos := <-done:
		return infos
	case <-time.After(wait):
		return nil // 后台继续，结果写入缓存
	}
}

// startRefresh 后台触发一次拉取（不阻塞调用方）。返回是否确实发起了拉取。
// inflight 标记由 refreshModels 自己 claim——此处只判断"没人拉就拉起"，
// 否则先置位再拉起会让被拉起的 goroutine 把自己当成重复请求（自我阻塞）。
func (h *Handler) startRefresh(c *modelCache, family string) bool {
	c.Lock()
	if c.inflight || len(c.ids) > 0 {
		c.Unlock()
		return false
	}
	if !c.lastFail.IsZero() && time.Since(c.lastFail) < modelsFetchFailCooldown {
		c.Unlock()
		return false
	}
	c.Unlock()
	go h.refreshModels(c, family)
	return true
}

// waitForWarm 等待缓存转热（最多 wait）；用于已有拉取在飞时的并发调用方。
func (h *Handler) waitForWarm(c *modelCache, wait time.Duration) []upstream.ModelInfo {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if infos := h.modelsCached(c); infos != nil {
			return infos
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// familyLabel 家族展示名（未注册家族回退原 id）。
func familyLabel(family string) string {
	if l, ok := familyLabels[family]; ok {
		return l
	}
	return family
}

// staticEntries 静态表条目。
func staticEntries(family string) []map[string]any {
	specs := staticFor(family)
	out := make([]map[string]any, 0, len(specs))
	for _, s := range specs {
		e := s.entry(family)
		if s.accessLabel == "" {
			e["access_label"] = upstream.AccessLabel(s.access)
		}
		out = append(out, e)
	}
	return out
}

// mergeStatic 动态清单 + 静态表补漏（账号实际可用但不在代理列表中的模型）。
func mergeStatic(infos []upstream.ModelInfo, static []staticModelSpec, family string) []map[string]any {
	out := make([]map[string]any, 0, len(infos)+len(static))
	seen := map[string]bool{}
	for _, mi := range infos {
		// 展示统一用用户侧小写 ID（glm-5.2），与 CanonicalModel 映射一致。
		id := strings.ToLower(mi.ID)
		seen[id] = true
		out = append(out, infoEntry(family, mi, id))
	}
	for _, s := range static {
		if seen[s.id] {
			continue
		}
		e := s.entry(family)
		if s.accessLabel == "" {
			e["access_label"] = upstream.AccessLabel(s.access)
		}
		out = append(out, e)
	}
	return out
}

// infoEntry 动态条目 → 对外条目（保留全部目录元数据）。
func infoEntry(family string, mi upstream.ModelInfo, id string) map[string]any {
	name := mi.Name
	if name == "" {
		name = mi.ID
	}
	access, label := mi.Access, mi.AccessLabel
	if access == "" {
		access, label = upstream.AccessForStatic(id)
	}
	if label == "" {
		label = upstream.AccessLabel(access)
	}
	e := map[string]any{
		"id": id, "object": "model", "created": modelCreated,
		"owned_by": family, "family": family,
		"name": name, "source": sourceLive,
		"access": access, "access_label": label,
		"context_length": mi.ContextWindow,
	}
	if e["context_length"] == int64(0) {
		e["context_length"] = 131072 // 兜底
	}
	if mi.MaxTokens > 0 {
		e["max_output_tokens"] = mi.MaxTokens
	}
	if mi.Vendor != "" {
		e["vendor"] = mi.Vendor
	}
	if len(mi.Modes) > 0 {
		e["modes"] = mi.Modes
	}
	if mi.Description != "" {
		e["description"] = mi.Description
	}
	if mi.SupportsImages {
		e["supports_images"] = true
	}
	if mi.SupportsTools {
		e["supports_tools"] = true
	}
	if mi.IsDefault {
		e["is_default"] = true
	}
	return e
}

// ── 端点 ──────────────────────────────────────────────────────────────────

// models 返回模型列表（SPEC §29.3）：X-Provider 显式家族 → 该家族全量清单；
// 无渠道标记 → 唯一视图（按路由表，模型名全局唯一 + family 字段）。
//
// 协议分流（标准供应商等价面）：Anthropic 客户端（带 `anthropic-version`，SDK 恒发）
// 期望的信封与 OpenAI 不同（`data[].type/display_name/created_at` + has_more/first_id/
// last_id），同一路径必须按客户端协议给出对应形状，否则 Anthropic 侧的模型自动发现直接解析失败。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	if isAnthropicCaller(r) {
		h.anthropicModels(w, r)
		return
	}
	if fam := r.Header.Get("X-Provider"); fam != "" {
		if h.profiles().Get(fam) != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"object": "list",
				"data":   h.modelListFor(fam),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.unifiedModelList(),
	})
}

// isAnthropicCaller 判定调用方是否走 Anthropic 协议：`anthropic-version` 是 Anthropic
// SDK/CLI 的必发头（OpenAI 系客户端不会带）。
func isAnthropicCaller(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("anthropic-version")) != ""
}

// findModelEntry 在唯一视图里按 id 查条目（大小写不敏感；与请求解析同一约定）。
func (h *Handler) findModelEntry(id string) (map[string]any, bool) {
	id = strings.TrimSpace(id)
	for _, e := range h.unifiedModelList() {
		if cur, ok := e["id"].(string); ok && strings.EqualFold(cur, id) {
			return e, true
		}
	}
	return nil, false
}

// modelRetrieve GET /v1/models/{id}（标准「检索单个模型」）：OpenAI 形状直返条目，
// Anthropic 调用方走 Anthropic 形状；未注册 → 各自协议的错误信封 + 404
// （OpenAI `model_not_found` 与 SPEC §29 C1 的禁用语义一致）。
func (h *Handler) modelRetrieve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entry, ok := h.findModelEntry(id)
	if !ok {
		if isAnthropicCaller(r) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "not_found_error", "message": "model not found: " + id},
			})
			return
		}
		writeOpenAIError(w, http.StatusNotFound, "model_not_found", "model not found: "+id)
		return
	}
	if isAnthropicCaller(r) {
		writeJSON(w, http.StatusOK, anthropicModelEntry(entry))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// anthropicModelEntry 唯一视图条目 → Anthropic 模型对象（只放标准字段：
// Anthropic SDK 对形状较严，额外字段不保证被容忍）。
func anthropicModelEntry(e map[string]any) map[string]any {
	id, _ := e["id"].(string)
	display, _ := e["name"].(string)
	if display == "" {
		display = id
	}
	return map[string]any{
		"type": "model", "id": id, "display_name": display,
		"created_at": modelCreatedAt(e),
	}
}

// modelCreatedAt 条目 created（unix 秒）→ RFC3339（Anthropic 的 created_at 形状）。
func modelCreatedAt(e map[string]any) string {
	sec, ok := e["created"].(int)
	if !ok {
		if f, ok2 := e["created"].(int64); ok2 {
			sec = int(f)
		}
	}
	if sec == 0 {
		sec = modelCreated
	}
	return time.Unix(int64(sec), 0).UTC().Format(time.RFC3339)
}

// anthropicModels Anthropic 形状的模型清单：`limit`（默认 20，1..1000）+
// `after_id`/`before_id` 游标（Anthropic 语义：取该 id 之后/之前的条目）。
func (h *Handler) anthropicModels(w http.ResponseWriter, r *http.Request) {
	all := h.unifiedModelList()
	ids := make([]string, 0, len(all))
	byID := make(map[string]map[string]any, len(all))
	for _, e := range all {
		if id, ok := e["id"].(string); ok && id != "" {
			ids = append(ids, id)
			byID[strings.ToLower(id)] = e
		}
	}
	limit := 20
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"type": "error",
				"error": map[string]any{"type": "invalid_request_error",
					"message": "limit must be an integer between 1 and 1000"},
			})
			return
		}
		limit = n
	}
	start, end := 0, len(ids)
	if after := strings.TrimSpace(r.URL.Query().Get("after_id")); after != "" {
		idx := indexOfFold(ids, after)
		if idx < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"type": "error",
				"error": map[string]any{"type": "invalid_request_error",
					"message": "after_id not found: " + after},
			})
			return
		}
		start = idx + 1
	}
	if before := strings.TrimSpace(r.URL.Query().Get("before_id")); before != "" {
		idx := indexOfFold(ids, before)
		if idx < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"type": "error",
				"error": map[string]any{"type": "invalid_request_error",
					"message": "before_id not found: " + before},
			})
			return
		}
		end = idx
	}
	if start > end {
		start = end
	}
	window := ids[start:end]
	hasMore := len(window) > limit
	if hasMore {
		window = window[:limit]
	}
	data := make([]map[string]any, 0, len(window))
	for _, id := range window {
		data = append(data, anthropicModelEntry(byID[strings.ToLower(id)]))
	}
	out := map[string]any{"data": data, "has_more": hasMore,
		"first_id": nil, "last_id": nil}
	if len(window) > 0 {
		out["first_id"] = window[0]
		out["last_id"] = window[len(window)-1]
	}
	writeJSON(w, http.StatusOK, out)
}

// indexOfFold 大小写不敏感查下标（未命中 -1）。
func indexOfFold(list []string, want string) int {
	for i, s := range list {
		if strings.EqualFold(s, want) {
			return i
		}
	}
	return -1
}

// modelListFor 按家族组装模型列表（客户端 /v1/models 视图：有限等待上游）。
func (h *Handler) modelListFor(family string) []map[string]any {
	return h.catalog(family, modelFetchClientWait).Models
}

// modelAccountCandidates 家族账号候选序（上次成功账号优先 → 健康账号 → 近期
// 拉取失败账号 → 其余）：单账号故障（如全球域网络不可达）不得否定整个家族的
// 清单，也不该每次刷新都让同一账号在最前面耗掉一个等待窗口。近期失败账号只降序
// 不剔除——其余账号都不可用时它仍是唯一机会。
func (h *Handler) modelAccountCandidates(family string) []*pool.Account {
	cached := cacheFor(family)
	cached.RLock()
	lastGood := cached.lastGood
	bad := make(map[string]time.Time, len(cached.badAt))
	for k, v := range cached.badAt {
		bad[k] = v
	}
	cached.RUnlock()

	badRecently := func(name string) bool {
		t, ok := bad[name]
		return ok && time.Since(t) < modelFetchBadAccountTTL
	}
	var preferred, healthy, recentBad, other []*pool.Account
	for _, a := range h.cfg.Pool.Accounts() {
		if a.ProfileID != family {
			continue
		}
		switch {
		case a.Name == lastGood:
			preferred = append(preferred, a)
		case badRecently(a.Name):
			recentBad = append(recentBad, a)
		case h.cfg.Pool.Healthy(a.Name):
			healthy = append(healthy, a)
		default:
			other = append(other, a)
		}
	}
	return append(append(append(preferred, healthy...), recentBad...), other...)
}

// fetchModelsFrom 单账号拉取家族清单（客户端不支持清单接口 → 按失败处理）。
func (h *Handler) fetchModelsFrom(acct *pool.Account, family string) ([]upstream.ModelInfo, error) {
	if family == "workbuddy" {
		lister, ok := acct.Client.(upstream.ModelLister)
		if !ok {
			return nil, errNoModelLister
		}
		return lister.FetchModels(acct.Auth)
	}
	return h.cfg.Upstream.FetchModels(acct.Auth)
}

// attemptResult 单账号拉取结果。
type attemptResult struct {
	infos []upstream.ModelInfo
	err   error
}

// fetchModelsFromBounded 单账号拉取（等待上限 modelFetchAttemptTimeout）：
// 超时按失败处理并转下一账号——上游客户端的传输层超时（120s）属于上游内部
// 约定，不该成为面板/客户端的等待时间。被放弃的 goroutine 自行结束。
func (h *Handler) fetchModelsFromBounded(acct *pool.Account, family string) attemptResult {
	done := make(chan attemptResult, 1)
	go func() {
		infos, err := h.fetchModelsFrom(acct, family)
		done <- attemptResult{infos, err}
	}()
	select {
	case r := <-done:
		return r
	case <-time.After(modelFetchAttemptTimeout):
		return attemptResult{nil, errModelFetchTimeout}
	}
}

// refreshModels 拉取家族清单（缓存 1h）：单飞 + 逐账号回退；只有全部候选失败才
// 计入负冷却，失败原因留在缓存里供面板展示（fail-fast 可见，不静默降级）。
func (h *Handler) refreshModels(c *modelCache, family string) []upstream.ModelInfo {
	c.Lock()
	if len(c.ids) > 0 && time.Since(c.fetched) < dynamicModelsTTL {
		out := c.ids
		c.Unlock()
		return out
	}
	if c.inflight {
		// 已有拉取在飞：等其结果（绝不再发起第二个请求）。
		c.Unlock()
		return h.waitForWarm(c, modelFetchAttemptTimeout)
	}
	if !c.lastFail.IsZero() && time.Since(c.lastFail) < modelsFetchFailCooldown {
		c.Unlock()
		return nil
	}
	c.inflight = true
	c.Unlock()
	defer func() {
		c.Lock()
		c.inflight = false
		c.Unlock()
	}()

	candidates := h.modelAccountCandidates(family)
	if len(candidates) == 0 {
		c.Lock()
		c.lastErrMsg = "no " + family + " account in pool"
		c.Unlock()
		return nil
	}
	failures := make([]string, 0, len(candidates))
	for _, acct := range candidates {
		r := h.fetchModelsFromBounded(acct, family)
		if r.err == nil && len(r.infos) > 0 {
			c.Lock()
			c.ids = r.infos
			c.fetched = time.Now()
			c.lastFail = time.Time{}
			c.lastErrMsg = ""
			c.lastGood = acct.Name
			c.lastGoodRealm = regionOf(acct.Auth)
			delete(c.badAt, acct.Name)
			c.Unlock()
			go h.refreshPromos(candidates) // 加料：异步拉配置，绝不拖住目录刷新
			return r.infos
		}
		msg := "empty list"
		if r.err != nil {
			msg = r.err.Error()
		}
		c.Lock()
		if c.badAt == nil {
			c.badAt = map[string]time.Time{}
		}
		c.badAt[acct.Name] = time.Now()
		c.Unlock()
		failures = append(failures, shortUID(acct.UID)+": "+truncateText(msg, 120))
	}
	log.Printf("models fetch failed family=%s attempts=%d: %s", family, len(candidates), strings.Join(failures, " | "))
	c.Lock()
	c.lastFail = time.Now()
	c.lastErrMsg = strings.Join(failures, " | ")
	c.Unlock()
	return nil
}

// scanModelAccounts 逐账号拉取家族清单（面板「扫描各账号」按需触发）：账号间清单
// 可能不同（实测付费/免费档可见模型集不同），单账号往返不可省。并发上限 4，
// 结果按账号逐条暴露失败。
func (h *Handler) scanModelAccounts(family string) []map[string]any {
	accounts := h.modelAccountCandidates(family)
	out := make([]map[string]any, len(accounts))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, acct := range accounts {
		wg.Add(1)
		go func(i int, acct *pool.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := map[string]any{
				"account": acct.Name, "uid": acct.UID, "nick": acct.UserName,
				"domain": domainOf(acct.Auth), "region": regionOf(acct.Auth),
			}
			infos, err := h.fetchModelsFrom(acct, family)
			if err != nil || len(infos) == 0 {
				row["ok"] = false
				row["error"] = truncateText(errText(err), 160)
			} else {
				models := make([]map[string]any, 0, len(infos))
				for _, mi := range infos {
					models = append(models, infoEntry(family, mi, strings.ToLower(mi.ID)))
				}
				row["ok"] = true
				row["models"] = models
			}
			out[i] = row
		}(i, acct)
	}
	wg.Wait()
	return out
}

// domainOf / regionOf 账号域信息（面板展示与区域区分）。
func domainOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return a.Domain
}

func regionOf(a *auth.Auth) string {
	if d := domainOf(a); strings.HasSuffix(d, ".workbuddy.ai") {
		return "global"
	}
	return "cn"
}

// errNoModelLister 客户端未实现清单接口（按拉取失败处理，不静默给空清单）。
var errNoModelLister = errors.New("client does not implement model listing")

// errNoConfigAPI 客户端未实现模型配置接口（华为无 /v3/config；调用方已过滤，此处兜底）。
var errNoConfigAPI = errors.New("client does not implement model config")

// errModelFetchTimeout 单账号清单拉取超时（转下一账号，见 fetchModelsFromBounded）。
var errModelFetchTimeout = errors.New("model fetch timeout")

// shortUID 账号标识缩写（日志展示；完整值见 overview/扫描结果）。
func shortUID(uid string) string {
	if len(uid) <= 14 {
		return uid
	}
	return uid[:8] + "…" + uid[len(uid)-4:]
}

// errText 错误文案（nil → 空串）。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ── 模型配置（/v3/config）按区域缓存：付费/免费/折扣标注的权威来源 ──────────
//
// 早期标注来自目录 tags 的中文徽章文案（无倍率、无时间窗，且按 UA 分流拿不到
// 促销结构）；现改为读 /v3/config 的 modelPromotions（桌面 UA），按**账号区域**
// 分别缓存——同一模型在不同区域的促销可能不同（dsv41f：国内按量计费 / 国际限免）。
// 该配置里的模型清单**只用于给已在目录中的模型贴标签，绝不新增条目**。
type promoStore struct {
	sync.RWMutex
	cfgs     map[string]*upstream.ModelConfig // 成功后缓存（TTL 1h）
	at       map[string]time.Time             // 上次成功时刻
	errs     map[string]string                // 上次失败原因（面板可读）
	errAt    map[string]time.Time             // 上次失败时刻（5min 负冷却）
	inflight map[string]bool                  // 单飞：同一区域不并发重复拉取
}

var modelPromos = &promoStore{
	cfgs: map[string]*upstream.ModelConfig{}, at: map[string]time.Time{},
	errs: map[string]string{}, errAt: map[string]time.Time{}, inflight: map[string]bool{},
}

// promoClaim 领取某区域的拉取权（成功缓存 TTL 1h；失败负冷却 5min；单飞）。
func promoClaim(realm string) bool {
	modelPromos.Lock()
	defer modelPromos.Unlock()
	now := time.Now()
	if modelPromos.cfgs[realm] != nil && now.Sub(modelPromos.at[realm]) < dynamicModelsTTL {
		return false
	}
	if modelPromos.inflight[realm] {
		return false
	}
	if t, ok := modelPromos.errAt[realm]; ok && now.Sub(t) < modelsFetchFailCooldown {
		return false
	}
	modelPromos.inflight[realm] = true
	return true
}

// promoFinish 落定一次拉取结果（成功覆盖缓存并清错；失败记原因与负冷却）。
func promoFinish(realm string, cfg *upstream.ModelConfig, err error) {
	modelPromos.Lock()
	defer modelPromos.Unlock()
	delete(modelPromos.inflight, realm)
	if err != nil || cfg == nil {
		modelPromos.errAt[realm] = time.Now()
		if err != nil {
			modelPromos.errs[realm] = errText(err)
		} else {
			modelPromos.errs[realm] = "empty config"
		}
		return
	}
	modelPromos.cfgs[realm] = cfg
	modelPromos.at[realm] = time.Now()
	delete(modelPromos.errs, realm)
}

// promoReset 测试用：清空缓存。
func promoReset() {
	modelPromos.Lock()
	defer modelPromos.Unlock()
	modelPromos.cfgs = map[string]*upstream.ModelConfig{}
	modelPromos.at = map[string]time.Time{}
	modelPromos.errs = map[string]string{}
	modelPromos.errAt = map[string]time.Time{}
	modelPromos.inflight = map[string]bool{}
}

// refreshPromos 按区域拉取模型配置（best-effort：失败只记原因，不影响目录）。
// 只对腾讯家族有意义（/v3/config 是腾讯端点，华为客户端未实现 ConfigAPI）。
// 由调用方以 goroutine 发起：配置是**加料**，绝不能让目录刷新等它。
func (h *Handler) refreshPromos(candidates []*pool.Account) {
	// 先筛出真正支持 /v3/config 的账号：华为客户端不实现该接口，若把它也算进候选，
	// 它会先抢到 realm=cn 再以"不支持"收场，给 cn 打上 5min 负冷却——腾讯 CN 的配置
	// 就被一起饿死（国际版反而正常，症状是"只有全球有标注"）。这类"不支持的家族
	// 污染共享 realm 缓存"的耦合必须在此切断，而不是靠调用方挑对家族。
	eligible := make([]*pool.Account, 0, len(candidates))
	for _, acct := range candidates {
		if _, ok := acct.Client.(upstream.ConfigAPI); ok {
			eligible = append(eligible, acct)
		}
	}
	if len(eligible) == 0 {
		return
	}
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for _, acct := range eligible {
		realm := regionOf(acct.Auth)
		if seen[realm] || !promoClaim(realm) {
			continue
		}
		seen[realm] = true
		wg.Add(1)
		// 区域之间**并行**拉取：不同区域是独立上游，串行会让某个区域（如本机到
		// www.workbuddy.ai 时通时断）把另一个区域一起拖住——曾实测"全球在拉、国内
		// 一直没标"就是这个串行阻塞造成的。
		go func(acct *pool.Account, realm string) {
			defer wg.Done()
			cfg, err := fetchConfigBounded(acct)
			if err != nil {
				log.Printf("v3-config fetch failed realm=%s account=%s: %v", realm, shortUID(acct.UID), err)
			}
			promoFinish(realm, cfg, err)
		}(acct, realm)
	}
	wg.Wait()
}

// fetchConfigBounded 单区域配置拉取（等待上限同模型目录的单账号上限）。
// 上游客户端的传输层超时（120s）不该让某个区域的不可达变成整次刷新的等待时间。
func fetchConfigBounded(acct *pool.Account) (*upstream.ModelConfig, error) {
	api, ok := acct.Client.(upstream.ConfigAPI)
	if !ok {
		return nil, errNoConfigAPI
	}
	type res struct {
		cfg *upstream.ModelConfig
		err error
	}
	done := make(chan res, 1)
	go func() {
		cfg, err := api.FetchConfig(acct.Auth)
		done <- res{cfg, err}
	}()
	select {
	case r := <-done:
		return r.cfg, r.err
	case <-time.After(modelFetchAttemptTimeout):
		return nil, errModelFetchTimeout
	}
}

// promoStatusOf 各区域配置的可用状态（面板自证标注来源：哪些区域有配置背书）。
func promoStatusOf() map[string]map[string]any {
	modelPromos.RLock()
	defer modelPromos.RUnlock()
	out := map[string]map[string]any{}
	for _, realm := range []string{"cn", "global"} {
		st := map[string]any{"ok": modelPromos.cfgs[realm] != nil}
		if t := modelPromos.at[realm]; !t.IsZero() {
			st["at"] = t.Format(time.RFC3339)
		}
		if e := modelPromos.errs[realm]; e != "" {
			st["error"] = e
		}
		if modelPromos.inflight[realm] {
			st["fetching"] = true
		}
		out[realm] = st
	}
	return out
}

// realmAccess 该模型在各区域的生效访问标注（只含**配置里确实有该模型**的区域）。
func (h *Handler) realmAccess(modelID string, now time.Time) map[string]map[string]any {
	out := map[string]map[string]any{}
	modelPromos.RLock()
	cfgs := make(map[string]*upstream.ModelConfig, len(modelPromos.cfgs))
	for r, c := range modelPromos.cfgs {
		cfgs[r] = c
	}
	modelPromos.RUnlock()

	for realm, cfg := range cfgs {
		if cfg == nil {
			continue
		}
		cm, ok := cfg.Find(modelID)
		if !ok {
			continue // 该区域的清单里没有这个模型：不臆造标注
		}
		kind, label, factor, window := cfg.EffectiveAccess(modelID, now)
		if label == "" {
			label = upstream.AccessLabel(kind)
		}
		e := map[string]any{
			"access": kind, "access_label": label, "multiplier": cm.Multiplier(),
		}
		if window != "" {
			e["window"] = window
		}
		if factor >= 0 {
			e["factor"] = factor
		}
		out[realm] = e
	}
	return out
}

// overlayAccess 用促销/倍率覆盖目录条目的访问标注（按区域）。只贴标签不增删条目。
func (h *Handler) overlayAccess(models []map[string]any, sourceRealm string) {
	if len(models) == 0 {
		return
	}
	now := time.Now()
	for _, e := range models {
		id, _ := e["id"].(string)
		if id == "" {
			continue
		}
		ra := h.realmAccess(strings.ToLower(id), now)
		if len(ra) == 0 {
			continue
		}
		primary := ra[sourceRealm]
		if primary == nil {
			// 无来源区域（静态回落路径）时按字典序取，避免 map 遍历顺序不确定
			// 导致同一份数据每次刷新主标注在 CN/全球之间跳变。
			keys := make([]string, 0, len(ra))
			for k := range ra {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			primary = ra[keys[0]]
		}
		if v, ok := primary["access"]; ok {
			e["access"] = v
		}
		if v, ok := primary["access_label"]; ok {
			e["access_label"] = v
		}
		if v, ok := primary["multiplier"]; ok {
			e["multiplier"] = v
		}
		if v, ok := primary["window"]; ok {
			e["window"] = v
		}
		e["access_source"] = "v3-config"
		if len(ra) > 1 {
			e["access_by_realm"] = ra
		}
	}
}
