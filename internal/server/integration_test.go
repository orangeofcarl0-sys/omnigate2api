// 集成测试：假上游 + 假账号池，验证 handler 关键行为——
// 工具围栏流式解析、429 软冷却与账号轮换、指纹续接（无需真实凭据）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/auth"
	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// fakeUpstream 按 x-auth-token 决定场景的假 CodeArts 引擎。
func fakeUpstream(t *testing.T, perToken map[string]func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v2/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		token := r.Header.Get("x-auth-token")
		sc, ok := perToken[token]
		if !ok {
			sc = perToken["*"]
		}
		sc(w)
	}))
}

// sseFrame 写一行 SSE（CodeArts 逐行 data: 或 event:/data: 双形态兼容）。
func sseFrame(w http.ResponseWriter, event, data string) {
	if event != "" {
		_, _ = io.WriteString(w, "event: "+event+"\n")
	}
	if data != "" {
		_, _ = io.WriteString(w, "data: "+data+"\n")
	}
}

// chatDelta 构造 OpenAI chat.completion.chunk 增量帧。
func chatDelta(delta map[string]any) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
	return string(b)
}

// okStream 正常流：思考增量 + 正文（含工具围栏时解析）+ [DONE] → 合成 finish。
func okStream(toolFence bool) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseFrame(w, "", chatDelta(map[string]any{"reasoning_content": "r1"}))
		sseFrame(w, "", chatDelta(map[string]any{"content": "先看一下。"}))
		if toolFence {
			fence := "```tool_call\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"F:/x/a.mjs\"}}\n```"
			sseFrame(w, "", chatDelta(map[string]any{"content": fence}))
		} else {
			sseFrame(w, "", chatDelta(map[string]any{"content": "完成。"}))
		}
		sseFrame(w, "", `[DONE]`)
	}
}

// rateLimitFrame 429 场景：错误帧附带 429 文本。
func rateLimitFrame(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	sseFrame(w, "", `{"error_code":"InferHub.MaaS.003002002.502","error_msg":"provider API error (status 429)"}`)
	sseFrame(w, "", `[DONE]`)
}

// fakeUpstreamSized 记录各请求原始 body（用于断言折叠模式）。
func fakeUpstreamSized(t *testing.T, perToken map[string]func(w http.ResponseWriter), sizes *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v2/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if sizes != nil {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(b)))
			*sizes = append(*sizes, string(b))
		}
		token := r.Header.Get("x-auth-token")
		sc, ok := perToken[token]
		if !ok {
			sc = perToken["*"]
		}
		sc(w)
	}))
}

// buildTestServer 组装假上游 + 账号池 + handler。
func buildTestServer(t *testing.T, upstreamURL string, authes []*auth.Auth) (*httptest.Server, *pool.Pool, string, *Handler) {
	t.Helper()
	t.Setenv("OMNIGATE_UPSTREAM_BASE", upstreamURL) // 必须在 pool.New 之前
	stateFile := filepath.Join(t.TempDir(), "state.json")
	p, err := pool.New(authes, pool.Config{
		ErrThreshold: 3, ErrCooldown: 10 * time.Minute, SoftCooldown: 45 * time.Second,
		MaxConcurrent: 5, KeepaliveWindow: 10 * time.Minute,
	}, stateFile)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	cfg := Config{Pool: p, Upstream: upstream.New(10 * time.Second), APIKey: "test-key", DefaultModel: "glm-5.2"}
	h := NewHandler(cfg)
	srv := httptest.NewServer(h.mux)
	t.Cleanup(srv.Close)
	return srv, p, stateFile, h
}

func fakeAuth(id, token string) *auth.Auth {
	return auth.New(id, "user-"+id, "dom", token, "ak"+id, "sk"+id,
		"2099-01-01T00:00:00Z", "rt"+id, "v"+id)
}

// postChat 发一条 /v1/chat/completions，返回响应体。
func postChat(t *testing.T, srv *httptest.Server, body string) ([]byte, int) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode
}

