// CodeArts /v1/chat/chat SSE → OpenAI chat.completion 转换。
//
// 标准 SSE（event: <name> + data: <json>）。常见事件（逆向自 vscode-codebot）：
//   - onAnswer:  {"text":"增量","custom_extra_info":...}
//   - reasoning / thinking: 思考链增量
//   - done:      {"error_code":"0","error_msg":"","related_question_answer":[...],"response_message_id":"..."}
//     related_question_answer 是「相关追问」建议，不是正文，解析时忽略。
//   - error / heartbeat 等
//
// 另：部分帧可能直接给出 structured QA 对象 {question,options,answer}（无 text 字段）。
// 仅在正文仍为空时提取 answer，避免把整段 JSON 当回复；已有 text 时绝不覆盖。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// UpstreamError 流内业务错误。
type UpstreamError struct {
	Code string
	Msg  string
}

func (e *UpstreamError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("codearts error code=%s msg=%s", e.Code, e.Msg)
	}
	return fmt.Sprintf("codearts error: %s", e.Msg)
}

// RawCompletion 聚合结果。
type RawCompletion struct {
	Content   string
	Reasoning string
	Finish    string
	ToolCalls []ToolCall // 原生工具调用（delta.tool_calls 增量拼装；文本围栏路径为空）
}

// ToolCall 拼装完成的原生流式工具调用（OpenAI delta.tool_calls 按 index 合并）。
type ToolCall struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// mergeToolCallDeltas 合并 OpenAI 增量工具调用帧（SPEC §28.4 决策 D）：按 index
// 拼接 id/name/arguments 片段（name/id 非空覆盖、arguments 追加），流尾统一以
// 单帧完整参数发出。缺 index 的帧按出现顺序追加。
func mergeToolCallDeltas(stitch map[int]*ToolCall, delta map[string]any) {
	tcs, ok := delta["tool_calls"].([]any)
	if !ok {
		return
	}
	for _, raw := range tcs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		index := -1
		if i, ok := m["index"].(float64); ok {
			index = int(i)
		}
		if index < 0 {
			index = len(stitch)
			for {
				if _, exists := stitch[index]; !exists {
					break
				}
				index++
			}
		}
		tc := stitch[index]
		if tc == nil {
			tc = &ToolCall{Index: index}
			stitch[index] = tc
		}
		if id, ok := m["id"].(string); ok && id != "" {
			tc.ID = id
		}
		if fn, ok := m["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				tc.Name = name
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				tc.Arguments += args
			}
		}
	}
}

// stitchedCalls 按 index 升序返回拼装完成的工具调用。
func stitchedCalls(stitch map[int]*ToolCall) []ToolCall {
	if len(stitch) == 0 {
		return nil
	}
	idxs := make([]int, 0, len(stitch))
	for i := range stitch {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	out := make([]ToolCall, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, *stitch[i])
	}
	return out
}

// scanLine 处理一行 SSE：CodeArts 是逐行 data:（无空行分隔），
// 兼容标准 event:/data: 空行分隔两种形态。
// 返回 (event, data, 是否触发事件)。event 为空表示无 event 前缀。
func scanLine(line string, pendingEvent *string) (event, data string, ok bool) {
	line = strings.TrimRight(line, "\r")
	switch {
	case strings.HasPrefix(line, "event:"):
		*pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		return "", "", false
	case strings.HasPrefix(line, "data:"):
		d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if d == "" {
			return "", "", false
		}
		ev := *pendingEvent
		*pendingEvent = ""
		return ev, d, true
	case strings.HasPrefix(line, ":"):
		// 注释
		return "", "", false
	case line == "":
		*pendingEvent = ""
		return "", "", false
	}
	return "", "", false
}

