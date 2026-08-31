// roles 透传路径测试（SPEC §22）：归一化数组 → 原生 OpenAI messages 保真，
// text-only 路径零回归由既有测试保证。
package server

import (
	"net/http"
	"strings"
	"testing"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/auth"
)

// rolesProfile Message.Model="roles" 的 Profile（echo 风格）。
func rolesProfile() *adapt.Registry {
	p := adapt.Codearts
	p.Message.Model = "roles"
	p.Message.Folding = nil
	p.Session.Kind = "none"
	p.Tool = adapt.ToolProfile{}
	return adapt.NewRegistry(&p)
}

func TestRenderRolesMessages(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "system", Text: "be nice"},
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "ok", ToolCalls: []openAIToolCall{{ID: "call_1", Name: "Read", Arguments: `{"p":1}`}}},
		{Role: "tool", ToolCallID: "call_1", Text: "result"},
	}
	out := renderRolesMessages(msgs)
	if len(out) != 4 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0].Role != "system" || out[0].Content != "be nice" {
		t.Fatalf("system=%+v", out[0])
	}
	if len(out[2].ToolCalls) != 1 || out[2].ToolCalls[0].ID != "call_1" || out[2].ToolCalls[0].Arguments != `{"p":1}` {
		t.Fatalf("assistant=%+v", out[2])
	}
	if out[3].Role != "tool" || out[3].ToolCallID != "call_1" || out[3].Content != "result" {
		t.Fatalf("tool=%+v", out[3])
	}
}

func TestBuildUpstreamMessagesRolesVsTextOnly(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "system", Text: "s"},
		{Role: "user", Text: "u"},
		{Role: "assistant", Text: "a", ToolCalls: []openAIToolCall{{ID: "c1", Name: "f", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "c1", Text: "r"},
	}
	req := &chatRequest{Messages: msgs}

	roles := rolesProfile()
	rm := buildUpstreamMessages(req, roles.Get("codearts"), false, false, nil)
	if len(rm) != 4 || rm[2].Role != "assistant" {
		t.Fatalf("roles must pass through 4 messages: %+v", rm)
	}

	textOnly := buildUpstreamMessages(req, &adapt.Codearts, false, false, nil)
	if len(textOnly) != 1 || textOnly[0].Role != "user" {
		t.Fatalf("text-only must fold to single user message: %+v", textOnly)
	}
}

// 集成：roles Profile 的请求上送体为原生 messages（role/tool_calls/tool_call_id 保真）。
func TestIntegrationRolesOutboundBody(t *testing.T) {
	var sizes []string
	fake := fakeUpstreamSized(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}, &sizes)
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	h.cfg.Profiles = rolesProfile()

	body := `{"model":"glm-5.2","messages":[
		{"role":"system","content":"be nice"},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"ok","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"result"}
	]}`
	if _, code := postChat(t, srv, body); code != 200 {
		t.Fatalf("status=%d", code)
	}
	last := sizes[len(sizes)-1]
	if !strings.Contains(last, `"role":"system"`) || !strings.Contains(last, `"role":"tool"`) {
		t.Fatalf("roles must pass through roles: %s", last)
	}
	if !strings.Contains(last, `"tool_call_id":"call_1"`) {
		t.Fatalf("tool_call_id must pass through: %s", last)
	}
	// 不得出现折叠标记（roles 不经折叠渲染）
	if strings.Contains(last, "[对话历史]") || strings.Contains(last, "[系统指令]") {
		t.Fatalf("roles must not fold: %s", last)
	}
}
