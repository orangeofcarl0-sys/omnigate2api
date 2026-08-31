// 裸模型名路由表（SPEC §29）：模型名全局唯一 → 渠道（家族）映射。
// 客户端只发裸模型名，渠道完全由网关侧路由表决定；同名模型撞名在
// 加载/保存时 fail-fast，绝不静默回退。X-Provider 显式覆盖优先于表。
package adapt

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// ModelRoute 一条裸模型名 → 渠道（家族）路由。
type ModelRoute struct {
	Model  string `json:"model"`
	Family string `json:"family"`
}

// RouteTable 裸模型名路由表（线程安全；校验后替换）。
type RouteTable struct {
	mu     sync.RWMutex
	routes []ModelRoute
}

// NewRouteTable 构造并校验（fail-fast）；模型名统一小写归一（用户侧展示约定）。
func NewRouteTable(routes []ModelRoute) (*RouteTable, error) {
	t := &RouteTable{}
	if err := t.Replace(routes); err != nil {
		return nil, err
	}
	return t, nil
}

// validate 校验：模型名非空、全局唯一，家族非空（撞名/非法 = 拒绝）。
func validateRoutes(routes []ModelRoute) error {
	if len(routes) == 0 {
		return fmt.Errorf("route table is empty")
	}
	seen := map[string]string{}
	for _, r := range routes {
		if r.Model == "" {
			return fmt.Errorf("route entry with empty model")
		}
		if r.Family == "" {
			return fmt.Errorf("route %q: empty family", r.Model)
		}
		if prev, ok := seen[r.Model]; ok {
			return fmt.Errorf("duplicate model name %q (families %s and %s) — 撞名 fail-fast，需显式裁决或独立注册名", r.Model, prev, r.Family)
		}
		seen[r.Model] = r.Family
	}
	return nil
}

// Replace 全量替换（校验失败保持原表不变；模型名统一小写归一）。
func (t *RouteTable) Replace(routes []ModelRoute) error {
	for i := range routes {
		routes[i].Model = strings.ToLower(strings.TrimSpace(routes[i].Model))
	}
	if err := validateRoutes(routes); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.routes = append([]ModelRoute(nil), routes...)
	return nil
}

// FamilyOf 查表：模型名小写匹配，命中返回 (family, true)。
func (t *RouteTable) FamilyOf(model string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	key := strings.ToLower(strings.TrimSpace(model))
	for _, r := range t.routes {
		if r.Model == key {
			return r.Family, true
		}
	}
	return "", false
}

// Resolve 解析路由（SPEC §29.2）：explicit（X-Provider 显式覆盖）非空直接返回；
// 否则查表命中 → 表内家族；未命中 → 缺省 "codearts"。
func (t *RouteTable) Resolve(model, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if f, ok := t.FamilyOf(model); ok {
		return f
	}
	return "codearts"
}

// Routes 返回当前表副本（管理 API 展示）。
func (t *RouteTable) Routes() []ModelRoute {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]ModelRoute, len(t.routes))
	copy(out, t.routes)
	return out
}

// LoadRouteTableFile 从 JSON 文件加载（缺失 → nil；非法 → 错误，fail-fast）。
func LoadRouteTableFile(path string) (*RouteTable, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var routes []ModelRoute
	if err := json.Unmarshal(raw, &routes); err != nil {
		return nil, fmt.Errorf("parse routes %q: %w", path, err)
	}
	return NewRouteTable(routes)
}

// SaveRouteTableFile JSON 落盘（0600）。
func SaveRouteTableFile(path string, t *RouteTable) error {
	raw, err := json.MarshalIndent(t.Routes(), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
