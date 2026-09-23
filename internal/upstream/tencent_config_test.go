// 模型配置与促销判定（SPEC §29.7.2）：日期窗、每日时段窗（含跨零点）、
// 「仅徽章无 factor」不得误标免费、过期回落牌价、倍率解析。
package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func cfgWith(t *testing.T, raw string) *ModelConfig {
	t.Helper()
	var cfg ModelConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

const promoIntl = `{
  "models":[{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","credits":"x0.00"},
            {"id":"hy4-preview-f","name":"Hy4 preview","credits":"x0.00"},
            {"id":"gpt-6-astra","name":"GPT-6-Astra","credits":"x6.67 credits"}],
  "modelPromotions":[
    {"id":"deepseek-v4.1-flash","enabled":true,"kind":"discount","modelIds":["deepseek-v4.1-flash"],
     "badge":{"label":"Free now","color":"#FF0000"},
     "discount":{"factor":0,"discountedCredits":"0x","displayMode":"replace"},
     "schedule":{"timezone":"Asia/Shanghai","validFrom":"2026-08-06T00:00:00+08:00","validUntil":"2026-09-25T00:00:00+08:00"},
     "hover":{"textZh":"Sep 11 – Sep 25: Free daily use with unlimited credits.","textEn":"..."}},
    {"id":"hy4-f-free-trial-202608","enabled":true,"kind":"discount","modelIds":["hy4-preview-f"],
     "badge":{"label":"Free now"},
     "discount":{"factor":0},
     "schedule":{"validFrom":"2026-09-11T00:00:00+08:00","validUntil":"2026-10-10T00:00:00+08:00"}}
  ]}`

// 日期窗：窗口内 free，过期后回落 paid。
func TestPromotionDateWindow(t *testing.T) {
	cfg := cfgWith(t, promoIntl)
	inside := time.Date(2026, 9, 24, 12, 0, 0, 0, cstZone)
	kind, label, factor, window := cfg.EffectiveAccess("deepseek-v4.1-flash", inside)
	if kind != AccessFree || label != "Free now" || factor != 0 {
		t.Fatalf("inside window: kind=%s label=%s factor=%v", kind, label, factor)
	}
	if window == "" {
		t.Fatal("window text must be surfaced for the tooltip")
	}
	after := time.Date(2026, 9, 26, 12, 0, 0, 0, cstZone)
	if kind, _, _, _ := cfg.EffectiveAccess("deepseek-v4.1-flash", after); kind != AccessPaid {
		t.Fatalf("expired promo must fall back to paid, got %s", kind)
	}
}

// 每日时段窗（跨零点）：夜间 free，白天回落 paid。
func TestPromotionDailyWindowWrapsMidnight(t *testing.T) {
	cfg := cfgWith(t, `{
	  "models":[{"id":"hy4-preview","name":"Hy4 preview","credits":"x0.29"}],
	  "modelPromotions":[
	    {"id":"hy4-night","enabled":true,"modelIds":["hy4-preview"],"badge":{"label":"夜间免费"},
	     "discount":{"factor":0},
	     "schedule":{"daily":[{"start":"23:00","end":"23:59"},{"start":"0:00","end":"8:00"}]}}
	  ]}`)
	night := time.Date(2026, 9, 24, 2, 30, 0, 0, cstZone)
	if kind, label, _, _ := cfg.EffectiveAccess("hy4-preview", night); kind != AccessNightFree || label != "夜间免费" {
		t.Fatalf("night should be free: kind=%s label=%s", kind, label)
	}
	day := time.Date(2026, 9, 24, 12, 0, 0, 0, cstZone)
	if kind, _, _, _ := cfg.EffectiveAccess("hy4-preview", day); kind != AccessPaid {
		t.Fatalf("daytime must not be free, got %s", kind)
	}
}

// 单条 23:00–7:50 形式的跨零点窗口。
func TestPromotionDailyWindowSingleRangeWraps(t *testing.T) {
	cfg := cfgWith(t, `{
	  "models":[{"id":"glm-5.2","name":"GLM-5.2","credits":"x0.79"}],
	  "modelPromotions":[
	    {"id":"glm52-night","enabled":true,"modelIds":["glm-5.2"],"badge":{"label":"夜间折扣"},
	     "discount":{"factor":0.5},"schedule":{"daily":[{"start":"23:00","end":"7:50"}]}}
	  ]}`)
	if kind, _, f, _ := cfg.EffectiveAccess("glm-5.2", time.Date(2026, 9, 24, 3, 0, 0, 0, cstZone)); kind != AccessDiscount || f != 0.5 {
		t.Fatalf("wrapped night window: kind=%s factor=%v", kind, f)
	}
	if kind, _, _, _ := cfg.EffectiveAccess("glm-5.2", time.Date(2026, 9, 24, 12, 0, 0, 0, cstZone)); kind != AccessPaid {
		t.Fatalf("outside window must be paid, got %s", kind)
	}
}

// 白天"仅徽章"促销（无 factor）不得把夜间免费误标成免费——这是成对下发时的陷阱。
func TestPromotionBadgeOnlyIsNotDiscount(t *testing.T) {
	cfg := cfgWith(t, `{
	  "models":[{"id":"hy4-preview","name":"Hy4 preview","credits":"x0.29"}],
	  "modelPromotions":[
	    {"id":"night-real","enabled":true,"modelIds":["hy4-preview"],"badge":{"label":"夜间免费"},
	     "discount":{"factor":0},"schedule":{"daily":[{"start":"23:00","end":"8:00"}]}},
	    {"id":"daytime-badge","enabled":true,"modelIds":["hy4-preview"],"badge":{"label":"夜间免费"},
	     "discount":{"factor":null},"schedule":{"daily":[{"start":"8:00","end":"23:00"}]}}
	  ]}`)
	day := time.Date(2026, 9, 24, 14, 0, 0, 0, cstZone)
	if kind, _, _, _ := cfg.EffectiveAccess("hy4-preview", day); kind != AccessPaid {
		t.Fatalf("badge-only promo must not grant free: %s", kind)
	}
	night := time.Date(2026, 9, 24, 1, 0, 0, 0, cstZone)
	if kind, _, _, _ := cfg.EffectiveAccess("hy4-preview", night); kind != AccessNightFree {
		t.Fatalf("real night promo must apply: %s", kind)
	}
}

// 同模型多条折扣 → 取最优惠（factor 最小）；禁用项不参与。
func TestPromotionPicksBestFactor(t *testing.T) {
	half, zero := 0.5, 0.0
	cfg := &ModelConfig{
		Models: []ConfigModel{{ID: "m", Credits: "x0.79"}},
		Promotions: []ModelPromotion{
			{ID: "half", Enabled: true, ModelIDs: []string{"m"}, Discount: PromoDiscount{Factor: &half}, Badge: PromoBadge{Label: "五折"}},
			{ID: "free", Enabled: true, ModelIDs: []string{"m"}, Discount: PromoDiscount{Factor: &zero}, Badge: PromoBadge{Label: "免费"}},
			{ID: "disabled", Enabled: false, ModelIDs: []string{"m"}, Discount: PromoDiscount{Factor: &zero}, Badge: PromoBadge{Label: "不应生效"}},
		},
	}
	kind, label, factor, _ := cfg.EffectiveAccess("m", time.Now())
	if kind != AccessFree || label != "免费" || factor != 0 {
		t.Fatalf("must pick the best active promo: kind=%s label=%s factor=%v", kind, label, factor)
	}
}

// 模型不在配置清单里 → 查不到（调用方据此不臆造标注）。
func TestConfigFindAndMultiplier(t *testing.T) {
	cfg := cfgWith(t, promoIntl)
	if _, ok := cfg.Find("not-there"); ok {
		t.Fatal("unknown model must not be found")
	}
	m, ok := cfg.Find("GPT-6-Astra") // 大小写不敏感
	if !ok {
		t.Fatal("case-insensitive lookup failed")
	}
	if got := m.Multiplier(); got != "x6.67" {
		t.Fatalf("multiplier parse: %q", got)
	}
	if got := (ConfigModel{Credits: "x0.00"}).Multiplier(); got != "x0.00" {
		t.Fatalf("multiplier without suffix: %q", got)
	}
	if got := (ConfigModel{Credits: ""}).Multiplier(); got != "" {
		t.Fatalf("empty credits (alias tier) must stay empty: %q", got)
	}
}

// FetchConfig：envelope 解包 + 桌面 UA（CLI UA 的响应不含 promotions，故必须带桌面 UA）。
func TestFetchConfigEnvelope(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/config" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":` + promoIntl + `}`))
	}))
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	cfg, err := c.FetchConfig(billingAuth())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models) != 3 || len(cfg.Promotions) != 2 {
		t.Fatalf("parsed models=%d promotions=%d", len(cfg.Models), len(cfg.Promotions))
	}
	if gotUA != desktopUserAgent {
		t.Fatalf("must use desktop UA (promotions are UA-gated): %q", gotUA)
	}
}

// 业务码非 0 → 报错（不得静默当空配置）。
func TestFetchConfigBizError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":11000,"msg":"login expired"}`))
	}))
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	if _, err := c.FetchConfig(billingAuth()); err == nil {
		t.Fatal("business error must surface")
	}
}
