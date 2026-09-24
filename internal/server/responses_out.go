// OpenAI Responses 出站重建（SPEC §16.3）：协议无关中间流 → /v1/responses
// 事件序列与非流式 Response 对象。流式帧序：response.created/in_progress →
// [message item: output_item.added + content_part.added + output_text.delta…] →
// [function_call item: output_item.added + function_call_arguments.delta] →
// 逐级 done → response.completed；思考增量丢弃（协议无对应公开事件）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"omnigate2api/internal/upstream"
)

// responsesSink /v1/responses 流式 writer。
type responsesSink struct {
	w         http.ResponseWriter
	fl        http.Flusher
	id        string
	model     string
	started   bool
	itemIndex int
	itemType  string // "" | "message" | "function_call"
	itemID    string // 当前 item 的 id（done 事件定位用）
	done      bool
	text      strings.Builder // 聚合文本（response.completed.output 用）
	toolItems []any           // 已完成的 function_call 输出项
	usage     *upstream.Usage // 上游真实用量（随 response.completed 下发）
}

// Usage 收下上游真实用量；Responses 的用量挂在 response.completed.usage。
func (s *responsesSink) Usage(u *upstream.Usage) error {
	s.usage = u
	return nil
}

// responsesUsageJSON Responses 形状的用量：input/output/total + 缓存命中
// （标准位 input_tokens_details.cached_tokens）+ 上游扩展项。
func responsesUsageJSON(u *upstream.Usage) map[string]any {
	if u == nil {
		return nil
	}
	m := map[string]any{
		"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens,
		"total_tokens": u.TotalTokens,
	}
	if u.CachedTokens > 0 {
		m["input_tokens_details"] = map[string]any{"cached_tokens": u.CachedTokens}
	}
	if u.ReasoningTokens > 0 {
		m["output_tokens_details"] = map[string]any{"reasoning_tokens": u.ReasoningTokens}
	}
	if u.Credit > 0 {
		m["credit"] = u.Credit
	}
	return m
}

func newResponsesSink(w http.ResponseWriter, model string) *responsesSink {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	return &responsesSink{w: w, fl: fl, id: "resp_" + randHex(24), model: model}
}

