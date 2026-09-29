// Token 用量统计与白嫖金额换算（SPEC §34）：聚合/持久化往返、区域化牌价、
// 免费判定（促销 credit=0 / 华为福利模型）与 admin API 契约。
package server

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestUsageStatsSnapshotMoney(t *testing.T) {
	u := NewUsageStats("", nil) // 内存态；默认牌价 0.014/0.03/7.15
	// 国内积分抵扣：7.9 积分 × ¥0.014
	u.Record("workbuddy", "cn", "glm-5.2", 1000, 500, 200, 7.9, false, false)
	// 全球免费促销（credit=0）→ 免费直用
	u.Record("workbuddy", "global", "hy3", 2000, 1000, 0, 0, true, false)
	// 华为福利模型 → 免费直用
	u.Record("codearts", "huawei", "glm-5.3-flash", 3000, 500, 0, 0, true, false)
	// 华为非福利模型 → 真实计费，不算白嫖
	u.Record("codearts", "huawei", "glm-5.2", 3000, 500, 0, 0, false, false)

	snap := u.Snapshot()
	if snap.Total.Credits < 7.89 || snap.Total.Credits > 7.91 {
		t.Fatalf("credits=%v", snap.Total.Credits)
	}
	// 积分抵扣金额只来自国内行（华为 credit=0、全球 credit=0）
	if !almost(snap.Total.CreditsCNY, 7.9*0.014) {
		t.Fatalf("credits_cny=%v", snap.Total.CreditsCNY)
	}
	// 免费直用 token = (2000+1000) + (3000+500)，默认未配置单价 → free_cny=0 且 free_priced=false
	if snap.Total.FreeTokens != 6500 || snap.Total.FreeRequests != 2 {
		t.Fatalf("free=%+v", snap.Total)
	}
	if snap.Total.FreeCNY != 0 || snap.Total.FreePriced {
		t.Fatalf("unpriced free must not fabricate money: %+v", snap.Total)
	}
	// 按模型行：免费判定与区域化金额
	byModel := map[string]UsageModelRow{}
	for _, m := range snap.Models {
		byModel[m.Family+"|"+m.Realm+"|"+m.Model] = m
	}
	// 默认未配置免费单价：免费直用行只报 token 数，金额为 0 且不标已定价
	if r := byModel["workbuddy|global|hy3"]; r.Today.FreePriced || r.Today.FreeRequests != 1 ||
		r.Total.FreeTokens != 3000 || r.Total.Credits != 0 || r.Total.CreditsCNY != 0 {
		t.Fatalf("hy3 row=%+v", r)
	}
	if r := byModel["codearts|huawei|glm-5.3-flash"]; r.Total.FreeTokens != 3500 {
		t.Fatalf("benefit model row=%+v", r)
	}
	if r := byModel["codearts|huawei|glm-5.2"]; r.Total.FreeRequests != 0 {
		t.Fatalf("non-benefit huawei must not be free: %+v", r)
	}
	if r := byModel["workbuddy|cn|glm-5.2"]; !almost(r.Total.CreditsCNY, 7.9*0.014) {
		t.Fatalf("cn credit money=%v", r.Total.CreditsCNY)
	}
}

func TestUsageStatsPricingOverrideAndFreePrice(t *testing.T) {
	u := NewUsageStats("", &Pricing{CreditCNY: 0.02, CreditUSD: 0.04, USDCNY: 7.0, FreeCNYPerMToken: 1.5})
	u.Record("workbuddy", "cn", "glm-5.2", 0, 0, 0, 10, false, false)       // 10 × 0.02 = 0.2
	u.Record("workbuddy", "global", "hy3", 2_000_000, 0, 0, 0, true, false) // 2M × 1.5 = 3
	snap := u.Snapshot()
	if !almost(snap.Total.CreditsCNY, 0.2) {
		t.Fatalf("credit override=%v", snap.Total.CreditsCNY)
	}
	if !snap.Total.FreePriced || !almost(snap.Total.FreeCNY, 3.0) {
		t.Fatalf("free pricing=%+v", snap.Total)
	}
}

func TestUsageStatsPersistRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.json")
	u := NewUsageStats(file, nil)
	u.Record("workbuddy", "cn", "glm-5.2", 100, 50, 10, 1.5, false, false)
	u.Flush()

	u2 := NewUsageStats(file, nil)
	snap := u2.Snapshot()
	if snap.Total.InputTokens != 100 || snap.Total.OutputTokens != 50 || snap.Total.CachedTokens != 10 {
		t.Fatalf("roundtrip lost tokens: %+v", snap.Total)
	}
	if !almost(snap.Total.Credits, 1.5) {
		t.Fatalf("roundtrip credits=%v", snap.Total.Credits)
	}
}

func TestIsBenefitModel(t *testing.T) {
	for model, want := range map[string]bool{
		"glm-5.3-flash":          true, // 福利
		"deepseek-v4-flash-0731": true, // 福利
		"deepseek-v4-pro-0813":   true, // 福利
		"glm-5.2":                false,
		"kimi-k2.7":              false,
		"no-such-model":          false,
	} {
		if got := isBenefitModel(model); got != want {
			t.Fatalf("isBenefitModel(%s)=%v want %v", model, got, want)
		}
	}
}

// promoFree：按区域促销缓存判定免费（付费模型 credit 取整为 0 也不误判）。
func TestPromoFree(t *testing.T) {
	// 窗口**相对当前时间**构造（±1 天）：写死日期会变成定时炸弹——2026-09-30 零点一过，
	// "hy3 免费"就不再成立，测试在真实时间下无故变红（实测：09-30 00:46 起失败）。
	from := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	until := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	raw := `{"models":[{"id":"hy3","credits":"x0.00"},{"id":"glm-5.3","credits":"x0.79"}],
	  "modelPromotions":[{"id":"hy3-free","enabled":true,"kind":"discount","modelIds":["hy3"],
	    "badge":{"label":"Free now"},"discount":{"factor":0,"discountedCredits":"0x","displayMode":"replace"},
	    "schedule":{"timezone":"Asia/Shanghai","validFrom":"` + from + `","validUntil":"` + until + `"}}]}`
	var cfg upstream.ModelConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	saved := modelPromos.cfgs["cn"]
	modelPromos.cfgs["cn"] = &cfg
	defer func() {
		if saved == nil {
			delete(modelPromos.cfgs, "cn")
		} else {
			modelPromos.cfgs["cn"] = saved
		}
	}()
	now := time.Now()
	if free, ok := promoFree("cn", "hy3", now); !ok || !free {
		t.Fatalf("hy3 must be free: free=%v ok=%v", free, ok)
	}
	if free, ok := promoFree("cn", "glm-5.3", now); !ok || free {
		t.Fatalf("paid model must not be free: free=%v ok=%v", free, ok)
	}
	if free, ok := promoFree("cn", "no-such", now); ok || free {
		t.Fatalf("unknown model must be ok=false: free=%v ok=%v", free, ok)
	}
	if _, ok := promoFree("global", "hy3", now); ok {
		t.Fatalf("cold realm must be ok=false")
	}
}