// isValidStructuredQA 检查是否为有效的结构化问答对象。
// 避免将代码片段或 other structured content误认为是问答结构。
func isValidStructuredQA(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	// 检查是否包含必要的问答字段
	question, hasQ := obj["question"]
	answer, hasA := obj["answer"]
	_, hasOptions := obj["options"]

	// 必须同时有 question 和 answer 字段
	if !hasQ || !hasA {
		return false
	}

	// 确保字段是字符串类型
	qStr, qOk := question.(string)
	aStr, aOk := answer.(string)
	if !qOk || !aOk {
		return false
	}

	// 检查是否是典型的问答格式（避免代码块）
	// Question should be a natural language question
	if len(qStr) == 0 || len(qStr) > 500 { // 问题不应该太长
		return false
	}

	// Answer should be relatively short and not contain code-like structures
	if len(aStr) == 0 || len(aStr) > 100 { // 答案不应该太长
		return false
	}

	// Check if it looks like code (contains common programming keywords)
	lowerAns := strings.ToLower(aStr)
	if strings.Contains(lowerAns, "import ") || strings.Contains(lowerAns, "def ") ||
		strings.Contains(lowerAns, "class ") || strings.Contains(lowerAns, "func ") ||
		strings.Contains(lowerAns, "package ") || strings.Contains(lowerAns, "module ") ||
		strings.Contains(lowerAns, "struct ") || strings.Contains(lowerAns, "interface ") {
		return false
	}

	// If options are present, they should be a slice
	if hasOptions {
		opts, optsOk := obj["options"].([]any)
		if !optsOk {
			return false
		}
		// Options should be reasonable (not too many, not too long)
		if len(opts) > 10 || len(opts) < 2 {
			return false
		}
	}

	return true
}

// deltaFromChunk 从 OpenAI 兼容 chunk 中提取 delta 与 finish_reason。
// 标准格式：{"choices":[{"delta":{...},"finish_reason":"stop"}]}。
// finish_reason 与 delta 可同帧（OpenAI 终止帧常为 delta:{} + finish_reason，
// 腾讯原生流即此形态）——两者都返回，由调用方先记 finish 再处理 delta。
func deltaFromChunk(payload map[string]any) (delta map[string]any, finishReason string) {
	if d, ok := payload["delta"].(map[string]any); ok {
		return d, ""
	}
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, ""
	}
	if c0, ok := choices[0].(map[string]any); ok {
		if d, ok := c0["delta"].(map[string]any); ok {
			return d, finishReasonOf(c0)
		}
		return nil, finishReasonOf(c0)
	}
	return nil, ""
}

// finishReasonOf 取 choice 帧的 finish_reason。
func finishReasonOf(c0 map[string]any) string {
	if fr, ok := c0["finish_reason"].(string); ok {
		return fr
	}
	return ""
}

