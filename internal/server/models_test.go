// 模型目录与路由面板数据源（SPEC §29.7）：账号级故障回退、静态回落、
// 唯一视图可用渠道标注、面板 overview/scan 形状。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/auth"
	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// nowCST 北京时间（促销时间窗以 Asia/Shanghai 为准）。
func nowCST() time.Time { return time.Now().In(time.FixedZone("CST", 8*3600)) }

// fakeModelCatalog 按 Authorization token 分派的腾讯模型目录假上游：
// 便于断言"某账号失败不影响家族其余账号"。
func fakeModelCatalog(t *testing.T, byToken map[string]int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/console/enterprises/personal/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if code, ok := byToken[tok]; ok && code >= 400 {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"code":500,"msg":"boom"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
}

const modelCatalogBody = `{"code":0,"msg":"","data":{
  "models":[
    {"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":1000000,"maxOutputTokens":64000,
     "tags":["craft","badge:夜间折扣:#1E90FF"],"supportsImages":true,"supportsToolCall":true,
     "vendor":"e","descriptionZh":"1M 上下文","isDefault":false},
    {"id":"hy4-preview","name":"Hy4 preview","maxInputTokens":1000000,"maxOutputTokens":64000,
     "tags":["craft","badge:夜间免费:#FF0000"],"supportsImages":true,"supportsToolCall":true,"vendor":"j"},
    {"id":"old-model","name":"OLD","disabled":true}
  ],
  "agents":[{"name":"cli","models":["glm-5.2","hy4-preview","old-model"]},{"name":"ide","models":["old-model"]}]
}}`

// 单账号失败 → 回退家族内其余账号（不得由单个不可达账号否定整个家族目录）。
func Test29CatalogFailsOverAcrossAccounts(t *testing.T) {
	resetModelCaches()
	fake := fakeModelCatalog(t, map[string]int{"tok-bad": 500}, modelCatalogBody)
	t.Setenv("OMNIGATE_TENCENT_BASE", fake.URL)
	auths := []*auth.Auth{tencentFakeAuth("bad", "tok-bad"), tencentFakeAuth("good", "tok-good")}
	_, _, _, h := buildTestServer(t, fake.URL, auths)
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	cat := h.catalog("workbuddy", modelFetchPanelWait)
	if cat.Source != sourceLive {
		t.Fatalf("family must be live from the healthy account: source=%s err=%s", cat.Source, cat.Error)
	}
	ids := map[string]map[string]any{}
	for _, m := range cat.Models {
		ids[m["id"].(string)] = m
	}
	if _, ok := ids["hy4-preview"]; !ok {
		t.Fatalf("expected catalog from the working account: %v", ids)
	}
	// tags → 访问类别贯通到面板条目
	if got := ids["hy4-preview"]["access"]; got != "night_free" {
		t.Fatalf("hy4-preview access=%v want night_free", got)
	}
	if got := ids["glm-5.2"]["access_label"]; got != "夜间折扣" {
		t.Fatalf("glm-5.2 access_label=%v", got)
	}
	if ids["glm-5.2"]["supports_images"] != true {
		t.Fatalf("supports_images must survive: %v", ids["glm-5.2"])
	}
	if _, ok := ids["old-model"]; ok {
		t.Fatalf("disabled models must be filtered out")
	}
}

// 没有任何家族账号 → 静态回落表 + 失败原因（不静默给空清单）。
func Test29CatalogStaticFallbackWithoutAccount(t *testing.T) {
	resetModelCaches()
	fake := fakeModelCatalog(t, nil, modelCatalogBody)
	t.Setenv("OMNIGATE_TENCENT_BASE", fake.URL)
	_, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	// 面板「刷新目录」路径（同步等待）：失败原因与静态回落一并返回。
	cat := h.catalog("workbuddy", modelFetchPanelWait)
	if cat.Source != sourceStatic {
		t.Fatalf("source=%s want static", cat.Source)
	}
	if len(cat.Models) == 0 {
		t.Fatal("static fallback must still list models")
	}
	if !strings.Contains(cat.Error, "no workbuddy account") {
		t.Fatalf("failure reason must be surfaced: %q", cat.Error)
	}
}

// 面板非阻塞：冷缓存 + wait=0 立即返回静态表并标记预热中（不等待上游）。
func Test29CatalogDoesNotBlockOnColdCache(t *testing.T) {
	resetModelCaches()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {} // 永不响应：面板打开不得被上游延迟绑架
	}))
	defer slow.Close()
	t.Setenv("OMNIGATE_TENCENT_BASE", slow.URL)
	_, _, _, h := buildTestServer(t, slow.URL,
		[]*auth.Auth{fakeAuth("u1", "tok1"), tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	done := make(chan familyCatalog, 1)
	go func() { done <- h.catalog("workbuddy", 0) }()
	select {
	case cat := <-done:
		if cat.Source != sourceStatic || len(cat.Models) == 0 {
			t.Fatalf("cold cache must serve static immediately: %+v", cat.Source)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("catalog(0) must not wait for upstream")
	}
}

// overview：单请求给出两家族目录 + 路由表 + 禁用集 + 默认表。
func Test29RoutesOverviewShape(t *testing.T) {
	resetModelCaches()
	fake := fakeModelCatalog(t, nil, modelCatalogBody)
	t.Setenv("OMNIGATE_TENCENT_BASE", fake.URL)
	_, _, _, h := buildTestServer(t, fake.URL,
		[]*auth.Auth{fakeAuth("u1", "tok1"), tencentFakeAuth("u2", "tok-good")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	rec := httptest.NewRecorder()
	h.adminRoutesOverview(rec, httptest.NewRequest("GET", "/admin/api/routes/overview", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var got struct {
		Families []familyCatalog    `json:"families"`
		Routes   []adapt.ModelRoute `json:"routes"`
		Blocked  []string           `json:"blocked"`
		Defaults []adapt.ModelRoute `json:"defaults"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Families) != 2 {
		t.Fatalf("families=%d want 2", len(got.Families))
	}
	if len(got.Routes) == 0 || len(got.Defaults) == 0 {
		t.Fatalf("routes=%d defaults=%d", len(got.Routes), len(got.Defaults))
	}
	// 默认表：腾讯固有模型 → workbuddy；撞名组由 codearts 持有裸名。
	def := map[string]string{}
	for _, r := range got.Defaults {
		def[r.Model] = r.Family
	}
	if def["kimi-k2.7"] != "workbuddy" || def["glm-5.3-flash"] != "codearts" {
		t.Fatalf("default table wrong: kimi-k2.7=%q glm-5.3-flash=%q", def["kimi-k2.7"], def["glm-5.3-flash"])
	}
}

// 唯一视图：撞名模型标注 available_families 与 routed_family。
func Test29UnifiedModelViewAvailableFamilies(t *testing.T) {
	resetModelCaches()
	fake := fakeModelCatalog(t, nil, modelCatalogBody)
	t.Setenv("OMNIGATE_TENCENT_BASE", fake.URL)
	_, _, _, h := buildTestServer(t, fake.URL,
		[]*auth.Auth{fakeAuth("u1", "tok1"), tencentFakeAuth("u2", "tok-good")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	var hit map[string]any
	for _, m := range h.unifiedModelList() {
		if m["id"] == "glm-5.2" {
			hit = m
		}
	}
	if hit == nil {
		t.Fatal("glm-5.2 missing from unified view")
	}
	if hit["routed_family"] != "codearts" {
		t.Fatalf("routed_family=%v", hit["routed_family"])
	}
	fams, _ := hit["available_families"].([]string)
	if len(fams) != 2 {
		t.Fatalf("glm-5.2 exists in both channels: %v", hit["available_families"])
	}
}

// 逐账号扫描：按账号暴露成功/失败（付费与免费档可见模型不同）。
func Test29ModelsScanPerAccount(t *testing.T) {
	resetModelCaches()
	fake := fakeModelCatalog(t, map[string]int{"tok-bad": 500}, modelCatalogBody)
	t.Setenv("OMNIGATE_TENCENT_BASE", fake.URL)
	_, _, _, h := buildTestServer(t, fake.URL,
		[]*auth.Auth{tencentFakeAuth("bad", "tok-bad"), tencentFakeAuth("good", "tok-good")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	rows := h.scanModelAccounts("workbuddy")
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	byUID := map[string]map[string]any{}
	for _, r := range rows {
		byUID[r["uid"].(string)] = r
	}
	if byUID["bad"]["ok"] != false {
		t.Fatalf("bad account must report failure: %v", byUID["bad"])
	}
	if byUID["good"]["ok"] != true {
		t.Fatalf("good account must report success: %v", byUID["good"])
	}
	if ms, _ := byUID["good"]["models"].([]map[string]any); len(ms) != 2 {
		t.Fatalf("good account models=%v", byUID["good"]["models"])
	}
}

// 访问标注按区域叠加：同模型国内按量计费 / 国际限免 → 两个区域各一条；
// 且**只贴标签不增删条目**（配置里的其它模型绝不引入目录）。
func Test29AccessOverlayPerRealm(t *testing.T) {
	promoReset()
	t.Cleanup(promoReset)

	cn := &upstream.ModelConfig{
		Models: []upstream.ConfigModel{{ID: "deepseek-v4.1-flash", Credits: "x0.11"}},
	}
	zero := 0.0
	until := nowCST().Add(48 * time.Hour).Format(time.RFC3339)
	gl := &upstream.ModelConfig{
		Models: []upstream.ConfigModel{
			{ID: "deepseek-v4.1-flash", Credits: "x0.00"},
			{ID: "gpt-6-astra", Credits: "x6.67"}, // 只在配置里、不在目录里
		},
		Promotions: []upstream.ModelPromotion{{
			ID: "free", Enabled: true, ModelIDs: []string{"deepseek-v4.1-flash"},
			Badge:    upstream.PromoBadge{Label: "Free now"},
			Discount: upstream.PromoDiscount{Factor: &zero},
			Schedule: upstream.PromoSchedule{ValidUntil: until},
		}},
	}
	promoFinish("cn", cn, nil)
	promoFinish("global", gl, nil)

	h := &Handler{}
	entries := []map[string]any{{"id": "deepseek-v4.1-flash", "access": "paid", "access_label": "按量计费"}}
	h.overlayAccess(entries, "cn")

	if len(entries) != 1 {
		t.Fatalf("overlay must not add entries: %d", len(entries))
	}
	if entries[0]["access"] != "paid" || entries[0]["multiplier"] != "x0.11" {
		t.Fatalf("CN realm (source) label wrong: %v", entries[0])
	}
	byRealm, ok := entries[0]["access_by_realm"].(map[string]map[string]any)
	if !ok || len(byRealm) != 2 {
		t.Fatalf("both realms must be reported: %v", entries[0]["access_by_realm"])
	}
	if byRealm["global"]["access"] != "free" || byRealm["global"]["access_label"] != "Free now" {
		t.Fatalf("global promo must show free: %v", byRealm["global"])
	}
	// 只在配置里、不在目录里的模型不得出现
	for _, e := range entries {
		if e["id"] == "gpt-6-astra" {
			t.Fatal("config-only model must NOT be injected into the catalog")
		}
	}

	// 来源区域为全球时，主标注取全球值
	entries2 := []map[string]any{{"id": "deepseek-v4.1-flash"}}
	h.overlayAccess(entries2, "global")
	if entries2[0]["access"] != "free" {
		t.Fatalf("global source must drive the primary label: %v", entries2[0])
	}
}

// 配置里没有该模型（如华为模型）→ 不动原有标注。
func Test29AccessOverlayLeavesUnknownModelsAlone(t *testing.T) {
	promoReset()
	t.Cleanup(promoReset)
	promoFinish("cn", &upstream.ModelConfig{Models: []upstream.ConfigModel{{ID: "other"}}}, nil)
	h := &Handler{}
	entries := []map[string]any{{"id": "glm-5.2", "access": "discount", "access_label": "夜间折扣"}}
	h.overlayAccess(entries, "cn")
	if entries[0]["access"] != "discount" || entries[0]["access_label"] != "夜间折扣" {
		t.Fatalf("unknown model must keep its tags-based label: %v", entries[0])
	}
	if _, ok := entries[0]["access_by_realm"]; ok {
		t.Fatal("unknown model must not gain per-realm labels")
	}
}

// 华为家族不得污染共享的 realm 配置缓存：华为客户端没有 /v3/config，若它抢到
// realm=cn 再以"不支持"收场，就会给 cn 打上 5min 负冷却，把腾讯 CN 的配置一起
// 饿死（实测症状：只有全球有标注、国内标注缺失，且状态里看不出原因）。
func Test29PromoRefreshSkipsUnsupportedFamilies(t *testing.T) {
	promoReset()
	t.Cleanup(promoReset)
	fake := fakeModelCatalog(t, nil, modelCatalogBody)
	t.Setenv("OMNIGATE_TENCENT_BASE", fake.URL)
	_, _, _, h := buildTestServer(t, fake.URL,
		[]*auth.Auth{fakeAuth("u1", "tok1"), tencentFakeAuth("u2", "tok-good")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	var huawei []*pool.Account
	for _, a := range h.cfg.Pool.Accounts() {
		if a.ProfileID == "codearts" {
			huawei = append(huawei, a)
		}
	}
	if len(huawei) == 0 {
		t.Fatal("test needs a codearts account")
	}
	h.refreshPromos(huawei) // 华为客户端不实现 ConfigAPI

	st := promoStatusOf()["cn"]
	if st["ok"] == true {
		t.Fatalf("huawei must not populate the cn config: %v", st)
	}
	if e, ok := st["error"]; ok && e != "" {
		t.Fatalf("huawei must not record a cn failure (would block the tencent fetch): %v", st)
	}
	if !promoClaim("cn") {
		t.Fatal("cn realm must still be claimable by the tencent family")
	}
}
