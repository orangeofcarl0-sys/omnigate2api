package server

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// anthropic 归一化（SPEC §13.2）
// ---------------------------------------------------------------------------

func TestParseAnthropicBasic(t *testing.T) {
	body := `{
		"model":"glm-5.2","stream":true,"max_tokens":4096,
		"system":"be nice",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"text","text":"hello"}]},
			{"role":"user","content":"again"}
		]
	}`
	req, err := parseAnthropicRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages=%d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Text != "be nice" {
		t.Fatalf("system=%+v", req.Messages[0])
	}
	if req.Messages[1].Text != "hi" || req.Messages[2].Text != "hello" {
		t.Fatalf("msgs=%+v", req.Messages)
	}
	if !req.Stream || req.Model != "glm-5.2" {
		t.Fatalf("req=%+v", req)
	}
}

func TestParseAnthropicSystemBlocks(t *testing.T) {
	body := `{"max_tokens":100,"messages":[{"role":"user","content":"q"}],
		"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`
	req, err := parseAnthropicRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Text != "ab" {
		t.Fatalf("system=%q", req.Messages[0].Text)
	}
}

func TestParseAnthropicToolUseAndResult(t *testing.T) {
	body := `{"max_tokens":100,"messages":[
		{"role":"user","content":"改文件"},
		{"role":"assistant","content":[{"type":"text","text":"先读"},{"type":"tool_use","id":"tu_1","name":"Read","input":{"file_path":"F:/x/a.mjs"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"alpha = 1"},{"type":"text","text":"继续"}]}
	]}`
	req, err := parseAnthropicRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages=%d %+v", len(req.Messages), req.Messages)
	}
	as := req.Messages[1]
	if as.Role != "assistant" || as.Text != "先读" || len(as.ToolCalls) != 1 {
		t.Fatalf("assistant=%+v", as)
	}
	tc := as.ToolCalls[0]
	if tc.ID != "tu_1" || tc.Name != "Read" || tc.Arguments != `{"file_path":"F:/x/a.mjs"}` {
		t.Fatalf("tool call=%+v", tc)
	}
	tr := req.Messages[2]
	if tr.Role != "tool" || tr.ToolCallID != "tu_1" || tr.Text != "alpha = 1" {
		t.Fatalf("tool result=%+v", tr)
	}
	if req.Messages[3].Role != "user" || req.Messages[3].Text != "继续" {
		t.Fatalf("tail user=%+v", req.Messages[3])
	}
}

func TestParseAnthropicImagePlaceholder(t *testing.T) {
	body := `{"max_tokens":100,"messages":[{"role":"user","content":[
		{"type":"text","text":"看这张图"},
		{"type":"image","source":{"type":"base64"}}
	]}]}`
	req, err := parseAnthropicRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Messages[0].Text, "看这张图") || !strings.Contains(req.Messages[0].Text, "[用户发送了一个附件：image]") {
		t.Fatalf("media placeholder missing: %q", req.Messages[0].Text)
	}
}

func TestParseAnthropicThinkingDropped(t *testing.T) {
	body := `{"max_tokens":100,"messages":[{"role":"user","content":"q"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"secret chain"},{"type":"text","text":"答"}]}]}`
	req, err := parseAnthropicRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("thinking must be dropped: %+v", req.Messages)
	}
	if strings.Contains(req.Messages[1].Text, "secret chain") {
		t.Fatal("thinking leaked into text")
	}
}

func TestParseAnthropicTools(t *testing.T) {
	body := `{"max_tokens":100,"messages":[{"role":"user","content":"q"}],
		"tools":[{"name":"f1","description":"d","input_schema":{"type":"object","properties":{}}}],
		"tool_choice":{"type":"tool","name":"f1"}}`
	req, err := parseAnthropicRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tools=%v", req.Tools)
	}
	fn, _ := req.Tools[0]["function"].(map[string]any)
	if fn["name"] != "f1" || fn["description"] != "d" {
		t.Fatalf("tool=%v", req.Tools[0])
	}
	if req.ToolChoice.Mode != "function" || req.ToolChoice.Function != "f1" {
		t.Fatalf("choice=%+v", req.ToolChoice)
	}
}

