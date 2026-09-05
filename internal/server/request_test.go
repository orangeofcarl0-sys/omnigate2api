package server

import (
	"encoding/json"
	"omnigate2api/internal/adapt"
	"strings"
	"testing"
)

func TestBuildUpstreamMessagesNewChat(t *testing.T) {
	req := &chatRequest{
		Messages: []openAIMessage{
			{Role: "system", Text: "be nice"},
			{Role: "user", Text: "hi"},
			{Role: "assistant", Text: "hello"},
			{Role: "user", Text: "again"},
		},
	}
	msgs := buildUpstreamMessages(req, &adapt.Codearts, false, false, nil)
	if len(msgs) != 1 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "be nice") || !strings.Contains(msgs[0].Content, "again") {
		t.Fatalf("full prompt missing pieces: %q", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, "[对话历史]") {
		t.Fatalf("expected history fold for multi-turn: %q", msgs[0].Content)
	}
}

func TestBuildUpstreamMessagesContinueChat(t *testing.T) {
	req := &chatRequest{
		Messages: []openAIMessage{
			{Role: "system", Text: "be nice"},
			{Role: "user", Text: "hi"},
			{Role: "user", Text: "again"},
		},
	}
	msgs := buildUpstreamMessages(req, &adapt.Codearts, false, true, nil)
	if len(msgs) != 1 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	// 续聊只发尾部，不该再塞完整 system+历史
	// （护栏文本会提及 [对话历史] 字样，故检查真实折叠结构才有的换行形态）
	if strings.Contains(msgs[0].Content, "be nice") || strings.Contains(msgs[0].Content, "[对话历史]\n") {
		t.Fatalf("continue should be tail-only: %q", msgs[0].Content)
	}
	// 尾部 = 最新 user 消息 + 收尾护栏（guardBlock）
	if !strings.Contains(msgs[0].Content, "again") || !strings.Contains(msgs[0].Content, "[回复要求]") {
		t.Fatalf("tail=%q", msgs[0].Content)
	}
}

func TestParseChatRequest(t *testing.T) {
	body := `{
		"model":"glm-5.2",
		"stream":true,
		"conversation_id":"conv-1",
		"messages":[
			{"role":"system","content":"be nice"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello"},
			{"role":"user","content":[{"type":"text","text":"again"}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"tools":[{"type":"function","function":{"name":"f1","parameters":{"type":"object","properties":{}}}}],
		"tool_choice":"required"
	}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "glm-5.2" || !req.Stream || req.ConversationID != "conv-1" {
		t.Fatalf("basic fields: %+v", req)
	}
	if len(req.Messages) != 5 {
		t.Fatalf("messages=%d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Text != "be nice" {
		t.Fatalf("sys=%+v", req.Messages[0])
	}
	if req.Messages[3].Text != "again" {
		t.Fatalf("fragmented content=%q", req.Messages[3].Text)
	}
	if req.Messages[4].Role != "tool" || req.Messages[4].ToolCallID != "call_1" {
		t.Fatalf("tool msg=%+v", req.Messages[4])
	}
	if len(req.Tools) != 1 || req.ToolChoice.Mode != "required" {
		t.Fatalf("tools=%v choice=%+v", req.Tools, req.ToolChoice)
	}
}

func TestParseChatRequestErrors(t *testing.T) {
	if _, err := parseChatRequest([]byte(`{}`)); err == nil {
		t.Fatal("expected error for empty messages")
	}
	if _, err := parseChatRequest([]byte(`{"messages":[{"role":"assistant","content":"x"}]}`)); err == nil {
		t.Fatal("expected error: no user/tool message")
	}
}

func TestParseAssistantToolCalls(t *testing.T) {
	body := `{"messages":[{"role":"assistant","content":"","tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}}
	]},{"role":"user","content":"done"}]}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages[0].ToolCalls) != 1 {
		t.Fatalf("tool calls=%d", len(req.Messages[0].ToolCalls))
	}
	c := req.Messages[0].ToolCalls[0]
	if c.ID != "call_1" || c.Name != "f1" || c.Arguments != `{"a":1}` {
		t.Fatalf("call=%+v", c)
	}
}

