// 用量（usage）端到端：上游真实 token 用量要贯通三种协议与流式/非流式两条路径。
//
// 修复前：上游终帧带的 `usage` 被解析层丢弃，非流式只能拿 len/4+1 的**估算**，
// 流式则完全不报用量（Anthropic 的 message_delta 无 usage、Responses 的
// response.completed 也无 usage）——客户端"看 token"的需求落空。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 上游真实用量（刻意远离估算值，便于区分"真实"与"len/4+1 估算"）。
const (
	upPromptTokens = 1234
	upComplTokens  = 567
	upTotalTokens  = 1801
)

// usageServer 假上游：正文 + 终帧携带真实 usage（protoTestServer 已配好三协议 Profile）。
func usageServer(t *testing.T) *httptest.Server {
	t.Helper()
	stream := orderFrame(map[string]any{"content": "你好"}, "") +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1234,"completion_tokens":567,"total_tokens":1801}}` + "\n\n" +
		"data: [DONE]\n\n"
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}})
	return protoTestServer(t, fake)
}

// 非流式：usage 必须是上游真实值，而不是估算。
func TestUsageNonStreamUsesUpstreamNumbers(t *testing.T) {
	srv := usageServer(t)
	body := `{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"你好"}]}`
	raw, code := postChat(t, srv, body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, truncateText(string(raw), 200))
	}
	var resp struct {
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Usage["prompt_tokens"] != upPromptTokens || resp.Usage["completion_tokens"] != upComplTokens ||
		resp.Usage["total_tokens"] != upTotalTokens {
		t.Fatalf("usage must be the upstream's real numbers, got %v", resp.Usage)
	}
}

// 流式 + stream_options.include_usage：终止帧前追加只带 usage 的 chunk（OpenAI 语义）。
func TestUsageStreamIncludeUsage(t *testing.T) {
	srv := usageServer(t)
	body := `{"model":"glm-5.2","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"你好"}]}`
	raw, code := postChat(t, srv, body)
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	out := string(raw)
	usageAt := strings.Index(out, `"usage"`)
	if usageAt < 0 {
		t.Fatalf("usage chunk missing:\n%s", truncateText(out, 500))
	}
	if !strings.Contains(out[usageAt:], `"prompt_tokens":1234`) {
		t.Fatalf("usage chunk must carry real numbers:\n%s", truncateText(out[usageAt:], 300))
	}
	if doneAt := strings.Index(out, "[DONE]"); doneAt < 0 || usageAt > doneAt {
		t.Fatal("usage chunk must precede [DONE]")
	}
	// usage chunk 的 choices 必须为空（OpenAI 该帧的形态）
	seg := out[:usageAt]
	if !strings.Contains(seg[strings.LastIndex(seg, "data: "):], `"choices":[]`) {
		t.Fatalf("usage chunk must have empty choices:\n%s", truncateText(seg, 200))
	}
}

// 未请求 include_usage：不下发 usage chunk（与 OpenAI 行为一致，不给严格客户端添乱）。
func TestUsageStreamWithoutIncludeUsage(t *testing.T) {
	srv := usageServer(t)
	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"你好"}]}`
	raw, _ := postChat(t, srv, body)
	if strings.Contains(string(raw), `"usage"`) {
		t.Fatalf("usage chunk must not be sent unless include_usage is requested:\n%s", truncateText(string(raw), 400))
	}
}

// Anthropic 流：message_delta.usage 报真实 input/output tokens。
func TestUsageAnthropicMessageDelta(t *testing.T) {
	srv := usageServer(t)
	body := `{"model":"glm-5.2","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"你好"}]}`
	raw, code := postProto(t, srv, "/v1/messages", body)
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	out := string(raw)
	idx := strings.Index(out, "event: message_delta")
	if idx < 0 {
		t.Fatalf("message_delta missing:\n%s", truncateText(out, 500))
	}
	tail := out[idx:]
	if !strings.Contains(tail, `"input_tokens":1234`) || !strings.Contains(tail, `"output_tokens":567`) {
		t.Fatalf("message_delta must carry real usage:\n%s", truncateText(tail, 300))
	}
}

// Responses 流：response.completed.usage 报真实用量。
func TestUsageResponsesCompleted(t *testing.T) {
	srv := usageServer(t)
	body := `{"model":"glm-5.2","stream":true,"input":"你好"}`
	raw, code := postProto(t, srv, "/v1/responses", body)
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	out := string(raw)
	// 锚点取**含 response.completed 的那一行**：usage 在同行的 response 对象里、
	// 位于 "type":"response.completed" 之前，用它之后的后缀做断言必然取空。
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, `"type":"response.completed"`) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("response.completed missing:\n%s", truncateText(out, 500))
	}
	if !strings.Contains(line, `"usage":{"input_tokens":1234`) || !strings.Contains(line, `"total_tokens":1801`) {
		t.Fatalf("response.completed must carry real usage:\n%s", truncateText(line, 400))
	}
}
