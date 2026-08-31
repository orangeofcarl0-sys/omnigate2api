// 阶段 8e 集成（SPEC §28.4）：原生 tool_calls 三协议帧形、tool_choice string、
// refresh envelope 自动刷新、402/12153 错误策略、/v1/models 按家族分发。
package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/auth"
)

// tencent8eUpstream 假 copilot：chat/refresh/models 三端点，chat 响应可定制。
func tencent8eUpstream(t *testing.T, chatResp string, chatRec *[]string, refreshEnv string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/chat/completions":
			if chatRec != nil {
				b, _ := io.ReadAll(r.Body)
				*chatRec = append(*chatRec, string(b))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, chatResp)
		case "/v2/plugin/auth/token/refresh":
			w.Header().Set("Content-Type", "application/json")
			if refreshEnv == "" {
				_, _ = io.WriteString(w, `{"accessToken":"new-token","refreshToken":"new-refresh","expiresIn":3600,"domain":"d"}`)
			} else {
				_, _ = io.WriteString(w, refreshEnv)
			}
		case "/console/enterprises/personal/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"models":[{"id":"kimi-k2.9","name":"K","maxInputTokens":300000,"maxOutputTokens":8192,"disabled":false}],"agents":[{"name":"cli","models":["kimi-k2.9"]}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// resetModelCaches 清空跨测试共享的模型缓存（正/负缓存）。
func resetModelCaches() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	tencentModelsCache.Lock()
	tencentModelsCache.ids = nil
	tencentModelsCache.fetched = time.Time{}
	tencentModelsCache.lastFail = time.Time{}
	tencentModelsCache.Unlock()
}

// providerChat 发一条带 X-Provider: workbuddy 的 chat 请求，返回响应体。
func providerChat(t *testing.T, srv *httptest.Server, body string) ([]byte, int) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Provider", "workbuddy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode
}

// sseDataLines 提取 SSE 响应全部 data: 行。
func sseDataLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("bad sse data %q: %v", data, err)
		}
		out = append(out, m)
	}
	return out
}

const nativeToolStream = "" +
	"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_9\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"\"}}]}}]}\n" +
	"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"/a\\\"}\"}}]}}]}\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n" +
	"data: [DONE]\n"

// 流式：原生 tool_calls 以单帧完整参数下发，回合终结于 tool_calls（决策 D）。
func Test8eNativeToolCallsStreamChat(t *testing.T) {
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	tencent := tencent8eUpstream(t, nativeToolStream, nil, "")
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL)
	srv, _, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"do it"}],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Provider", "workbuddy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	chunks := sseDataLines(t, raw)
	var toolChunk, finChunk map[string]any
	for _, c := range chunks {
		if tc, ok := c["choices"].([]any); ok {
			if len(tc) > 0 {
				if ch, ok := tc[0].(map[string]any); ok && ch["delta"] != nil {
					if d, ok := ch["delta"].(map[string]any); ok && d["tool_calls"] != nil {
						toolChunk = c
					}
				}
				if ch, ok := tc[0].(map[string]any); ok && ch["finish_reason"] == "tool_calls" {
					finChunk = c
				}
			}
		}
	}
	if toolChunk == nil {
		t.Fatalf("no tool_calls chunk: %s", raw)
	}
	// 断言：单帧完整参数（决策 D），id/name 透传
	tc := toolChunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if tc["id"] != "call_9" || fn["name"] != "Read" || fn["arguments"] != `{"path":"/a"}` {
		t.Fatalf("tool call must carry full args in single frame: %+v", tc)
	}
	if finChunk == nil {
		t.Fatalf("finish_reason must be tool_calls: %s", raw)
	}
}

// 非流式：聚合响应直接带 message.tool_calls（原生路径，不经文本围栏）。
func Test8eNativeToolCallsNonStream(t *testing.T) {
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	tencent := tencent8eUpstream(t, nativeToolStream, nil, "")
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL)
	srv, _, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	raw, code := providerChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"do it"}],"tools":[{"type":"function","function":{"name":"Read"}}]}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, raw)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("parse: %v body=%s", err, raw)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("must carry native tool_calls: %s", raw)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_9" || tc.Function.Name != "Read" || tc.Function.Arguments != `{"path":"/a"}` {
		t.Fatalf("native call wrong: %+v", tc)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish=%s", resp.Choices[0].FinishReason)
	}
}

