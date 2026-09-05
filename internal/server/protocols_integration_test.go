// 三协议端到端集成：/v1/messages 与 /v1/responses 在假上游驱动下的事件序列
// 与非流式对象契约（SPEC §16.2/16.3），以及 inbound.protocols 门控（§13.4）。
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

// postProto 发送各协议端点请求，返回响应体与状态码。
func postProto(t *testing.T, srv *httptest.Server, path, body string) ([]byte, int) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
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

// allProtoProfile 全协议开放的 Profile（覆盖内置 codearts 仅 chat 的门控）。
func allProtoProfile() *adapt.Registry {
	p := *textOnlyTestProfile() // §31：协议归一化测试基底维持 text-only 语义
	p.Inbound.Protocols = []string{"chat", "anthropic", "responses"}
	return adapt.NewRegistry(&p)
}

func protoTestServer(t *testing.T, fake *httptest.Server) *httptest.Server {
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	h.cfg.Profiles = allProtoProfile()
	return srv
}

// sseEvents 解析 SSE 帧为 (event, data) 列表。
func sseEvents(raw string) []struct{ Event, Data string } {
	var out []struct{ Event, Data string }
	cur := ""
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event: "):
			cur = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, struct{ Event, Data string }{cur, strings.TrimPrefix(line, "data: ")})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// anthropic：流式事件序列 + 非流式 Message + count_tokens
// ---------------------------------------------------------------------------