// 免费直用按牌价倍率折算（正常 API 花费）；全球倍率被促销烘成 0 时回落国内牌价。
func TestUsageFreeMoneyByMultiplier(t *testing.T) {
	mk := func(raw string) *upstream.ModelConfig {
		var c upstream.ModelConfig
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		return &c
	}
	global := mk(`{"models":[{"id":"hy3","credits":"x0.79"},{"id":"dsv41f","credits":"x0.00"}]}`)
	cn := mk(`{"models":[{"id":"dsv41f","credits":"x0.11"}]}`)
	savedG, savedC := modelPromos.cfgs["global"], modelPromos.cfgs["cn"]
	modelPromos.cfgs["global"], modelPromos.cfgs["cn"] = global, cn
	defer func() {
		modelPromos.cfgs["global"], modelPromos.cfgs["cn"] = savedG, savedC
	}()
	// 全球 hy3（x0.79）：1000 token → 0.79 × 0.105 = 0.08295 积分 × $0.03 × 7.15
	u := NewUsageStats("", nil)
	cny, priced := u.freeMoney("global", "hy3", 1000)
	if !priced || !almost(cny, 0.79*0.105*0.03*7.15) {
		t.Fatalf("multiplier valuation: cny=%v priced=%v", cny, priced)
	}
	// API 牌价口径同源：1000 token 的正常花费应与免费折算一致
	if !almost(u.apiCost("global", "hy3", 1000), cny) {
		t.Fatalf("api cost must match multiplier valuation")
	}
	// 全球 dsv41f 倍率被促销烘成 x0.00 → 回落国内牌价 x0.11：
	// 1M token → 1000k × 0.11 × 0.105 = 11.55 积分 × ¥0.014
	cny, priced = u.freeMoney("global", "dsv41f", 1_000_000)
	if !priced || !almost(cny, 1000*0.11*0.105*0.014) {
		t.Fatalf("cn fallback valuation: cny=%v priced=%v", cny, priced)
	}
	// 模型不在清单 → 未定价
	if _, priced = u.freeMoney("global", "no-such", 1000); priced {
		t.Fatalf("unknown model must be unpriced")
	}
}

// 冷窗口自愈：uncertain 用量在快照时按当前促销状态折叠——缓存已热且免费 → 并入
// free_tokens（一次性改写存储）；缓存仍冷 → 保留 uncertain，不计金额。
func TestUsageStatsUncertainFold(t *testing.T) {
	mk := func(raw string) *upstream.ModelConfig {
		var c upstream.ModelConfig
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		return &c
	}
	global := mk(`{"models":[{"id":"hy3","credits":"x0.79"}],
	  "modelPromotions":[{"id":"hy3-free","enabled":true,"kind":"discount","modelIds":["hy3"],
	    "badge":{"label":"Free now"},"discount":{"factor":0,"discountedCredits":"0x","displayMode":"replace"},
	    "schedule":{"timezone":"Asia/Shanghai","validFrom":"2026-08-06T00:00:00+08:00","validUntil":"2027-09-30T00:00:00+08:00"}}]}`)

	// 1) 缓存冷：uncertain 保留，不计入 free、不编造金额
	u := NewUsageStats("", nil)
	u.Record("workbuddy", "global", "hy3", 1000, 100, 0, 0, false, true)
	snap := u.Snapshot()
	if snap.Total.FreeTokens != 0 || snap.Total.UncertainTokens != 1100 {
		t.Fatalf("cold cache: %+v", snap.Total)
	}

	// 2) 缓存热且免费：快照把 uncertain 折叠进 free
	modelPromos.cfgs["global"] = global
	defer delete(modelPromos.cfgs, "global")
	snap = u.Snapshot()
	if snap.Total.FreeTokens != 1100 || snap.Total.UncertainTokens != 0 {
		t.Fatalf("fold: %+v", snap.Total)
	}
	if !snap.Total.FreePriced || !almost(snap.Total.FreeCNY, 1.1*0.79*0.105*0.03*7.15) {
		t.Fatalf("fold money: %+v", snap.Total)
	}
	// 折叠是永久的：再次快照不重复计
	snap = u.Snapshot()
	if snap.Total.FreeTokens != 1100 || snap.Total.UncertainTokens != 0 {
		t.Fatalf("double fold: %+v", snap.Total)
	}
}