func TestParseAnthropicToolChoiceModes(t *testing.T) {
	for raw, want := range map[string]string{
		`"auto"`: "auto", `"any"`: "required", `"none"`: "none",
		`{"type":"auto"}`: "auto", `{"type":"any"}`: "required",
		`{"type":"none"}`: "none", `{"type":"tool","name":"x"}`: "function",
	} {
		tc := parseAnthropicToolChoice([]byte(raw))
		if tc.Mode != want {
			t.Fatalf("tool_choice %s → mode=%q want %q", raw, tc.Mode, want)
		}
	}
}

func TestParseAnthropicErrors(t *testing.T) {
	if _, err := parseAnthropicRequest([]byte(`{"max_tokens":100}`), nil); err == nil {
		t.Fatal("empty messages must error")
	}
	if _, err := parseAnthropicRequest([]byte(`{"messages":[{"role":"user","content":"q"}]}`), nil); err == nil {
		t.Fatal("missing max_tokens must error")
	}
	if _, err := parseAnthropicRequest([]byte(`{"max_tokens":100,"messages":[{"role":"assistant","content":"x"}]}`), nil); err == nil {
		t.Fatal("no sendable message must error")
	}
}

// ---------------------------------------------------------------------------
// responses 归一化（SPEC §13.2）
// ---------------------------------------------------------------------------

func TestParseResponsesBasic(t *testing.T) {
	body := `{"model":"glm-5.2","stream":true,"instructions":"be nice","input":"hi"}`
	req, err := parseResponsesRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("messages=%+v", req.Messages)
	}
	if !req.Stream || req.Model != "glm-5.2" {
		t.Fatalf("req=%+v", req)
	}
}

func TestParseResponsesFunctionCallMerge(t *testing.T) {
	body := `{"input":[
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"读文件"}]},
		{"type":"function_call","call_id":"fc_1","name":"Read","arguments":"{\"file_path\":\"F:/x/a.mjs\"}"},
		{"type":"function_call_output","call_id":"fc_1","output":"alpha = 1"},
		{"type":"message","role":"user","content":"继续"}
	]}`
	req, err := parseResponsesRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages=%d %+v", len(req.Messages), req.Messages)
	}
	as := req.Messages[0]
	if as.Role != "assistant" || as.Text != "读文件" || len(as.ToolCalls) != 1 {
		t.Fatalf("merged assistant=%+v", as)
	}
	tc := as.ToolCalls[0]
	if tc.ID != "fc_1" || tc.Name != "Read" || tc.Arguments != `{"file_path":"F:/x/a.mjs"}` {
		t.Fatalf("tool call=%+v", tc)
	}
	tr := req.Messages[1]
	if tr.Role != "tool" || tr.ToolCallID != "fc_1" || tr.Text != "alpha = 1" {
		t.Fatalf("tool output=%+v", tr)
	}
	if req.Messages[2].Role != "user" || req.Messages[2].Text != "继续" {
		t.Fatalf("tail=%+v", req.Messages[2])
	}
}

func TestParseResponsesRoles(t *testing.T) {
	body := `{"input":[
		{"type":"message","role":"developer","content":"dev rules"},
		{"type":"message","role":"user","content":"q"}
	]}`
	req, err := parseResponsesRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Text != "dev rules" {
		t.Fatalf("developer→system: %+v", req.Messages[0])
	}
}

func TestParseResponsesTools(t *testing.T) {
	body := `{"input":"q","tools":[
		{"type":"function","name":"f1","description":"d","parameters":{"type":"object"},"strict":true}
	],"tool_choice":{"type":"function","name":"f1"}}`
	req, err := parseResponsesRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tools=%v", req.Tools)
	}
	fn, _ := req.Tools[0]["function"].(map[string]any)
	if fn["name"] != "f1" || fn["strict"] != true {
		t.Fatalf("tool=%v", req.Tools[0])
	}
	if req.ToolChoice.Mode != "function" || req.ToolChoice.Function != "f1" {
		t.Fatalf("choice=%+v", req.ToolChoice)
	}
}

func TestParseResponsesReasoningDropped(t *testing.T) {
	body := `{"input":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},
		{"type":"message","role":"user","content":"q"}
	]}`
	req, err := parseResponsesRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("reasoning must be dropped: %+v", req.Messages)
	}
}

func TestParseResponsesContentParts(t *testing.T) {
	body := `{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_image","image_url":"x"}]}
	]}`
	req, err := parseResponsesRequest([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := req.Messages[0].Text
	if !strings.Contains(text, "a") || !strings.Contains(text, "[用户发送了一个附件：input_image]") {
		t.Fatalf("parts=%q", text)
	}
}
