// 裸模型名路由（SPEC §29）：默认表组装、handler 接入辅助、/v1/models 唯一视图。
package server

import (
	"log"
	"strings"

	"omnigate2api/internal/adapt"
)

// buildDefaultRoutes 内置默认路由表（SPEC §29.1）：
//   - 华为静态清单（含活动模型）全量注册给 codearts；
//   - 腾讯静态清单中与华为撞名的模型（glm-5.2/glm-5.1/deepseek-v4-flash）
//     由 codearts 持有裸名（保持现状缺省语义），其余固有模型注册给 workbuddy；
//   - 运行时动态模型（如腾讯动态清单新增）未命中表 → 缺省 codearts。
func buildDefaultRoutes() []adapt.ModelRoute {
	codearts := map[string]bool{}
	var routes []adapt.ModelRoute
	for _, m := range staticModels {
		if id, ok := m["id"].(string); ok && id != "" {
			codearts[id] = true
			routes = append(routes, adapt.ModelRoute{Model: id, Family: "codearts"})
		}
	}
	for _, m := range staticTencentModels {
		id, _ := m["id"].(string)
		if id == "" || codearts[id] {
			continue // 撞名组：codearts 持有裸名（fail-fast 保证唯一）
		}
		routes = append(routes, adapt.ModelRoute{Model: id, Family: "workbuddy"})
	}
	return routes
}

// routesTable 返回路由表：cfg.Routes（文件加载）或内置默认表
// （NewHandler 已固化到 h.routes，见 handler.go）。
func (h *Handler) routesTable() *adapt.RouteTable { return h.routes }

// resolveProfile 显式渠道与裸模型名路由解析（SPEC §29.2）：
// explicit 非空且已注册 → 直接该家族；否则按模型名查路由表；未命中 → codearts。
func (h *Handler) resolveProfile(explicit, model string) *adapt.UpstreamProfile {
	if explicit != "" {
		if p := h.profiles().Get(explicit); p != nil {
			return p
		}
		log.Printf("provider %q not registered, falling back to route table", explicit)
	}
	fam := h.routesTable().Resolve(model, "")
	if p := h.profiles().Get(fam); p != nil {
		return p
	}
	return &adapt.Codearts
}

// unifiedModelList /v1/models 唯一视图（无显式渠道，SPEC §29.3）：
// 两家族清单按 family 标注并入，再按路由表顺序输出——模型名全局唯一；
// 表内模型在家族清单缺失时按缺省 context 兜底展示。
func (h *Handler) unifiedModelList() []map[string]any {
	byID := map[string]map[string]any{}
	for _, fam := range []string{"codearts", "workbuddy"} {
		for _, e := range h.modelListFor(fam) {
			if id, ok := e["id"].(string); ok && id != "" {
				// 撞名模型由路由表裁决的家族持有裸名：其它家族清单里出现
				// 同名模型时跳过（否则后遍历的家族会覆盖归属）。
				if owned, ok := h.routesTable().FamilyOf(id); ok && owned != fam {
					continue
				}
				e["family"] = fam
				byID[strings.ToLower(id)] = e
			}
		}
	}
	out := make([]map[string]any, 0, len(h.routesTable().Routes()))
	for _, r := range h.routesTable().Routes() {
		e, ok := byID[strings.ToLower(r.Model)]
		if !ok {
			e = map[string]any{
				"id": r.Model, "object": "model", "created": 1753600000,
				"family": r.Family, "context_length": 131072,
			}
		}
		out = append(out, e)
	}
	return out
}
