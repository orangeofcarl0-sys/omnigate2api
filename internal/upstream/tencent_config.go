// 腾讯模型配置（GET {base}/v3/config）：牌价倍率 + 结构化促销（SPEC §29.7.2）。
//
// 这是「付费/免费/折扣」的**权威来源**，取代早期从目录 tags 里的 `badge:<文案>`
// 猜文案的做法（原文案只给了中文标签，既无倍率也无时间窗，且按 UA 分流：
// CLI UA 的 config 根本不带 modelPromotions，只有桌面 UA 才有）。
//
// 实测形状（2026-09-24，CN/全球双域）：
//
//	{data:{models:[{id,credits:"x0.79 credits",...}],
//	       modelPromotions:[{id,modelIds,badge:{label,color},kind:"discount",
//	         discount:{factor,discountedCredits,displayMode},
//	         schedule:{timezone,validFrom,validUntil,daily:[{start,end}]},
//	         hover:{textZh,textEn},enabled,priority}]}}
//
// 两类窗口并存：validFrom/validUntil 是日期窗（国际版免费促销都是这种），
// schedule.daily 是每日时段窗（国内夜间免费/夜间折扣是这种）。
// 同一模型常成对出现「夜间折扣 + 白天徽章」两条促销：后者只有 badge、**没有
// factor**，仅用于白天展示文案，不代表折扣 —— 判定必须认 factor，不能只认 badge。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"omnigate2api/internal/auth"
)

// ConfigAPI 模型配置接口（腾讯客户端实现；华为无此端点）。
type ConfigAPI interface {
	FetchConfig(acct *auth.Auth) (*ModelConfig, error)
}

// ConfigModel 配置里的单模型条目（只需 id/名称/牌价倍率）。
type ConfigModel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Credits   string   `json:"credits"` // "x0.79 credits" / "x0.00" / ""（别名档，倍率浮动）
	Tags      []string `json:"tags"`
	Vendor    string   `json:"vendor"`
	IsDefault bool     `json:"isDefault"`
}

// PromoDaily 每日时段窗（本地时区见 PromoSchedule.Timezone）。
type PromoDaily struct {
	Start string `json:"start"` // "7:50" / "23:00"
	End   string `json:"end"`   // "7:50" / "23:59"
}

// PromoSchedule 促销时间窗：日期窗（validFrom/validUntil）与/或每日时段窗（daily）。
type PromoSchedule struct {
	Timezone   string       `json:"timezone"`
	ValidFrom  string       `json:"validFrom"`
	ValidUntil string       `json:"validUntil"`
	Daily      []PromoDaily `json:"daily"`
}

// PromoDiscount 折扣：factor 为倍率因子（0=免费，0.5=五折）；缺省表示"仅徽章"。
type PromoDiscount struct {
	Factor            *float64 `json:"factor"`
	DiscountedCredits string   `json:"discountedCredits"`
	DisplayMode       string   `json:"displayMode"`
}

// PromoBadge 徽章文案（国际版 "Free now"，国内 "限时免费"/"夜间免费"/"夜间折扣"）。
type PromoBadge struct {
	Label string `json:"label"`
	Color string `json:"color"`
}

// PromoHover 悬停文案（含活动期自然语言说明）。
type PromoHover struct {
	TextZh string `json:"textZh"`
	TextEn string `json:"textEn"`
}

// ModelPromotion 一条模型促销。
type ModelPromotion struct {
	ID       string        `json:"id"`
	Kind     string        `json:"kind"`
	Enabled  bool          `json:"enabled"`
	Priority int           `json:"priority"`
	ModelIDs []string      `json:"modelIds"`
	Badge    PromoBadge    `json:"badge"`
	Discount PromoDiscount `json:"discount"`
	Schedule PromoSchedule `json:"schedule"`
	Hover    PromoHover    `json:"hover"`
}

// ModelConfig 模型配置（models 用于查牌价倍率；modelPromotions 用于查促销）。
type ModelConfig struct {
	Models     []ConfigModel    `json:"models"`
	Promotions []ModelPromotion `json:"modelPromotions"`
	FetchedAt  time.Time        `json:"-"`
}