func (s *responsesSink) writeData(payload map[string]any) error {
	raw, _ := json.Marshal(payload)
	if _, err := io.WriteString(s.w, "data: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

func (s *responsesSink) responseObject(status string) map[string]any {
	return map[string]any{
		"id": s.id, "object": "response", "created_at": time.Now().Unix(),
		"status": status, "model": s.model, "output": []any{},
	}
}

func (s *responsesSink) start() error {
	s.started = true
	if err := s.writeData(map[string]any{"type": "response.created", "response": s.responseObject("in_progress")}); err != nil {
		return err
	}
	return s.writeData(map[string]any{"type": "response.in_progress", "response": s.responseObject("in_progress")})
}

// openMessageItem text 增量前：message item + output_text part。
func (s *responsesSink) openMessageItem() error {
	s.itemIndex++
	s.itemID = "msg_" + randHex(16)
	s.itemType = "message"
	if err := s.writeData(map[string]any{
		"type": "output_item.added", "output_index": s.itemIndex,
		"item": map[string]any{
			"id": s.itemID, "type": "message", "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	}); err != nil {
		return err
	}
	return s.writeData(map[string]any{
		"type": "content_part.added", "item_id": s.itemID, "output_index": s.itemIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

// closeItem 关闭当前 item 的逐级 done 事件。
func (s *responsesSink) closeItem() error {
	switch s.itemType {
	case "message":
		if err := s.writeData(map[string]any{"type": "output_text.done", "item_id": s.itemID, "output_index": s.itemIndex, "content_index": 0, "text": s.text.String()}); err != nil {
			return err
		}
		if err := s.writeData(map[string]any{"type": "content_part.done", "item_id": s.itemID, "output_index": s.itemIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}}); err != nil {
			return err
		}
		if err := s.writeData(map[string]any{"type": "output_item.done", "output_index": s.itemIndex, "item": map[string]any{
			"id": s.itemID, "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}},
		}}); err != nil {
			return err
		}
	case "function_call":
		item := s.toolItems[len(s.toolItems)-1]
		if err := s.writeData(map[string]any{"type": "function_call_arguments.done", "item_id": s.itemID, "output_index": s.itemIndex}); err != nil {
			return err
		}
		if err := s.writeData(map[string]any{"type": "output_item.done", "output_index": s.itemIndex, "item": item}); err != nil {
			return err
		}
	}
	s.itemType = ""
	s.itemID = ""
	return nil
}

func (s *responsesSink) Text(t string) error {
	if s.done {
		return nil
	}
	if !s.started {
		if err := s.start(); err != nil {
			return err
		}
	}
	if s.itemType != "message" {
		if s.itemType != "" {
			if err := s.closeItem(); err != nil {
				return err
			}
		}
		if err := s.openMessageItem(); err != nil {
			return err
		}
	}
	s.text.WriteString(t)
	return s.writeData(map[string]any{
		"type": "output_text.delta", "item_id": s.itemID, "output_index": s.itemIndex,
		"content_index": 0, "delta": t,
	})
}

// Reasoning 思考增量：Responses 的 reasoning item 仅服务端出站合成，丢弃。
func (s *responsesSink) Reasoning(t string) error { return nil }

func (s *responsesSink) ToolCall(idx int, c openAIToolCall) error {
	if s.done {
		return nil
	}
	if !s.started {
		if err := s.start(); err != nil {
			return err
		}
	}
	if s.itemType != "" {
		if err := s.closeItem(); err != nil {
			return err
		}
	}
	s.itemIndex++
	s.itemID = "fc_" + randHex(16)
	s.itemType = "function_call"
	item := map[string]any{
		"id": s.itemID, "type": "function_call", "status": "in_progress",
		"call_id": c.ID, "name": c.Name, "arguments": "",
	}
	s.toolItems = append(s.toolItems, item)
	if err := s.writeData(map[string]any{
		"type": "output_item.added", "output_index": s.itemIndex, "item": item,
	}); err != nil {
		return err
	}
	// 完整 arguments 单帧发出（SPEC §16.3：不强制分片）
	return s.writeData(map[string]any{
		"type": "function_call_arguments.delta", "item_id": s.itemID, "output_index": s.itemIndex,
		"delta": c.Arguments,
	})
}

// finishOutput 聚合已发 item 的最终 output 数组（response.completed 用）。
func (s *responsesSink) finishOutput() []any {
	out := []any{}
	if s.text.Len() > 0 {
		out = append(out, map[string]any{
			"id": "msg_" + randHex(16), "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}},
		})
	}
	for _, item := range s.toolItems {
		it := item.(map[string]any)
		it["status"] = "completed"
		out = append(out, it)
	}
	return out
}

func (s *responsesSink) Finish(fin string) error {
	if s.done {
		return nil
	}
	if !s.started {
		if err := s.start(); err != nil {
			return err
		}
	}
	if s.itemType != "" {
		if err := s.closeItem(); err != nil {
			return err
		}
	}
	s.done = true
	resp := map[string]any{
		"id": s.id, "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": s.model, "output": s.finishOutput(),
	}
	if u := responsesUsageJSON(s.usage); u != nil {
		resp["usage"] = u
	}
	return s.writeData(map[string]any{"type": "response.completed", "response": resp})
}

func (s *responsesSink) Error(msg string) error {
	if s.done {
		return nil
	}
	s.done = true
	return s.writeData(map[string]any{
		"type": "error", "error": map[string]any{"code": "upstream_error", "message": msg},
	})
}

// ---------------------------------------------------------------------------
// 非流式 Response 对象
// ---------------------------------------------------------------------------

// buildResponsesResponse 非流式聚合 → Responses Response 对象。
func buildResponsesResponse(model string, content string, calls []openAIToolCall, finish string, usage *upstream.Usage) map[string]any {
	var output []any
	if strings.TrimSpace(content) != "" {
		output = append(output, map[string]any{
			"id": "msg_" + randHex(16), "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}},
		})
	}
	for _, c := range calls {
		output = append(output, map[string]any{
			"id": "fc_" + randHex(16), "type": "function_call", "status": "completed",
			"call_id": c.ID, "name": c.Name, "arguments": c.Arguments,
		})
	}
	return map[string]any{
		"id": "resp_" + randHex(24), "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": model, "output": output,
		"usage": responsesUsageJSON(usage),
	}
}