func TestRawJSONString(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"{\"a\":1}"`, `{"a":1}`},
		{`{"a":1,"b":2}`, `{"a":1,"b":2}`},
		{``, `{}`},
		{`null`, `{}`},
	}
	for _, c := range cases {
		if got := rawJSONString(json.RawMessage(c.in)); got != c.want {
			t.Fatalf("rawJSONString(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// 指纹前缀稳定性：同一消息数组可复算；网关生成的 assistant tool_calls
// 被客户端原样回发时，前缀哈希必须一致（路线 D 续接判定的依据）。
func TestFingerprintStableAndReplay(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "hello"},
		{Role: "user", Text: "again"},
	}
	chain1 := fingerprint(msgs)
	chain2 := fingerprint(msgs)
	if chain1[len(chain1)-1] != chain2[len(chain2)-1] {
		t.Fatalf("fingerprint not stable: %s vs %s", chain1[len(chain1)-1], chain2[len(chain2)-1])
	}
	// 回发复算：base + assistant(tool_calls) 的前缀哈希与单独 base 一致。
	base := []openAIMessage{{Role: "user", Text: "q"}}
	replay := append(append([]openAIMessage{}, base...), openAIMessage{Role: "assistant", Text: "", ToolCalls: []openAIToolCall{
		{ID: "call_x", Name: "f", Arguments: `{"p":1}`},
	}})
	full := fingerprint(replay)
	baseChain := fingerprint(base)
	if full[len(base)-1] != baseChain[len(baseChain)-1] {
		t.Fatalf("replay prefix mismatch: %s vs %s", full[len(base)-1], baseChain[len(baseChain)-1])
	}
}

// TestParseChatContentParts SPEC §30.3：chat 分片 content——文本入 Text、
// 图片结构化入 Images、其余类型块入 Deferred、畸形跳过。
func TestParseChatContentParts(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"a"},
		{"type":"image_url","image_url":{"url":"https://example.com/x.png","detail":"high"}},
		{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,aGk="}},
		{"type":"image_url","image_url":{"url":"not-a-url"}},
		{"type":"file"},
		{"foo":"malformed"}
	]}]}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	m := req.Messages[0]
	if m.Text != "a" {
		t.Fatalf("text=%q", m.Text)
	}
	if len(m.Images) != 2 {
		t.Fatalf("images=%d (%+v)", len(m.Images), m.Images)
	}
	if m.Images[0].URL != "https://example.com/x.png" || m.Images[0].Detail != "high" {
		t.Fatalf("img0=%+v", m.Images[0])
	}
	if m.Images[1].URL != "data:image/jpeg;base64,aGk=" || m.Images[1].Detail != "" {
		t.Fatalf("img1=%+v", m.Images[1])
	}
	// 非法 URL 形态不会入 Images；本例中超限由 TestParseChatImageOversize 单测
	if len(m.Deferred) != 2 || m.Deferred[0].Type != "image" || m.Deferred[1].Type != "file" {
		t.Fatalf("deferred=%+v", m.Deferred)
	}
}

// TestParseChatImageOversize SPEC §30.7：data URI 超 maxImageB64 → 固定占位降级。
func TestParseChatImageOversize(t *testing.T) {
	big := strings.Repeat("A", maxImageB64+1)
	body := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + big + `"}}
	]}]}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	m := req.Messages[0]
	if len(m.Images) != 0 || len(m.Deferred) != 1 || m.Deferred[0].Fixed != deferredOversize {
		t.Fatalf("images=%d deferred=%+v", len(m.Images), m.Deferred)
	}
}

// TestParseChatImageCap SPEC §30.7：单消息图片上限 8，第 9 张起降级占位。
func TestParseChatImageCap(t *testing.T) {
	var parts []string
	for i := 0; i < 10; i++ {
		parts = append(parts, `{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}`)
	}
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"imgs"},` +
		strings.Join(parts, ",") + `]}]}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	m := req.Messages[0]
	if len(m.Images) != maxImagesPerMsg {
		t.Fatalf("images=%d", len(m.Images))
	}
	if len(m.Deferred) != 2 {
		t.Fatalf("deferred=%d", len(m.Deferred))
	}
}

// TestDetailWhitelist SPEC §30.5：detail 仅 auto/low/high 透传。
func TestDetailWhitelist(t *testing.T) {
	for d, want := range map[string]string{"auto": "auto", "low": "low", "high": "high", "bogus": "", "": ""} {
		if got := normalizeImageDetail(d); got != want {
			t.Fatalf("detail %q → %q, want %q", d, got, want)
		}
	}
}

// TestMediaPlaceholderTriProtocol SPEC §30 验收：三协议对同一图片消息归一化为
// 结构化 Images，placeholder 渲染后折叠出同模板占位（canonical type=image）。
func TestMediaPlaceholderTriProtocol(t *testing.T) {
	ph := mediaTemplate(nil)

	// chat 线
	chatReq, err := parseChatRequest([]byte(`{"model":"glm-5.2","messages":[
		{"role":"user","content":[
			{"type":"text","text":"看这张图"},
			{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}
		]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// anthropic 线
	anReq, err := parseAnthropicRequest([]byte(`{"max_tokens":100,"messages":[{"role":"user","content":[
		{"type":"text","text":"看这张图"},
		{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}
	]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// responses 线
	rsReq, err := parseResponsesRequest([]byte(`{"model":"glm-5.2","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"看这张图"},
			{"type":"input_image","image_url":"https://example.com/x.png"}
		]}]}`))
	if err != nil {
		t.Fatal(err)
	}

	for _, req := range []*chatRequest{chatReq, anReq, rsReq} {
		if len(req.Messages[0].Images) != 1 || req.Messages[0].Images[0].URL != "https://example.com/x.png" {
			t.Fatalf("parse images=%+v", req.Messages[0].Images)
		}
		renderMedia(req.Messages, "placeholder", ph, nil)
		if !strings.Contains(req.Messages[0].Text, "看这张图") ||
			!strings.Contains(req.Messages[0].Text, "[用户发送了一个附件：image]") {
			t.Fatalf("placeholder missing: %q", req.Messages[0].Text)
		}
		if len(req.Messages[0].Images) != 0 {
			t.Fatal("placeholder mode must clear images")
		}
	}
}
