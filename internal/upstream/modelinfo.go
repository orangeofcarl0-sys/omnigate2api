// 模型目录元数据（SPEC §29.7）：访问类别与能力标记的单一归一入口。
//
// 上游只以自由文本标签表达「免费/付费」语义——腾讯目录的 tags 里是
// `badge:<中文标签>:#RRGGBB`（实测仅三种：夜间免费 / 限时免费 / 夜间折扣），
// 华为侧的活动（福利）模型则由福利网关清单单独下发。此处把两家的表达
// 收敛成同一组枚举，供面板与 /v1/models 消费；原始标签保留在 AccessLabel
// 里（展示以官方文案为准，不自造）。
package upstream

import "strings"

// 访问类别枚举（面板据此上色，语义见各注释）。
const (
	AccessPaid        = "paid"         // 无免费标注：按积分/额度计费
	AccessFree        = "free"         // 免费
	AccessNightFree   = "night_free"   // 夜间免费（时段性）
	AccessLimitedFree = "limited_free" // 限时免费（活动期）
	AccessDiscount    = "discount"     // 折扣（夜间折扣等）
	AccessBenefit     = "benefit"      // 福利网关活动模型（华为）
)

// accessLabelDefault 各类别的兜底中文标签（上游未给原文时使用）。
var accessLabelDefault = map[string]string{
	AccessPaid:        "按量计费",
	AccessFree:        "免费",
	AccessNightFree:   "夜间免费",
	AccessLimitedFree: "限时免费",
	AccessDiscount:    "折扣",
	AccessBenefit:     "福利额度",
}

// AccessLabel 类别兜底标签（上游标签缺失时的展示文案）。
func AccessLabel(kind string) string {
	if s, ok := accessLabelDefault[kind]; ok {
		return s
	}
	return kind
}

// ParseModelTags 归一腾讯目录 tags：
//   - `badge:*` → (访问类别, 官方标签原文)；多枚 badge 取第一枚（实测每模型至多一枚）；
//   - 其余标签 → 模式清单（如 craft/text-to-image），原样保留。
//
// 无 badge → AccessPaid（"无免费标注"即按量计费，不臆测免费）。
func ParseModelTags(tags []string) (kind, label string, modes []string) {
	kind = AccessPaid
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		rest, ok := strings.CutPrefix(t, "badge:")
		if !ok {
			modes = append(modes, t)
			continue
		}
		// badge:<标签>[:#RRGGBB]——标签本身可能含冒号，按末段是否颜色值切分。
		text := rest
		if i := strings.LastIndex(rest, ":"); i > 0 && strings.HasPrefix(rest[i+1:], "#") {
			text = rest[:i]
		}
		text = strings.TrimSpace(text)
		if label == "" { // 首枚 badge 决定类别与展示文案
			kind, label = classifyBadge(text), text
		}
	}
	if label == "" {
		label = AccessLabel(kind)
	}
	return kind, label, modes
}

// classifyBadge 按关键词归类官方 badge 文案（未识别 → paid，标签原文照旧展示）。
func classifyBadge(text string) string {
	switch {
	case strings.Contains(text, "免费"):
		switch {
		case strings.Contains(text, "夜间"), strings.Contains(text, "夜猫"):
			return AccessNightFree
		case strings.Contains(text, "限时"):
			return AccessLimitedFree
		default:
			return AccessFree
		}
	case strings.Contains(text, "折扣"), strings.Contains(text, "优惠"):
		return AccessDiscount
	default:
		return AccessPaid
	}
}

// AccessForStatic 无目录元数据时的访问类别（华为静态表与福利模型）：
// 福利网关下发的活动模型 → benefit，其余按量计费。
func AccessForStatic(model string) (kind, label string) {
	if IsBenefitModel(model) {
		return AccessBenefit, AccessLabel(AccessBenefit)
	}
	return AccessPaid, AccessLabel(AccessPaid)
}
