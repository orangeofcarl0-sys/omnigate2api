// 裸模型名路由集成测试（SPEC §29）：无 X-Provider 走路由表落对渠道、
// 显式覆盖优先、/v1/models 唯一视图、管理 API 校验/热生效。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/auth"
)

// routeTestEnv 组装：华为 fake + 腾讯 fake + 双账号 + 双 Profile 注册表。
func routeTestEnv(t *testing.T, tencentRec *[]string) *httptest.Server {
	t.Helper()
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	tencent := fakeTencentUpstream(t, tencentRec)
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL)
	srv, _, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)
	// 断言路由表为内置默认表（glm-5.2→codearts；kimi-k2.7→workbuddy）
	if f, ok := h.routesTable().FamilyOf("glm-5.2"); !ok || f != "codearts" {
		t.Fatalf("default route glm-5.2: %q %v", f, ok)
	}
	if f, ok := h.routesTable().FamilyOf("kimi-k2.7"); !ok || f != "workbuddy" {
		t.Fatalf("default route kimi-k2.7: %q %v", f, ok)
	}
	return srv
}

// 无 X-Provider + 表内腾讯模型 → 路由表决策落 workbuddy（dsh 场景）。
func Test29RouteWithoutProvider(t *testing.T) {
	var tencentRec []string
	srv := routeTestEnv(t, &tencentRec)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"kimi-k2.7","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if len(tencentRec) != 1 {
		t.Fatalf("kimi-k2.7 must route to workbuddy: %d recs", len(tencentRec))
	}
}

// 显式 X-Provider 覆盖优先于路由表（表内 codearts 模型显式走 workbuddy）。
func Test29ExplicitOverrideWins(t *testing.T) {
	var tencentRec []string
	srv := routeTestEnv(t, &tencentRec)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"q"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Provider", "workbuddy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(tencentRec) != 1 {
		t.Fatalf("explicit override must hit workbuddy: %d recs", len(tencentRec))
	}
}

// /v1/models 无渠道 → 唯一视图：无重复 id，且带 family 字段。
func Test29UnifiedModelView(t *testing.T) {
	var tencentRec []string
	srv := routeTestEnv(t, &tencentRec)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var body struct {
		Data []struct {
			ID     string `json:"id"`
			Family string `json:"family"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("parse: %v %s", err, raw)
	}
	seen := map[string]string{}
	for _, m := range body.Data {
		if prev, dup := seen[m.ID]; dup {
			t.Fatalf("duplicate model id %q (family %s/%s)", m.ID, prev, m.Family)
		}
		seen[m.ID] = m.Family
	}
	if seen["glm-5.2"] != "codearts" || seen["kimi-k2.7"] != "workbuddy" {
		t.Fatalf("families wrong: %+v", seen)
	}
}

// 管理 API：GET 表；PUT 非法（撞名）→ 409 且表不变；PUT 合法 → 热生效。
func Test29AdminRoutesAPI(t *testing.T) {
	var tencentRec []string
	srv := routeTestEnv(t, &tencentRec)
	put := func(body string) (int, map[string]any) {
		req, _ := http.NewRequest("PUT", srv.URL+"/admin/api/routes", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	// 撞名 → 409
	code, out := put(`{"routes":[{"model":"glm-5.2","family":"codearts"},{"model":"glm-5.2","family":"workbuddy"}]}`)
	if code != http.StatusConflict {
		t.Fatalf("duplicate must 409, got %d %v", code, out)
	}
	// 合法替换（hy3→workbuddy 显式）→ 200 且热生效
	code, out = put(`{"routes":[{"model":"hy3","family":"workbuddy"},{"model":"glm-5.2","family":"codearts"}]}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("valid replace must 200: %d %v", code, out)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/admin/api/routes", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"hy3"`) || strings.Contains(string(raw), `"kimi-k2.7"`) {
		t.Fatalf("routes not hot-updated: %s", raw)
	}
}
