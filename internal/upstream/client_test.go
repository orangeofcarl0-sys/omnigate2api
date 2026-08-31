package upstream

import "testing"

// marshalChatMessages 序列化保真（SPEC §22.1）：roles 语义 + 空 Role 兜底 user。
func TestMarshalChatMessages(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "", ToolCalls: []ChatToolCall{{ID: "c1", Name: "f", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "r"},
		{Content: "folded"}, // Role 空 → 兜底 user（text-only 折叠路径）
	}
	out := marshalChatMessages(msgs)
	if len(out) != 4 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0]["role"] != "user" || out[0]["content"] != "hi" {
		t.Fatalf("user=%v", out[0])
	}
	tc := out[1]["tool_calls"].([]map[string]any)
	if tc[0]["id"] != "c1" || tc[0]["type"] != "function" {
		t.Fatalf("tool_calls=%v", tc)
	}
	fn := tc[0]["function"].(map[string]any)
	if fn["name"] != "f" || fn["arguments"] != `{}` {
		t.Fatalf("function=%v", fn)
	}
	if out[2]["tool_call_id"] != "c1" {
		t.Fatalf("tool_call_id=%v", out[2])
	}
	if out[3]["role"] != "user" || out[3]["content"] != "folded" {
		t.Fatalf("empty role must default to user: %v", out[3])
	}
}