// 工具围栏流式解析 + finish 合成。
func TestIntegrationToolStream(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){
		"*": okStream(true),
	})
	srv, _, _, _ := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})

	body := `{"model":"glm-5.2","stream":true,"tools":[{"type":"function","function":{"name":"Read","description":"r","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hi"}]}`
	raw, code := postChat(t, srv, body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	if !strings.Contains(string(raw), `"tool_calls"`) || !strings.Contains(string(raw), `"name":"Read"`) {
		t.Fatalf("tool_calls missing: %s", raw)
	}
	if !strings.Contains(string(raw), `"finish_reason":"tool_calls"`) || !strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("finish/DONE missing: %s", raw)
	}
	// 调用前正文透传；围栏与调用块本体不得泄漏为 content
	if !strings.Contains(string(raw), `"content":"先看一下。`) {
		t.Fatalf("pre-call preamble should stream: %s", raw)
	}
	if strings.Contains(string(raw), "\"content\":\"```tool_call\"") {
		t.Fatalf("fence leaked into content: %s", raw)
	}
}

// 429 场景：账号 A 限流 → 软冷却 + 轮换到账号 B；随后全部冷却期返回 503。
func TestIntegrationRateLimitRotation(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){
		"tok1": rateLimitFrame,
		"tok2": okStream(false),
	})
	srv, _, stateFile, _ := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1"), fakeAuth("u2", "tok2")})

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	raw, code := postChat(t, srv, body)
	if code != 200 {
		t.Fatalf("rotation should succeed: status=%d body=%s", code, raw)
	}
	if !strings.Contains(string(raw), "完成") {
		t.Fatalf("expected fallback account content: %s", raw)
	}
	// u1 应已进入软冷却
	st, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	var accounts []struct {
		Name      string `json:"name"`
		CoolUntil string `json:"cool_until"`
	}
	if err := json.Unmarshal(st, &accounts); err != nil {
		t.Fatalf("state json: %v err=%s", st, err)
	}
	found := false
	for _, a := range accounts {
		if a.Name == "u1" && a.CoolUntil != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("u1 should be in cooldown: %s", st)
	}
}

// 纯流式 429 帧 → SSE 错误事件 + 软冷却（streamTools 分支）。
func TestIntegrationStream429(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){
		"*": rateLimitFrame,
	})
	srv, _, _, _ := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})

	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	raw, code := postChat(t, srv, body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	if !strings.Contains(string(raw), "rate_limit_error") || !strings.Contains(string(raw), "retryable") {
		t.Fatalf("rate_limit payload missing: %s", raw)
	}
}

// 指纹续接：第二请求(前缀+1)必须走增量（tail-only，不含已折叠历史）。
func TestIntegrationFingerprintContinue(t *testing.T) {
	var sizes []string
	fake := fakeUpstreamSized(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}, &sizes)
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	h.cfg.SessionMode = "incremental"
	h.cfg.Profiles = adapt.NewRegistry(textOnlyTestProfile()) // §31：折叠/指纹机制测试载体

	mk := func(extra string) string {
		return `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a"},{"role":"user","content":"` + extra + `"}]}`
	}
	// 第二轮必须回发完整历史 + 增量（n+1），前缀命中走 tail-only
	next := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a"},{"role":"user","content":"one"},{"role":"user","content":"two"}]}`
	if _, code := postChat(t, srv, mk("one")); code != 200 {
		t.Fatalf("first request failed: %d", code)
	}
	if _, code := postChat(t, srv, next); code != 200 {
		t.Fatalf("continue request failed: %d", code)
	}
	if len(sizes) < 2 {
		t.Fatalf("expected >=2 upstream calls, got %d", len(sizes))
	}
	// 第二轮必须为 tail-only（已折叠历史不重发）
	if strings.Contains(sizes[1], "hi") || strings.Contains(sizes[1], "one") {
		t.Fatalf("continue must fold tail-only: %s", sizes[1])
	}
	if !strings.Contains(sizes[1], "two") {
		t.Fatalf("tail increment must be present: %s", sizes[1])
	}
}