// 端到端：流式请求的 usage 帧落账 + /admin/api/usage 契约（codearts 假上游）。
func TestUsageStatsE2E(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"content":"你好"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1234,"completion_tokens":567,"total_tokens":1801,` +
		`"credit":0.12,"prompt_cache_hit_tokens":1024}}` + "\n\n" +
		"data: [DONE]\n\n"
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}})
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	h.cfg.Usage = NewUsageStats("", nil)

	body := `{"model":"glm-5.2","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	if raw, code := postChat(t, srv, body); code != 200 {
		t.Fatalf("status=%d body=%s", code, truncateText(string(raw), 200))
	}

	snap := h.cfg.Usage.Snapshot()
	if snap.Total.InputTokens != 1234 || snap.Total.OutputTokens != 567 {
		t.Fatalf("tokens=%+v", snap.Total)
	}
	// 缓存字段取各拼写最大值（该帧只有 cache_hit=1024）
	if snap.Total.CachedTokens != 1024 {
		t.Fatalf("cached=%+v", snap.Total)
	}
	// 华为无积分概念 → credit 原样记录但金额为 0；glm-5.2 非福利 → 不算免费
	if !almost(snap.Total.Credits, 0.12) || snap.Total.CreditsCNY != 0 {
		t.Fatalf("huawei has no credits: %+v", snap.Total)
	}
	if snap.Total.FreeRequests != 0 {
		t.Fatalf("non-benefit huawei must not be free: %+v", snap.Total)
	}
	if len(snap.Models) != 1 || snap.Models[0].Model != "glm-5.2" || snap.Models[0].Realm != "huawei" {
		t.Fatalf("models=%+v", snap.Models)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/admin/api/usage", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("admin status=%d", resp.StatusCode)
	}
	var out struct {
		Enabled bool `json:"enabled"`
		Total   struct {
			Requests int64 `json:"requests"`
		} `json:"total"`
		Days []struct {
			Date     string `json:"date"`
			Requests int64  `json:"requests"`
		} `json:"days"`
		Models []struct {
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || out.Total.Requests != 1 || len(out.Models) != 1 || out.Models[0].Model != "glm-5.2" {
		t.Fatalf("admin api=%+v", out)
	}
	// 逐日数组随 API 下发（缺省近 14 天）：此刻只有今日一条
	if len(out.Days) != 1 || out.Days[0].Requests != 1 || out.Days[0].Date == "" {
		t.Fatalf("admin days=%+v", out.Days)
	}
}

// 逐日视图（SPEC §34.3）：按北京自然日分桶，今日/累计/按天三条口径自洽，
// 天数按 ?days= 截断且最新在前。
func TestUsageStatsDailySeries(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "usage.json")
	today := time.Now().In(statsCST).Format("2006-01-02")
	doc := map[string]any{"days": map[string]any{
		"2026-01-01": map[string]any{"workbuddy|global|hy3": map[string]any{
			"requests": 1, "input_tokens": 100, "output_tokens": 10, "free_requests": 1, "free_tokens": 110}},
		"2026-01-02": map[string]any{"workbuddy|cn|glm-5.2": map[string]any{
			"requests": 2, "input_tokens": 200, "output_tokens": 20, "credits": 2.0}},
		today: map[string]any{"workbuddy|cn|glm-5.2": map[string]any{
			"requests": 3, "input_tokens": 300, "output_tokens": 30, "credits": 3.0}},
	}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	u := NewUsageStats(file, nil)

	two := u.SnapshotDays(2)
	if len(two.Days) != 2 {
		t.Fatalf("days=%d want 2", len(two.Days))
	}
	if two.Days[0].Date != today || two.Days[1].Date != "2026-01-02" {
		t.Fatalf("day order=%s,%s（应最新在前）", two.Days[0].Date, two.Days[1].Date)
	}
	if two.Days[0].Requests != 3 || two.Days[0].InputTokens != 300 || !almost(two.Days[0].CreditsCNY, 3*0.014) {
		t.Fatalf("today row=%+v", two.Days[0])
	}
	// 今日档只算今天，累计含全部三天，免费直用口径同样只算一次
	if two.Today.Requests != 3 || two.Total.Requests != 6 || two.Total.FreeRequests != 1 || two.Total.FreeTokens != 110 {
		t.Fatalf("today=%+v total=%+v", two.Today, two.Total)
	}
	if full := u.Snapshot(); len(full.Days) != 3 {
		t.Fatalf("default days=%d want 3", len(full.Days))
	}
}