// applyEvent 把单事件应用到聚合状态。
//
// 刻意不做 helper 拆分：本函数的核心约束是判定顺序的隐式依赖（usage 终帧
// 必须在 text 快照前、快照替换必须在结构化 QA 前），拆散会把依赖藏进
// helper 之间的调用序，反而更难审；且新上游的方言适配走 Profile 声明
// （DeltaEvents/Terminators），本函数没有持续膨胀的趋势。触发条件：需要
// 直接支持第二种事件族（未经归一化）时，按事件族拆分。
func applyEvent(content, reason *strings.Builder, finish *string, upErr *error, stitch map[int]*ToolCall, event, data string) {
	event = strings.ToLower(strings.TrimSpace(event))
	var payload map[string]any
	if strings.TrimSpace(data) != "" {
		_ = json.Unmarshal([]byte(data), &payload)
	}
	text := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := payload[k].(string); ok {
				return v
			}
		}
		return ""
	}
	// 实测：text 为全文快照（非增量）→ 替换（[DONE]/done 文本 → 终止）。
	// 两处判定（delta 分支之后、output 数组之后）共用同一本地闭包——刻意
	// 不抽包外 helper：判定顺序依赖仍是本函数隐式约束（函数头注释），
	// 闭包保持该约束就地可见。
	snapshot := func() bool {
		t := text("text", "content", "delta", "message")
		if t == "" {
			return false
		}
		if t == "[DONE]" || t == "[DONE] " || strings.EqualFold(t, "done") {
			*finish = "stop"
			return true
		}
		content.Reset()
		content.WriteString(t)
		return true
	}
	switch event {
	case "", "data", "onanswer", "answer", "delta", "message", "content":
		// OpenAI 标准 SSE 终止标记（纯 data 行）
		if strings.TrimSpace(data) == "[DONE]" {
			if *finish == "" {
				*finish = "stop"
			}
			return
		}
		if code := text("error_code", "code"); code != "" && code != "0" {
			*upErr = &UpstreamError{Code: code, Msg: firstNonEmpty(text("error_msg", "message", "msg"), "request failed")}
			return
		}
		// OpenAI 风格 delta：{choices:[{delta:{content,reasoning_content,tool_calls}}]} → 增量追加
		delta, finishReason := deltaFromChunk(payload)
		if finishReason != "" {
			*finish = finishReason
		}
		if d := delta; d != nil {
			if c, ok := d["content"].(string); ok && c != "" {
				content.WriteString(c)
			}
			if r, ok := d["reasoning_content"].(string); ok && r != "" {
				reason.WriteString(r)
			}
			// 原生工具调用增量：按 index 拼装，流尾单帧发出（§28.4 决策 D）
			mergeToolCallDeltas(stitch, d)
			return
		}
		// 纯 usage 终帧：{choices:[],usage:{...}} → 结束
		if _, hasUsage := payload["usage"].(map[string]any); hasUsage {
			if *finish == "" {
				*finish = "stop"
			}
			return
		}
		if snapshot() {
			return
		}
		// 终态 output 数组：[{type:"output_text",text}]
		if out, ok := payload["output"].([]any); ok && len(out) > 0 {
			var sb strings.Builder
			for _, item := range out {
				if m, ok := item.(map[string]any); ok && m["type"] == "output_text" {
					if s, ok := m["text"].(string); ok {
						sb.WriteString(s)
					}
				}
			}
			if sb.Len() > 0 {
				content.Reset()
				content.WriteString(sb.String())
			}
			return
		}
		if snapshot() {
			return
		}
		// 纯 structured QA 帧（无 text）：仅正文仍空时取 answer，避免整段 JSON 当回复
		tryFillStructuredAnswer(content, payload)
	case "reasoning", "thinking", "onreasoning", "onthinking":
		if t := text("text", "reasoning", "content", "thinking"); t != "" {
			reason.WriteString(t)
		}
	case "done", "end", "finish":
		if code := text("error_code", "code"); code != "" && code != "0" {
			*upErr = &UpstreamError{Code: code, Msg: firstNonEmpty(text("error_msg", "message", "msg"), "request failed")}
			return
		}
		if f := text("finish_reason", "finish", "reason"); f != "" {
			*finish = f
		} else {
			*finish = "stop"
		}
	}
}

// tryFillStructuredAnswer 尝试填充 structured QA（{question,options,answer} 对象）。
// 仅 content 为空时生效（避免把整段 JSON 当回复）；答案优先，直接取 answer。
func tryFillStructuredAnswer(content *strings.Builder, obj map[string]any) {
	if content.Len() > 0 || !isValidStructuredQA(obj) {
		return
	}
	if a, ok := obj["answer"].(string); ok {
		content.WriteString(a)
	}
}

// unwrapQAContent 移除可能的 ```markdown 或 ```json 等包装，
// 若内容本身是结构化问答 JSON，则直接提取 answer。
func unwrapQAContent(s string) string {
	trimmed := strings.TrimSpace(s)
	// 裸 JSON 字符串：模型把整段 QA JSON 写进 text
	if obj, ok := parseQAJSON(trimmed); ok {
		if a, ok := obj["answer"].(string); ok {
			return a
		}
		return s
	}
	if strings.HasPrefix(s, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 3 {
			first := strings.ToLower(strings.TrimSpace(lines[0]))
			last := strings.ToLower(strings.TrimSpace(lines[len(lines)-1]))
			if (strings.HasPrefix(first, "```markdown") || strings.HasPrefix(first, "```json") || strings.HasPrefix(first, "```")) && last == "```" {
				content := strings.Join(lines[1:len(lines)-1], "\n")
				if obj, ok := parseQAJSON(content); ok {
					if ans, ok := obj["answer"].(string); ok {
						return ans
					}
				}
				return content
			}
		}
		return s
	}
	return s
}