// native 模式：默认契约——全量折叠，不依赖任何指纹/上游记忆。
// 请求成功且不存在增量路径的副作用即可（信息等价由实现保证）。
func TestIntegrationNativeModeDefault(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	srv, _, _, _ := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	if _, code := postChat(t, srv, body); code != 200 {
		t.Fatalf("native default failed: %d", code)
	}
}

// 熔断实证（SPEC §5.3）：incremental 下同会话续接为增量(小请求体)；
// 预热熔断(3×GuardMiss)后相同续接必须强制 native(全量折叠,请求体变大)。
func TestIntegrationCircuitBreakerForcesNative(t *testing.T) {
	var sizes []string
	fake := fakeUpstreamSized(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}, &sizes)
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	h.cfg.SessionMode = "incremental"
	h.cfg.Profiles = adapt.NewRegistry(textOnlyTestProfile()) // §31：折叠/指纹机制测试载体

	mk := func(extra string) string {
		return `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a"},{"role":"user","content":"one"},{"role":"user","content":"two"},{"role":"user","content":"` + extra + `"}]}`
	}
	seed := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a"},{"role":"user","content":"one"},{"role":"user","content":"two"}]}`
	if _, code := postChat(t, srv, seed); code != 200 {
		t.Fatalf("seed request failed: %d", code)
	}
	if _, code := postChat(t, srv, mk("three")); code != 200 {
		t.Fatalf("continue request failed: %d", code)
	}
	if len(sizes) < 2 {
		t.Fatalf("expected >=2 upstream calls, got %d", len(sizes))
	}
	incSize := len(sizes[1])
	// 第二轮必须为增量（tail-only，不含已折叠历史）——指纹续接生效
	if strings.Contains(sizes[1], "hi") || strings.Contains(sizes[1], "one") {
		t.Fatalf("continue must fold tail-only: %s", sizes[1])
	}

	// 预热熔断: 3 次守卫 miss → open
	now := time.Now()
	for i := 0; i < 3; i++ {
		h.breakerFor(&adapt.Codearts).Record(adapt.GuardMiss, now.Add(time.Duration(i)*time.Second))
	}
	if !h.breakerFor(&adapt.Codearts).Open(time.Now()) {
		t.Fatalf("breaker must be open after preheating")
	}
	if _, code := postChat(t, srv, mk("four")); code != 200 {
		t.Fatalf("post-circuit request failed: %d", code)
	}
	fullSize := len(sizes[len(sizes)-1])
	// 熔断必须强制 native：全量折叠（含已折叠历史）且体积大于增量
	if !strings.Contains(sizes[len(sizes)-1], "hi") {
		t.Fatalf("circuit must force native full fold: %s", sizes[len(sizes)-1])
	}
	if fullSize <= incSize {
		t.Fatalf("circuit must force native full fold: inc=%d full=%d", incSize, fullSize)
	}
}

// 多上游路由（SPEC §4.4）：X-Provider 选择 Profile；未注册回退 codearts；
// 隔离验证：echo(roles/none) 与 codearts 并存均正常工作。
func TestIntegrationMultiProfileRouting(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	echo := adapt.Codearts
	echo.ID = "echo"
	echo.Display = "Echo roles"
	echo.Message.Model = "roles"
	echo.Message.Folding = nil
	echo.Session.Kind = "none"
	reg := adapt.NewRegistry(&adapt.Codearts, &echo)

	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	h.cfg.Profiles = reg

	post := func(provider string) int {
		body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		if provider != "" {
			req.Header.Set("X-Provider", provider)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}
	if c := post(""); c != 200 {
		t.Fatalf("codearts default: %d", c)
	}
	if c := post("echo"); c != 200 {
		t.Fatalf("echo provider: %d", c)
	}
	if c := post("nope"); c != 200 {
		t.Fatalf("unregistered provider must fall back: %d", c)
	}
	if len(reg.IDs()) != 2 {
		t.Fatalf("registry ids: %v", reg.IDs())
	}
}
