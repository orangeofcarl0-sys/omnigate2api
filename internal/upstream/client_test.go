package upstream

import (
	"encoding/json"
	"testing"
)

// MarshalJSON 序列化保真（SPEC §22.1 + §30.5）：roles 语义、空 Role 兜底 user、
// 分片数组双形态。
func TestChatMessageMarshal(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "", ToolCalls: []ChatToolCall{{ID: "c1", Name: "f", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "r"},
		{Content: "folded"}, // Role 空 → 兜底 user（text-only 折叠路径）
	}
	// 无分片：string content 零回归
	out, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 4 {
		t.Fatalf("len=%d", len(parsed))
	}
	if parsed[0]["role"] != "user" || parsed[0]["content"] != "hi" {
		t.Fatalf("user=%v", parsed[0])
	}
	tc := parsed[1]["tool_calls"].([]any)
	tc0 := tc[0].(map[string]any)
	if tc0["id"] != "c1" || tc0["type"] != "function" {
		t.Fatalf("tool_calls=%v", tc0)
	}
	fn := tc0["function"].(map[string]any)
	if fn["name"] != "f" || fn["arguments"] != `{}` {
		t.Fatalf("function=%v", fn)
	}
	if parsed[2]["tool_call_id"] != "c1" {
		t.Fatalf("tool_call_id=%v", parsed[2])
	}
	if parsed[3]["role"] != "user" || parsed[3]["content"] != "folded" {
		t.Fatalf("empty role must default to user: %v", parsed[3])
	}
	// 有分片：content 数组，文本合首片
	img, err := json.Marshal(ChatMessage{Role: "user", Content: "look", ContentParts: []ChatContentPart{{
		Type: "image_url", ImageURL: &ChatImageURL{URL: "data:image/png;base64,aGk="},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var pm struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(img, &pm); err != nil {
		t.Fatal(err)
	}
	if len(pm.Content) != 2 || pm.Content[0]["type"] != "text" || pm.Content[0]["text"] != "look" {
		t.Fatalf("parts=%s", img)
	}
	if pm.Content[1]["type"] != "image_url" {
		t.Fatalf("image part=%v", pm.Content[1])
	}
}