// parseQAJSON 尝试把字符串解析为结构化问答 JSON 对象。
func parseQAJSON(s string) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, false
	}
	if !isValidStructuredQA(obj) {
		return nil, false
	}
	return obj, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// AggregateRaw 读取完整 SSE 并聚合。
func AggregateRaw(r io.Reader) (*RawCompletion, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		content strings.Builder
		reason  strings.Builder
		finish  = "stop"
		upErr   error
	)
	var pendingEvent string
	stitch := map[int]*ToolCall{}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if ev, data, ok := scanLine(strings.TrimRight(line, "\r\n"), &pendingEvent); ok {
			applyEvent(&content, &reason, &finish, &upErr, stitch, ev, data)
		}
		if err == io.EOF {
			break
		}
	}
	if upErr != nil {
		return nil, upErr
	}
	return &RawCompletion{
		Content:   unwrapQAContent(content.String()),
		Reasoning: reason.String(),
		Finish:    finish,
		ToolCalls: stitchedCalls(stitch),
	}, nil
}

// RateClassifier 判定错误文本是否属限流（避免 upstream 依赖具体上游策略）。
type RateClassifier func(msg string) bool

// SSEChunkWriter 向客户端回写 OpenAI 兼容 SSE chunk（流式转换共用）。
type SSEChunkWriter struct {
	w           http.ResponseWriter
	fl          http.Flusher
	id          string
	model       string
	done        bool
	isRateLimit RateClassifier
}

// NewSSEChunkWriter 设置 SSE 响应头并初始化 writer。
func NewSSEChunkWriter(w http.ResponseWriter, model string, isRateLimit ...RateClassifier) *SSEChunkWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	sw := &SSEChunkWriter{w: w, fl: fl, id: fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), model: model}
	if len(isRateLimit) > 0 {
		sw.isRateLimit = isRateLimit[0]
	}
	return sw
}

// Delta 写一个增量 chunk。
func (s *SSEChunkWriter) Delta(delta map[string]any) error {
	return s.write(delta, "")
}

// Finish 写终止 chunk（finish_reason）。
func (s *SSEChunkWriter) Finish(fin string) error {
	if s.done {
		return nil
	}
	if err := s.write(map[string]any{}, fin); err != nil {
		return err
	}
	return s.Done()
}