func TestIntegrationAnthropicStreamEvents(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	srv := protoTestServer(t, fake)

	body := `{"model":"glm-5.2","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	raw, code := postProto(t, srv, "/v1/messages", body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	events := sseEvents(string(raw))
	var types []string
	var stopReason string
	text := ""
	for _, ev := range events {
		var p struct {
			Type  string `json:"type"`
			Delta struct {
				StopReason string `json:"stop_reason"`
				TextData   string `json:"text"` // text_delta 增量在 delta.text（type 才是 text_delta）
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(ev.Data), &p)
		types = append(types, p.Type)
		if p.Delta.StopReason != "" {
			stopReason = p.Delta.StopReason
		}
		if p.Delta.TextData != "" {
			text += p.Delta.TextData
		}
	}
	if len(types) < 5 || types[0] != "message_start" {
		t.Fatalf("event types=%v", types)
	}
	if stopReason != "end_turn" || !strings.Contains(text, "完成") {
		t.Fatalf("stop=%q text=%q types=%v", stopReason, text, types)
	}
	if last := types[len(types)-1]; last != "message_stop" {
		t.Fatalf("last event=%q want message_stop", last)
	}
	// content_block_start/delta 必须出现
	if !containsStr(types, "content_block_start") || !containsStr(types, "content_block_delta") {
		t.Fatalf("block events missing: %v", types)
	}
}

func TestIntegrationAnthropicNonStreamObject(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	srv := protoTestServer(t, fake)

	body := `{"model":"glm-5.2","max_tokens":4096,"stream":false,"messages":[{"role":"user","content":"hi"}]}`
	raw, code := postProto(t, srv, "/v1/messages", body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	var msg struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "message" || msg.Role != "assistant" || msg.StopReason != "end_turn" {
		t.Fatalf("msg=%+v", msg)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" {
		t.Fatalf("content=%+v", msg.Content)
	}
}

func TestIntegrationAnthropicToolUse(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(true)})
	srv := protoTestServer(t, fake)

	body := `{"model":"glm-5.2","max_tokens":4096,"stream":false,
		"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],
		"messages":[{"role":"user","content":"读文件"}]}`
	raw, code := postProto(t, srv, "/v1/messages", body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	var msg struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.StopReason != "tool_use" {
		t.Fatalf("stop_reason=%q want tool_use", msg.StopReason)
	}
	found := false
	for _, c := range msg.Content {
		if c.Type == "tool_use" && c.Name == "Read" {
			if c.Input["file_path"] == nil {
				t.Fatalf("input missing: %+v", c.Input)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("tool_use block missing: %+v", msg.Content)
	}
}

func TestIntegrationAnthropicCountTokens(t *testing.T) {
	srv := protoTestServer(t, fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}))
	body := `{"model":"glm-5.2","max_tokens":100,"messages":[{"role":"user","content":"你好世界"}]}`
	raw, code := postProto(t, srv, "/v1/messages/count_tokens", body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.InputTokens <= 0 {
		t.Fatalf("input_tokens=%d want >0 (real estimate)", out.InputTokens)
	}
}

// ---------------------------------------------------------------------------
// responses：流式事件序列 + 非流式 Response
// ---------------------------------------------------------------------------

func TestIntegrationResponsesStreamEvents(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	srv := protoTestServer(t, fake)

	body := `{"model":"glm-5.2","stream":true,"input":"hi"}`
	raw, code := postProto(t, srv, "/v1/responses", body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	events := sseEvents(string(raw))
	var types []string
	for _, ev := range events {
		var p struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(ev.Data), &p)
		types = append(types, p.Type)
	}
	if len(types) < 5 || types[0] != "response.created" {
		t.Fatalf("event types=%v", types)
	}
	if !containsStr(types, "output_text.delta") || lastOf(types) != "response.completed" {
		t.Fatalf("delta/completed missing: %v", types)
	}
}

func TestIntegrationResponsesNonStreamObject(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(true)})
	srv := protoTestServer(t, fake)

	body := `{"model":"glm-5.2","stream":false,
		"tools":[{"type":"function","name":"Read"}],
		"input":[{"type":"message","role":"user","content":"读文件"}]}`
	raw, code := postProto(t, srv, "/v1/responses", body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Fatalf("resp=%+v", resp)
	}
	found := false
	for _, o := range resp.Output {
		if o.Type == "function_call" && o.Name == "Read" {
			if !strings.Contains(o.Arguments, "file_path") {
				t.Fatalf("arguments missing: %+v", o)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("function_call output missing: %+v", resp.Output)
	}
}

// ---------------------------------------------------------------------------
// inbound.protocols 门控（§13.4）
// ---------------------------------------------------------------------------

func TestIntegrationProtocolGating(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	// 内置 codearts 缺省全开（§13.4）；覆盖 profile 收窄时 404
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})

	body := `{"model":"glm-5.2","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	if _, code := postProto(t, srv, "/v1/messages", body); code != 200 {
		t.Fatalf("builtin default must enable all protocols: %d", code)
	}
	if _, code := postProto(t, srv, "/v1/chat/completions", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`); code != 200 {
		t.Fatalf("chat must stay enabled: %d", code)
	}

	// 收窄 Profile：仅 chat
	p := adapt.Codearts
	p.Inbound.Protocols = []string{"chat"}
	h.cfg.Profiles = adapt.NewRegistry(&p)
	if _, code := postProto(t, srv, "/v1/messages", body); code != 404 {
		t.Fatalf("narrowed profile must gate anthropic: %d", code)
	}
	if _, code := postProto(t, srv, "/v1/responses", `{"model":"glm-5.2","input":"hi"}`); code != 404 {
		t.Fatalf("narrowed profile must gate responses: %d", code)
	}
}

// ---------------------------------------------------------------------------
// 跨协议续接（§15.2）：chat 注册指纹 → anthropic 同前缀续接（tail-only 折叠）
// ---------------------------------------------------------------------------

func TestIntegrationCrossProtocolContinue(t *testing.T) {
	var sizes []string
	fake := fakeUpstreamSized(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}, &sizes)
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	h.cfg.Profiles = allProtoProfile()
	h.cfg.SessionMode = "incremental"

	// 第一轮 chat 全量（3 条消息）→ 注册指纹
	chatBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a"},{"role":"user","content":"one"}]}`
	if _, code := postChat(t, srv, chatBody); code != 200 {
		t.Fatalf("chat seed: %d", code)
	}
	// 第二轮 anthropic 回发完整历史 + 增量（4 条）→ 前缀命中续接
	anthBody := `{"model":"glm-5.2","max_tokens":100,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"a"},
		{"role":"user","content":"one"},
		{"role":"user","content":"two"}
	]}`
	if _, code := postProto(t, srv, "/v1/messages", anthBody); code != 200 {
		t.Fatalf("anthropic continue: %d", code)
	}
	if len(sizes) < 2 {
		t.Fatalf("expected >=2 upstream calls, got %d", len(sizes))
	}
	// 跨协议续接生效：第二轮折叠为 tail-only（不含已折叠历史）
	last := sizes[len(sizes)-1]
	if strings.Contains(last, "hi") || strings.Contains(last, "one") {
		t.Fatalf("cross-protocol continue must fold tail-only: %s", last)
	}
	if !strings.Contains(last, "two") {
		t.Fatalf("tail increment must be present: %s", last)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func lastOf(xs []string) string {
	return xs[len(xs)-1]
}
