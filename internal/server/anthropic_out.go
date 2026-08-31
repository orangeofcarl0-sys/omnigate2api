// Anthropic 出站重建（SPEC §16.2）：协议无关中间流 → /v1/messages 事件序列
// 与非流式 Message 对象。流式帧序：message_start → [text 块] → [tool_use 块]
// → message_delta(stop_reason) → message_stop；思考增量丢弃（协议无公开事件）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// anthropicSink /v1/messages 流式 writer。
type anthropicSink struct {
	w          http.ResponseWriter
	fl         http.Flusher
	id         string
	model      string
	started    bool
	blockIndex int
	textOpen   bool
	toolOpen   bool
	done       bool
}

func newAnthropicSink(w http.ResponseWriter, model string) *anthropicSink {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	return &anthropicSink{w: w, fl: fl, id: "msg_" + randHex(24), model: model}
}

func (s *anthropicSink) writeEvent(event string, payload map[string]any) error {
	raw, _ := json.Marshal(payload)
	if _, err := io.WriteString(s.w, "event: "+event+"\ndata: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

// start 首个事件必须为 message_start（含流错误前的空流终止）。
func (s *anthropicSink) start() error {
	s.started = true
	return s.writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.id, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *anthropicSink) openText() error {
	if err := s.writeEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         s.blockIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	}); err != nil {
		return err
	}
	s.blockIndex++
	s.textOpen = true
	return nil
}

func (s *anthropicSink) openToolUse(c openAIToolCall) error {
	if err := s.writeEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         s.blockIndex,
		"content_block": map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": map[string]any{}},
	}); err != nil {
		return err
	}
	s.blockIndex++
	s.toolOpen = true
	return nil
}

func (s *anthropicSink) closeBlock() error {
	if !s.textOpen && !s.toolOpen {
		return nil
	}
	index := s.blockIndex - 1
	if err := s.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
		return err
	}
	s.textOpen = false
	s.toolOpen = false
	return nil
}

func (s *anthropicSink) Text(t string) error {
	if s.done {
		return nil
	}
	if !s.started {
		if err := s.start(); err != nil {
			return err
		}
	}
	if !s.textOpen {
		if s.toolOpen {
			// 工具块之后的正文：回合语义已收束（post 抑制），此处防御性忽略
			return nil
		}
		if err := s.openText(); err != nil {
			return err
		}
	}
	return s.writeEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.blockIndex - 1,
		"delta": map[string]any{"type": "text_delta", "text": t},
	})
}

// Reasoning 思考增量：Anthropic 无公开事件（thinking delta 需协商头），丢弃。
func (s *anthropicSink) Reasoning(t string) error { return nil }

func (s *anthropicSink) ToolCall(idx int, c openAIToolCall) error {
	if s.done {
		return nil
	}
	if !s.started {
		if err := s.start(); err != nil {
			return err
		}
	}
	if s.textOpen {
		if err := s.closeBlock(); err != nil {
			return err
		}
	} else if s.toolOpen {
		if err := s.closeBlock(); err != nil {
			return err
		}
	}
	if err := s.openToolUse(c); err != nil {
		return err
	}
	// 完整 arguments 单帧发出（模拟层一次性解析，不强制分片，SPEC §16.2）
	return s.writeEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.blockIndex - 1,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": c.Arguments},
	})
}

func (s *anthropicSink) Finish(fin string) error {
	if s.done {
		return nil
	}
	if !s.started {
		if err := s.start(); err != nil {
			return err
		}
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.done = true
	if err := s.writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopReason(fin), "stop_sequence": nil},
	}); err != nil {
		return err
	}
	return s.writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

// anthropicStopReason finish 映射（SPEC §16.2）：stop→end_turn / tool_calls→tool_use / length→max_tokens。
func anthropicStopReason(fin string) string {
	switch fin {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func (s *anthropicSink) Error(msg string) error {
	if s.done {
		return nil
	}
	s.done = true
	return s.writeEvent("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": msg},
	})
}

// ---------------------------------------------------------------------------
// 非流式 Message 对象
// ---------------------------------------------------------------------------

// buildAnthropicMessage 非流式聚合 → Anthropic Message。
// 工具 arguments（JSON 字符串）反序列化为 input 对象；usage 为估算值。
func buildAnthropicMessage(model string, content string, calls []openAIToolCall, finish string, usage map[string]int) map[string]any {
	var blocks []any
	if strings.TrimSpace(content) != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": content})
	}
	for _, c := range calls {
		var input any
		if err := json.Unmarshal([]byte(c.Arguments), &input); err != nil {
			input = map[string]any{}
		}
		blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": input})
	}
	u := map[string]any{"input_tokens": usage["prompt_tokens"], "output_tokens": usage["completion_tokens"]}
	return map[string]any{
		"id": "msg_" + randHex(24), "type": "message", "role": "assistant",
		"model": model, "content": blocks,
		"stop_reason": anthropicStopReason(finish), "stop_sequence": nil,
		"usage": u,
	}
}

// anthropicCountTokensEstim 估算输入 tokens（SPEC §16.2 真估算，替代恒 0 stub；
// 与 chat usageEstimate 同规则：文本/arguments 按 len/4+1 累计）。
func anthropicCountTokensEstim(msgs []openAIMessage) int {
	total := 0
	for _, m := range msgs {
		total += tokensApprox(m.Text)
		for _, c := range m.ToolCalls {
			total += tokensApprox(c.Arguments)
		}
	}
	return total
}

// anthropicCountTokens /v1/messages/count_tokens：基于归一化消息的估算
// （SPEC §16.2 真估算，与 usageEstimate 同规则）。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, tooLarge, err := readBody(r)
	if err != nil || tooLarge {
		writeProtoError(protoAnthropic, w, http.StatusBadRequest, "invalid_request", "bad request body")
		return
	}
	req, err := parseAnthropicRequest(body, nil)
	if err != nil {
		writeProtoError(protoAnthropic, w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": anthropicCountTokensEstim(req.Messages)})
}