// Done 写 [DONE] 终止标记。
func (s *SSEChunkWriter) Done() error {
	if s.done {
		return nil
	}
	s.done = true
	if _, err := io.WriteString(s.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

// Error 写上游错误事件并终止。限流类错误（由注入的 RateClassifier 判定）
// 附 retryable 提示，客户端可据此退避重试而不是直接判死。
func (s *SSEChunkWriter) Error(msg string) error {
	payload := map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": "CODEARTS_STREAM_ERROR"}}
	if s.isRateLimit != nil && s.isRateLimit(msg) {
		payload["error"].(map[string]any)["type"] = "rate_limit_error"
		payload["error"].(map[string]any)["retryable"] = true
	}
	raw, _ := json.Marshal(payload)
	if _, err := io.WriteString(s.w, "event: error\n"+"data: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return s.Done()
}

func (s *SSEChunkWriter) write(delta map[string]any, fin string) error {
	choice := map[string]any{"index": 0, "delta": delta}
	if fin != "" {
		choice["finish_reason"] = fin
	}
	chunk := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   s.model,
		"choices": []any{choice},
	}
	raw, _ := json.Marshal(chunk)
	if _, err := io.WriteString(s.w, "data: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

// StreamDeltas 解析上游 SSE 并增量回调 (content, reason) 增量。
// finish 非空表示本轮终止帧（携带最终 finish_reason）；upErr 非空表示上游错误帧。
// 返回聚合的原始补全（含最终 finish_reason）与读流/回调错误。
func StreamDeltas(r io.Reader, onDelta func(content, reason, finish string, upErr error) error) (*RawCompletion, error) {
	return StreamDeltasWithTools(r, onDelta, nil)
}

// StreamDeltasWithTools 同 StreamDeltas；另在终止帧/流尾以单帧完整参数回调
// 拼装好的原生工具调用（§28.4 决策 D）。onTool 为 nil 时行为与 StreamDeltas
// 完全一致（文本围栏路径回调走既有 onDelta）。回调顺序：全部 tool_calls →
// 终止帧。
func StreamDeltasWithTools(r io.Reader, onDelta func(content, reason, finish string, upErr error) error, onTool func(t ToolCall) error) (*RawCompletion, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		content   strings.Builder
		reason    strings.Builder
		finish    = "stop"
		streamErr error
	)
	var pendingEvent string
	finishSent := false
	stitch := map[int]*ToolCall{}
	flushed := false
	// deferred 工具调用出现之后才到达的正文：原生 tool_calls 是在 flushTools()（终止
	// 处）一次性发出的，若后面的正文即时透传就会跑在工具调用前面 —— 与上游帧序相反。
	// 故先攒着，等工具调用发完再连同终止帧一起发出，保证「正文在调用前 / 正文在调用后」
	// 两种相对次序都不被打乱。
	var deferred strings.Builder
	flushTools := func() {
		if flushed || onTool == nil {
			flushed = true
			return
		}
		flushed = true
		for _, tc := range stitchedCalls(stitch) {
			if streamErr == nil {
				streamErr = onTool(tc)
			}
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			streamErr = err
			break
		}
		if ev, data, ok := scanLine(strings.TrimRight(line, "\r\n"), &pendingEvent); ok {
			var upErr error
			beforeContent, beforeReason := content.Len(), reason.Len()
			applyEvent(&content, &reason, &finish, &upErr, stitch, ev, data)
			if upErr != nil {
				if e := onDelta("", "", "", upErr); e != nil {
					streamErr = e
					break
				}
				continue
			}
			var cd, rd string
			// 快照类事件会 Reset 后整体替换：新内容比旧内容短时长度差为负，
			// 下标切片会越界 panic——按替换语义整体输出（客户端多显示优于崩溃）。
			switch l := content.Len(); {
			case l > beforeContent:
				cd = content.String()[beforeContent:]
			case l < beforeContent:
				cd = content.String()
			}
			switch l := reason.Len(); {
			case l > beforeReason:
				rd = reason.String()[beforeReason:]
			case l < beforeReason:
				rd = reason.String()
			}
			if len(stitch) > 0 && cd != "" {
				deferred.WriteString(cd) // 已出现工具调用增量：正文延后到调用之后
				cd = ""
			}
			isFinish := strings.EqualFold(ev, "done") || strings.EqualFold(ev, "end") || strings.EqualFold(ev, "finish")
			if isFinish {
				finishSent = true
				flushTools() // 工具调用须先于终止帧（帧序契约，§28.4 决策 D）
				if deferred.Len() > 0 {
					cd = deferred.String() + cd // 延后正文排在工具调用之后
					deferred.Reset()
				}
			}
			if cd != "" || rd != "" || isFinish {
				finArg := ""
				if isFinish {
					finArg = finish
				}
				if streamErr == nil {
					if e := onDelta(cd, rd, finArg, nil); e != nil {
						streamErr = e
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	// 上游不发送显式终止事件直接 EOF 时（data:[DONE] 行只置 finish 不触发
	// isFinish），在此补发工具调用与终止回调——否则客户端收不到 finish chunk。
	if !finishSent && streamErr == nil {
		flushTools()
		if finish != "" || deferred.Len() > 0 {
			if e := onDelta(deferred.String(), "", finish, nil); e != nil {
				streamErr = e
			}
			deferred.Reset()
		}
	}
	return &RawCompletion{Content: content.String(), Reasoning: reason.String(),
		Finish: finish, ToolCalls: stitchedCalls(stitch)}, streamErr
}