// FetchConfig 拉取模型配置（桌面 UA：CLI UA 的响应不含 modelPromotions）。
func (c *TencentClient) FetchConfig(acct *auth.Auth) (*ModelConfig, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for model config")
	}
	base, _ := c.resolve(acct.Domain)
	raw, status, err := c.tencentDo(acct, tencentHTTPOpts{
		base: base, method: http.MethodGet, path: "/v3/config", desktopUA: true,
	})
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int64           `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("config parse: %w", uerr)
	}
	if err := checkBiz("v3-config", status, env.Code, env.Msg); err != nil {
		return nil, err
	}
	body := raw
	if len(env.Data) > 0 { // 兼容 envelope 与平铺两种形态
		body = env.Data
	}
	var cfg ModelConfig
	if uerr := json.Unmarshal(body, &cfg); uerr != nil {
		return nil, fmt.Errorf("config parse: %w", uerr)
	}
	if len(cfg.Models) == 0 {
		return nil, fmt.Errorf("v3-config returned empty model list")
	}
	cfg.FetchedAt = time.Now()
	return &cfg, nil
}

// Find 按 ID 查配置里的模型条目（大小写不敏感；配置清单只用于贴标签）。
func (cfg *ModelConfig) Find(modelID string) (ConfigModel, bool) {
	for _, m := range cfg.Models {
		if strings.EqualFold(m.ID, modelID) {
			return m, true
		}
	}
	return ConfigModel{}, false
}

// Multiplier 牌价倍率展示串（"x0.79"）；空 credits（别名档，倍率浮动）返回 ""。
func (m ConfigModel) Multiplier() string {
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m.Credits), "credits"))
	return strings.TrimSpace(s)
}

// EffectiveAccess 某一时刻该模型的生效访问类别：
//
//	kind  free | night_free | limited_free | discount | paid
//	label 官方徽章文案（无促销时为空）
//	factor 生效折扣因子（-1 表示无折扣）
//	window 生效窗口的可读说明（供面板 tooltip）
//
// 判定规则（按优先级）：enabled 且**当前处于有效期**（日期窗 ∩ 每日时段窗）且
// **带 factor 且 factor<1** 的促销里，取 factor 最小者（最优惠）——只带 badge 的
// "白天徽章"条目因无 factor 被排除，不会把白天的夜间免费误标成免费。
// 无生效促销 → paid（牌价倍率由调用方另取）。
func (cfg *ModelConfig) EffectiveAccess(modelID string, now time.Time) (kind, label string, factor float64, window string) {
	best := -1.0
	local := now.In(cstZone)
	for i := range cfg.Promotions {
		p := &cfg.Promotions[i]
		if !p.Enabled || !p.HasModel(modelID) {
			continue
		}
		f := p.Discount.Factor
		if f == nil || *f >= 1 {
			continue // 无 factor（仅展示徽章）或非折扣
		}
		if !p.Active(local) {
			continue
		}
		if best < 0 || *f < best {
			best, kind, label, window = *f, kindForFactor(*f, p.Badge.Label), p.Badge.Label, p.WindowText()
		}
	}
	if best < 0 {
		return AccessPaid, "", -1, ""
	}
	return kind, label, best, window
}

// HasModel 该促销是否覆盖指定模型。
func (p *ModelPromotion) HasModel(modelID string) bool {
	for _, id := range p.ModelIDs {
		if strings.EqualFold(strings.TrimSpace(id), strings.TrimSpace(modelID)) {
			return true
		}
	}
	return false
}

// Active 当前是否落在有效期（日期窗 ∧ 每日时段窗；未声明的维度视为不限）。
func (p *ModelPromotion) Active(nowLocal time.Time) bool {
	s := p.Schedule
	if s.ValidFrom != "" && nowLocal.Before(parsePromoTime(s.ValidFrom)) {
		return false
	}
	if s.ValidUntil != "" && !nowLocal.Before(parsePromoTime(s.ValidUntil)) {
		return false
	}
	if len(s.Daily) == 0 {
		return true
	}
	cur := nowLocal.Hour()*60 + nowLocal.Minute()
	for _, d := range s.Daily {
		start, ok1 := parseHHMM(d.Start)
		end, ok2 := parseHHMM(d.End)
		if !ok1 || !ok2 {
			continue
		}
		if start <= end {
			if cur >= start && cur <= end {
				return true
			}
			continue
		}
		if cur >= start || cur <= end { // 跨零点（如 23:00–7:50）
			return true
		}
	}
	return false
}

// WindowText 窗口的可读说明（面板 tooltip）：日期窗 + 每日时段窗。
func (p *ModelPromotion) WindowText() string {
	s := p.Schedule
	var parts []string
	if s.ValidFrom != "" || s.ValidUntil != "" {
		from, until := "…", "…"
		if t := parsePromoTime(s.ValidFrom); !t.IsZero() {
			from = t.Format("2006-01-02")
		}
		if t := parsePromoTime(s.ValidUntil); !t.IsZero() {
			until = t.Format("2006-01-02")
		}
		parts = append(parts, from+" ~ "+until)
	}
	if len(s.Daily) > 0 {
		var ds []string
		for _, d := range s.Daily {
			ds = append(ds, d.Start+"–"+d.End)
		}
		parts = append(parts, "每日 "+strings.Join(ds, "、"))
	}
	txt := strings.Join(parts, " · ")
	if h := firstNonEmpty(p.Hover.TextZh, p.Hover.TextEn); h != "" {
		if txt != "" {
			txt += " ｜ "
		}
		txt += h
	}
	return txt
}

// kindForFactor 因子 + 徽章文案 → 访问类别枚举（复用 §29.7.2 的分类词表）。
func kindForFactor(factor float64, label string) string {
	if factor <= 0 {
		if k := classifyBadge(label); k != AccessPaid {
			return k // 夜间免费 / 限时免费 / 免费
		}
		return AccessFree
	}
	return AccessDiscount
}

// parsePromoTime 解析促销时间戳（RFC3339 带偏移；解析失败返回零值）。
func parsePromoTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.In(cstZone)
		}
	}
	return time.Time{}
}

// parseHHMM 解析 "H:MM"/"HH:MM" → 当日分钟数。
func parseHHMM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	hi, err1 := strconv.Atoi(strings.TrimSpace(h))
	mi, err2 := strconv.Atoi(strings.TrimSpace(m))
	if err1 != nil || err2 != nil || hi < 0 || hi > 24 || mi < 0 || mi > 59 {
		return 0, false
	}
	return hi*60 + mi, true
}