// tool_choice string 语义（决策 D）：none → 连 tools 删除；required → 透传 string。
func Test8eToolChoiceNoneDropsTools(t *testing.T) {
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	var bodies []string
	tencent := tencent8eUpstream(t, "data: [DONE]\n", &bodies, "")
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL)
	srv, _, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	providerChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"q"}],"tools":[{"type":"function","function":{"name":"F"}}],"tool_choice":"none"}`)
	providerChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"q"}],"tools":[{"type":"function","function":{"name":"F"}}],"tool_choice":"required"}`)
	if len(bodies) != 2 {
		t.Fatalf("requests=%d", len(bodies))
	}
	if strings.Contains(bodies[0], `"tools"`) || strings.Contains(bodies[0], `"tool_choice"`) {
		t.Fatalf("none must drop tools and tool_choice: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"tool_choice":"required"`) || !strings.Contains(bodies[1], `"name":"F"`) {
		t.Fatalf("required must pass string and keep tools: %s", bodies[1])
	}
}

// 到期自动刷新走 envelope 解析：Validate 触发 refresh → 新 token 生效。
func Test8eRefreshEnvelopeViaValidate(t *testing.T) {
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	var refreshHits int
	tencent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			refreshHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"env-token","refreshToken":"new-refresh","expiresIn":3600,"domain":"d"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n")
	}))
	defer tencent.Close()
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL)

	// 腾讯账号：expiration 1 分钟后到期 → 请求前置刷新
	authz := tencentFakeAuth("u2", "tok2")
	authz.Expiration = time.Now().Add(time.Minute).Format(time.RFC3339)
	srv, _, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{authz})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

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
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if refreshHits != 1 {
		t.Fatalf("refresh must run once before chat: %d", refreshHits)
	}
	if authz.CloudDragonTok != "env-token" {
		t.Fatalf("token must be updated from envelope: %s", authz.CloudDragonTok)
	}
}

// 402/积分不足 → 冷却至次日 04:00（决策 B）。
func Test8eHardCreditCooldown(t *testing.T) {
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	tencent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/chat/completions" {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, `{"msg":"余额不足，请充值"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer tencent.Close()
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL)
	srv, p, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

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
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("must fail with 503 after hard credit: %d", resp.StatusCode)
	}
	list := p.List()
	if len(list) != 1 || !list[0]["cooling"].(bool) {
		t.Fatalf("account must be cooling: %+v", list)
	}
	wantUntil := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 4, 0, 0, 0, time.Local).AddDate(0, 0, 1)
	until, err := time.Parse(time.RFC3339, list[0]["until"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !until.Equal(wantUntil) {
		t.Fatalf("cooldown must end tomorrow 04:00: got %s want %s", until, wantUntil)
	}
}

// 12153 会话死亡 → 永久禁用 + 重新登录提示（决策 B）。
func Test8eSessionDeadDisable(t *testing.T) {
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	tencent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"msg":"12153 Offline user session not found"}`)
	}))
	defer tencent.Close()
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL)
	srv, p, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	providerChat(t, srv, `{"model":"m","messages":[{"role":"user","content":"q"}]}`)
	list := p.List()
	if len(list) != 1 || !list[0]["disabled"].(bool) {
		t.Fatalf("session-dead account must be disabled: %+v", list)
	}
	if reason, _ := list[0]["reason"].(string); !strings.Contains(reason, "login-tencent") {
		t.Fatalf("disable reason must hint re-login: %q", reason)
	}
}

// /v1/models 按 X-Provider 分家族：workbuddy → 动态 cli 模型；缺省 → 华为路径。
func Test8eModelsProviderAware(t *testing.T) {
	resetModelCaches()
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	tencent := tencent8eUpstream(t, "", nil, "")
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL)
	srv, _, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	getModels := func(provider string) string {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		if provider != "" {
			req.Header.Set("X-Provider", provider)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(raw)
	}
	tb := getModels("workbuddy")
	if !strings.Contains(tb, "kimi-k2.9") || !strings.Contains(tb, `"owned_by":"workbuddy"`) {
		t.Fatalf("workbuddy models must come from cli agent: %s", tb)
	}
	if strings.Contains(tb, "qwen3-vl-235b") {
		t.Fatalf("workbuddy must not expose huawei models: %s", tb)
	}
	// 缺省家族不受影响（华为静态回落，不触达腾讯上游）
	resetModelCaches()
	cd := getModels("")
	if !strings.Contains(cd, "qwen3-vl-235b") || strings.Contains(cd, "kimi-k2.9") {
		t.Fatalf("codearts default models regressed: %s", cd)
	}
}
