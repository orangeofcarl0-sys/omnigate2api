// /v1/models 端点：动态模型列表（缓存 1h + 失败负冷却），失败回退静态表。
package server

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// modelCache 单家族模型缓存：正缓存 1h + 失败负冷却 5min。
type modelCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

var (
	dynamicModelsCache = &modelCache{}
	tencentModelsCache = &modelCache{}
)

var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "codearts", "context_length": 202752},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "codearts", "context_length": 202752},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "codearts", "context_length": 131072},
	{"id": "qwen3-vl-235b", "object": "model", "created": 1753600000, "owned_by": "codearts", "context_length": 131072},
	// 活动（福利）模型：注册名来自 MaaS 福利网关 config（小写 ID 原样透传），
	// 请求需带 maas_type: benefit 头（见 upstream.SendChatV2）。
	{"id": "glm-5.3-flash", "object": "model", "created": 1753600000, "owned_by": "codearts", "context_length": 1048576, "max_output_tokens": 131072},
	{"id": "deepseek-v4-flash-0731", "object": "model", "created": 1753600000, "owned_by": "codearts", "context_length": 1048576, "max_output_tokens": 393216},
	{"id": "deepseek-v4-pro-0813", "object": "model", "created": 1753600000, "owned_by": "codearts", "context_length": 1048576, "max_output_tokens": 393216},
}

// staticTencentModels 腾讯家族静态回落表（参考实现缺省模型，owned_by=workbuddy；
// 动态清单成功时优先动态，SPEC §28.4 决策 B）。
var staticTencentModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "minimax-m3-pay", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：按请求 X-Provider 分家族（§28.4 决策 B），
// 优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	family := "codearts"
	if id := r.Header.Get("X-Provider"); id == "workbuddy" {
		family = "workbuddy"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelListFor(family),
	})
}

// modelList 华为家族动态模型列表（默认 /v1/models 行为，兼容既有调用）。
func (h *Handler) modelList() []map[string]any {
	return h.modelListFor("codearts")
}

// modelListFor 按家族组装模型列表：动态优先 + 静态表补漏；动态失败全线回落
// 静态表。
func (h *Handler) modelListFor(family string) []map[string]any {
	cache, static, owner := dynamicModelsCache, staticModels, "codearts"
	if family == "workbuddy" {
		cache, static, owner = tencentModelsCache, staticTencentModels, "workbuddy"
	}
	if infos := h.fetchModels(cache, family); len(infos) > 0 {
		return mergeStatic(infos, static, owner)
	}
	return static
}

// mergeStatic 动态清单 + 静态表补漏（账号实际可用但不在代理列表中的模型）。
func mergeStatic(infos []upstream.ModelInfo, static []map[string]any, ownedBy string) []map[string]any {
	out := make([]map[string]any, 0, len(infos)+len(static))
	seen := map[string]bool{}
	for _, mi := range infos {
		// 展示统一用用户侧小写 ID（glm-5.2），与 CanonicalModel 映射一致。
		displayID := strings.ToLower(mi.ID)
		seen[displayID] = true
		entry := map[string]any{
			"id":                displayID,
			"object":            "model",
			"created":           1753600000,
			"owned_by":          ownedBy,
			"context_length":    mi.ContextWindow,
			"max_output_tokens": mi.MaxTokens,
		}
		if mi.ContextWindow == 0 {
			entry["context_length"] = 131072 // 兜底
		}
		out = append(out, entry)
	}
	for _, sm := range static {
		id, _ := sm["id"].(string)
		if id != "" && !seen[id] {
			out = append(out, sm)
		}
	}
	return out
}

// fetchModels 从池中任一家族健康账号拉模型清单（华为 agent-center / 腾讯
// /console/enterprises/personal/models 取 agent "cli"），缓存 1h + 负冷却 5min。
func (h *Handler) fetchModels(c *modelCache, family string) []upstream.ModelInfo {
	c.RLock()
	if len(c.ids) > 0 && time.Since(c.fetched) < dynamicModelsTTL {
		out := c.ids
		c.RUnlock()
		return out
	}
	if !c.lastFail.IsZero() && time.Since(c.lastFail) < modelsFetchFailCooldown {
		c.RUnlock()
		return nil
	}
	c.RUnlock()

	var get func(acct *pool.Account) ([]upstream.ModelInfo, error)
	if family == "workbuddy" {
		get = func(acct *pool.Account) ([]upstream.ModelInfo, error) {
			lister, ok := acct.Client.(upstream.ModelLister)
			if !ok {
				return nil, nil // 客户端不支持清单接口：按失败处理（负缓存）
			}
			return lister.FetchModels(acct.Auth)
		}
	} else {
		get = func(acct *pool.Account) ([]upstream.ModelInfo, error) {
			return h.cfg.Upstream.FetchModels(acct.Auth)
		}
	}
	acct := h.cfg.Pool.PickFor(family, nil)
	if acct == nil {
		return nil
	}
	infos, err := get(acct)
	if err != nil || len(infos) == 0 {
		c.Lock()
		c.lastFail = time.Now()
		c.Unlock()
		return nil
	}
	c.Lock()
	c.ids = infos
	c.fetched = time.Now()
	c.lastFail = time.Time{} // 成功则清空负缓存
	c.Unlock()
	return infos
}
