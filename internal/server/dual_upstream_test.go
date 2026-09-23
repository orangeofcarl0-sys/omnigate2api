// 双上游并存集成（SPEC §24）：华为 + 腾讯账号同池，按 Profile 路由与
// 轮换互不串用；腾讯请求走 roles 透传 + TencentClient。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/auth"
)

// tencentFakeAuth 构造腾讯命名空间账号。
func tencentFakeAuth(id, token string) *auth.Auth {
	return &auth.Auth{
		UserID: id, UserName: "user-" + id, Profile: "workbuddy",
		CloudDragonTok: token, RefreshToken: "rt" + id,
		Expiration: "2099-01-01T00:00:00Z", EnterpriseID: "ent-" + id,
		Domain: "www.codebuddy.cn",
	}
}

// fakeTencentUpstream 假 copilot 服务器（记录请求体，返回 okStream）。
func fakeTencentUpstream(t *testing.T, sizes *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if sizes != nil {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(b)))
			*sizes = append(*sizes, string(b))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
}

// workbuddyProfile 已并入内置表（8d）：测试直接使用 adapt.Workbuddy 真身，
// 避免本地替身与内置漂移（A3）。

func TestIntegrationDualUpstreamCoexist(t *testing.T) {
	var huaweiSizes, tencentSizes []string
	huawei := fakeUpstreamSized(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}, &huaweiSizes)
	tencent := fakeTencentUpstream(t, &tencentSizes)
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL) // TencentClient 构造时读取

	srv, _, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{fakeAuth("u1", "tok1"), tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(textOnlyTestProfile(), &adapt.Workbuddy)

	// 1) 默认（codearts）→ 华为账号与折叠
	if _, code := postChat(t, srv, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`); code != 200 {
		t.Fatalf("codearts chat: %d", code)
	}
	// 2) X-Provider: workbuddy → 腾讯账号与 roles 透传
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"r"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Provider", "workbuddy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if len(huaweiSizes) != 1 || len(tencentSizes) != 1 {
		t.Fatalf("routing: huawei=%d tencent=%d", len(huaweiSizes), len(tencentSizes))
	}
	if !strings.Contains(huaweiSizes[0], "[对话历史]") && !strings.Contains(huaweiSizes[0], "[回复要求]") {
		t.Fatalf("codearts must fold: %s", huaweiSizes[0])
	}
	if !strings.Contains(tencentSizes[0], `"role":"tool"`) || !strings.Contains(tencentSizes[0], "deepseek-v4-flash") {
		t.Fatalf("tencent must pass through roles+model: %s", tencentSizes[0])
	}
	if strings.Contains(tencentSizes[0], "[系统指令]") {
		t.Fatalf("tencent must not fold: %s", tencentSizes[0])
	}
}

// 池内按 Profile 隔离：华为账号全冷却不影响腾讯账号选取。
func TestIntegrationPoolPickByProfile(t *testing.T) {
	var tencentSizes []string
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	tencent := fakeTencentUpstream(t, &tencentSizes)
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL)

	// u1 华为账号先禁用；u2 腾讯健康
	auths := []*auth.Auth{fakeAuth("u1", "tok1"), tencentFakeAuth("u2", "tok2")}
	srv, p, _, h := buildTestServer(t, huawei.URL, auths)
	h.cfg.Profiles = adapt.NewRegistry(textOnlyTestProfile(), &adapt.Workbuddy)
	p.Disable("u1", "manual")

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"q"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Provider", "workbuddy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("workbuddy request must succeed despite huawei account disabled: %d", resp.StatusCode)
	}
	if len(tencentSizes) != 1 {
		t.Fatalf("tencent upstream must be used: %d", len(tencentSizes))
	}
}

// TestAdminGrowth SPEC §32 观测面：成长中心状态接口（积分/能量/签到/任务/宠物）。
func TestAdminGrowth(t *testing.T) {
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "checkin-activity-status"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"theme_name":"Buddy加油站","today_checked_in":true,"streak_days":4,"daily_credit":100,"total_credits":400,"active":true}}`))
		case strings.Contains(r.URL.Path, "get-user-resource"):
			_, _ = w.Write([]byte(`{"Response":{"Data":{"Accounts":[{"CapacityRemain":888}]}}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0}`))
		}
	}))
	defer billing.Close()
	activity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/buddy/quota"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"affordable":0,"balance":43,"cost_per_open":10,"max_open_count":5}}`))
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"a","accept_status":"claimed"},{"task_code":"b","accept_status":"completed"},{"task_code":"c","accept_status":"accepted"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/travel/status"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"traveling","location":{"name":"咖啡馆"},"arrive_at":200,"server_now":100}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0}`))
		}
	}))
	defer activity.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", billing.URL)
	t.Setenv("OMNIGATE_ACTIVITY_BASE", activity.URL)
	t.Setenv("OMNIGATE_TENCENT_BASE", billing.URL)

	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	srv, _, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	req, _ := http.NewRequest("GET", srv.URL+"/admin/api/growth", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	for _, want := range []string{`"credits":888`, `"energy":43`, `"streak_days":4`, `"claimed":1`, `"completed":1`, `"traveling"`, "咖啡馆"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("growth must expose %s: %s", want, raw)
		}
	}
}
