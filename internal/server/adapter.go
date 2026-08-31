// 入站协议归一化（SPEC §13）：anthropic /v1/messages 与 responses /v1/responses
// 解析为统一内部模型 openAIMessage 数组，与 chat 共用指纹/折叠/工具模拟管线。
// 全部为无状态纯函数；非文本块折叠为占位文本（§13.3），未知块/思考链丢弃
// （客户端不回发 → 参与指纹会破坏前缀可复算性，见 SPEC §13.2 注）。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"omnigate2api/internal/adapt"
)

// defaultMediaPlaceholder 非文本块缺省占位模板（Profile.folding.media_placeholder 可覆盖）。
func defaultMediaPlaceholder(typ string) string {
	return "[用户发送了一个附件：" + typ + "]"
}

// mediaPlaceholder Profile 模板 → 占位函数（§13.3：{type} 占位符替换）。
// 返回 nil 表示使用内置默认（无需替换）。
func (h *Handler) mediaPlaceholder(p *adapt.UpstreamProfile) func(string) string {
	if p == nil || p.Message.Folding == nil || p.Message.Folding.MediaPlaceholder == "" {
		return nil
	}
	tmpl := p.Message.Folding.MediaPlaceholder
	return func(typ string) string {
		if !strings.Contains(tmpl, "{type}") {
			return tmpl
		}
		return strings.ReplaceAll(tmpl, "{type}", typ)
	}
}

// reapplyMediaPlaceholder 把 parse 期内置默认占位文本替换为 Profile 模板
// （parse 不依赖 Profile；模板应用统一在拿到 profile 后，§13.3）。
func reapplyMediaPlaceholder(msgs []openAIMessage, ph func(string) string) []openAIMessage {
	const prefix = "[用户发送了一个附件："
	for i := range msgs {
		s := msgs[i].Text
		for {
			start := strings.Index(s, prefix)
			if start < 0 {
				break
			}
			rest := s[start+len(prefix):]
			end := strings.Index(rest, "]")
			if end < 0 {
				break
			}
			typ := rest[:end]
			s = s[:start] + ph(typ) + rest[end+1:]
		}
		msgs[i].Text = s
	}
	return msgs
}

// ---------------------------------------------------------------------------
// anthropic（/v1/messages）
// ---------------------------------------------------------------------------

type anthropicBlock struct {
	Type      string          `json:"type"` // text|tool_use|tool_result|image|thinking|refusal
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result: string | blocks
	Source    json.RawMessage `json:"source"`
}

// parseAnthropicRequest 解析 /v1/messages → 统一请求模型（SPEC §13.2 矩阵）。
func parseAnthropicRequest(body []byte, mediaPlaceholder func(string) string) (*chatRequest, error) {
	if mediaPlaceholder == nil {
		mediaPlaceholder = defaultMediaPlaceholder
	}
	var raw struct {
		Model      string            `json:"model"`
		System     json.RawMessage   `json:"system"` // string | []{type,text}
		Messages   []json.RawMessage `json:"messages"`
		MaxTokens  int               `json:"max_tokens"`
		Stream     bool              `json:"stream"`
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	if len(raw.Messages) == 0 {
		return nil, errors.New("messages is empty")
	}
	if raw.MaxTokens <= 0 {
		return nil, errors.New("max_tokens is required")
	}
	req := &chatRequest{
		Model:      raw.Model,
		Stream:     raw.Stream,
		ToolChoice: parseAnthropicToolChoice(raw.ToolChoice),
	}
	if raw.Tools != nil {
		req.Tools = anthropicToolsToOpenAI(raw.Tools)
	}
	// system（字符串或 text 块数组）→ 前导 system 消息
	if len(raw.System) > 0 && string(raw.System) != "null" {
		req.Messages = append(req.Messages, openAIMessage{Role: "system", Text: anthropicSystemText(raw.System)})
	}
	for i, rw := range raw.Messages {
		msgs, err := parseAnthropicMessage(rw, mediaPlaceholder)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		req.Messages = append(req.Messages, msgs...)
	}
	if !hasSendable(req.Messages) {
		return nil, errors.New("no user or tool message found")
	}
	return req, nil
}

func hasSendable(msgs []openAIMessage) bool {
	for _, m := range msgs {
		if m.Role == "user" || m.Role == "tool" {
			return true
		}
	}
	return false
}

// anthropicSystemText system 字段（字符串或 text 块数组）→ 纯文本。
func anthropicSystemText(rw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(rw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rw, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// parseAnthropicMessage 单条 anthropic 消息 → 可能多条 openAIMessage
// （tool_result 拆为独立 role=tool，其余 text 合并为前置 user）。
func parseAnthropicMessage(rw json.RawMessage, mediaPlaceholder func(string) string) ([]openAIMessage, error) {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(rw, &msg); err != nil {
		return nil, err
	}
	role := strings.ToLower(strings.TrimSpace(msg.Role))
	if role == "" {
		return nil, errors.New("missing role")
	}

	// 字符串 content
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		switch role {
		case "user", "assistant":
			return []openAIMessage{{Role: role, Text: s}}, nil
		default:
			return nil, fmt.Errorf("unsupported role %q", role)
		}
	}

	// content blocks
	var blocks []anthropicBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, fmt.Errorf("parse content: %w", err)
	}
	var out []openAIMessage
	var user strings.Builder
	flushUser := func() {
		if user.Len() > 0 {
			out = append(out, openAIMessage{Role: "user", Text: user.String()})
			user.Reset()
		}
	}
	// assistant 的 text 与 tool_use 块合并为单条消息（客户端回发同构，§13.2）
	var asText strings.Builder
	var asCalls []openAIToolCall
	flushAssistant := func() {
		if asText.Len() > 0 || len(asCalls) > 0 {
			out = append(out, openAIMessage{Role: "assistant", Text: asText.String(), ToolCalls: asCalls})
			asText.Reset()
			asCalls = nil
		}
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if role == "user" {
				user.WriteString(b.Text)
			} else {
				asText.WriteString(b.Text)
			}
		case "tool_use":
			if role != "assistant" {
				continue
			}
			asCalls = append(asCalls, openAIToolCall{
				ID: b.ID, Name: b.Name, Arguments: rawJSONString(b.Input),
			})
		case "tool_result":
			if role != "user" {
				continue
			}
			flushUser()
			// content 可为 string 或 blocks；提取文本部分
			text := anthropicToolResultText(b.Content)
			out = append(out, openAIMessage{Role: "tool", ToolCallID: b.ToolUseID, Text: text})
		case "image":
			if role == "user" {
				user.WriteString(mediaPlaceholder("image"))
			}
		case "thinking":
			// 客户端不回发思考块：参与指纹会破坏可复算性，丢弃（SPEC §13.2 注）
		default:
			// refusal 等未知块：忽略
		}
	}
	if role == "user" {
		flushUser()
	} else {
		flushAssistant()
	}
	return out, nil
}

// anthropicToolResultText tool_result.content（string 或 blocks）→ 文本。
func anthropicToolResultText(rw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(rw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(rw, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// anthropicToolsToOpenAI {name,description,input_schema} → OpenAI function 格式。
func anthropicToolsToOpenAI(raw []json.RawMessage) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, rw := range raw {
		var t struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if json.Unmarshal(rw, &t) != nil || t.Name == "" {
			continue
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.InputSchema) > 0 {
			fn["parameters"] = json.RawMessage(t.InputSchema)
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// parseAnthropicToolChoice auto|any|none 字符串或 {type,name} 对象 → 统一模型。
func parseAnthropicToolChoice(raw json.RawMessage) toolChoiceOpenAI {
	if len(raw) == 0 || string(raw) == "null" {
		return toolChoiceOpenAI{Mode: "auto"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(s) {
		case "none":
			return toolChoiceOpenAI{Mode: "none"}
		case "any":
			return toolChoiceOpenAI{Mode: "required"}
		default:
			return toolChoiceOpenAI{Mode: "auto"}
		}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		switch strings.ToLower(obj.Type) {
		case "none":
			return toolChoiceOpenAI{Mode: "none"}
		case "any":
			return toolChoiceOpenAI{Mode: "required"}
		case "tool":
			return toolChoiceOpenAI{Mode: "function", Function: obj.Name}
		}
	}
	return toolChoiceOpenAI{Mode: "auto"}
}

// ---------------------------------------------------------------------------
// responses（/v1/responses，Codex 语义）
// ---------------------------------------------------------------------------

type responsesItem struct {
	Type      string          `json:"type"` // message|function_call|function_call_output|reasoning|computer_call
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"` // string | [{type:input_text|output_text,text}]
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    string          `json:"output"`
}

// parseResponsesRequest 解析 /v1/responses → 统一请求模型（SPEC §13.2 矩阵）。
func parseResponsesRequest(body []byte, mediaPlaceholder func(string) string) (*chatRequest, error) {
	if mediaPlaceholder == nil {
		mediaPlaceholder = defaultMediaPlaceholder
	}
	var raw struct {
		Model        string            `json:"model"`
		Instructions string            `json:"instructions"`
		Input        json.RawMessage   `json:"input"` // string | []item
		Stream       bool              `json:"stream"`
		Tools        []json.RawMessage `json:"tools"`
		ToolChoice   json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	req := &chatRequest{
		Model:      raw.Model,
		Stream:     raw.Stream,
		ToolChoice: parseResponsesToolChoice(raw.ToolChoice),
	}
	if raw.Tools != nil {
		req.Tools = responsesToolsToOpenAI(raw.Tools)
	}
	if strings.TrimSpace(raw.Instructions) != "" {
		req.Messages = append(req.Messages, openAIMessage{Role: "system", Text: raw.Instructions})
	}

	// input 为字符串
	var in string
	if err := json.Unmarshal(raw.Input, &in); err == nil {
		if strings.TrimSpace(in) != "" {
			req.Messages = append(req.Messages, openAIMessage{Role: "user", Text: in})
		}
	} else {
		var items []responsesItem
		if err := json.Unmarshal(raw.Input, &items); err != nil {
			return nil, fmt.Errorf("parse input: %w", err)
		}
		for i, it := range items {
			msgs, err := parseResponsesItem(it, mediaPlaceholder)
			if err != nil {
				return nil, fmt.Errorf("input[%d]: %w", i, err)
			}
			for _, m := range msgs {
				// function_call 并入前一条 assistant（SPEC §13.2：相邻合并）
				if len(m.ToolCalls) > 0 && len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "assistant" {
					last := &req.Messages[len(req.Messages)-1]
					last.ToolCalls = append(last.ToolCalls, m.ToolCalls...)
				} else {
					req.Messages = append(req.Messages, m)
				}
			}
		}
	}
	if !hasSendable(req.Messages) {
		return nil, errors.New("no user or tool message found")
	}
	return req, nil
}

// parseResponsesItem 单条 input item → 消息（相邻 function_call 与前一 assistant 合并）。
func parseResponsesItem(it responsesItem, mediaPlaceholder func(string) string) ([]openAIMessage, error) {
	switch it.Type {
	case "message":
		role := strings.ToLower(strings.TrimSpace(it.Role))
		if role != "user" && role != "assistant" && role != "developer" {
			return nil, fmt.Errorf("unsupported message role %q", it.Role)
		}
		if role == "developer" {
			role = "system" // §13.2：developer → system
		}
		return []openAIMessage{{Role: role, Text: responsesContentText(it.Content, mediaPlaceholder)}}, nil
	case "function_call":
		return []openAIMessage{{Role: "assistant", ToolCalls: []openAIToolCall{{
			ID: it.CallID, Name: it.Name, Arguments: it.Arguments,
		}}}}, nil
	case "function_call_output":
		if it.CallID == "" {
			return nil, errors.New("function_call_output missing call_id")
		}
		return []openAIMessage{{Role: "tool", ToolCallID: it.CallID, Text: it.Output}}, nil
	case "reasoning", "computer_call", "computer_call_output", "local_shell_call":
		// 客户端不回发 → 丢弃（与 thinking 同理，SPEC §13.2 注）
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported input item type %q", it.Type)
	}
}

// responsesContentText content（字符串或 [{type,text}]）→ 文本；非文本块占位。
func responsesContentText(rw json.RawMessage, mediaPlaceholder func(string) string) string {
	var s string
	if err := json.Unmarshal(rw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			sb.WriteString(p.Text)
		default:
			if p.Type != "" {
				sb.WriteString(mediaPlaceholder(p.Type))
			}
		}
	}
	return sb.String()
}

// responsesToolsToOpenAI {type:function,name,description,parameters,strict} → OpenAI 嵌套。
func responsesToolsToOpenAI(raw []json.RawMessage) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, rw := range raw {
		var t struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      bool            `json:"strict"`
		}
		if json.Unmarshal(rw, &t) != nil || t.Name == "" {
			continue
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.Parameters) > 0 {
			fn["parameters"] = json.RawMessage(t.Parameters)
		}
		fn["strict"] = t.Strict
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// parseResponsesToolChoice auto/none/function 语义 → 统一模型。
func parseResponsesToolChoice(raw json.RawMessage) toolChoiceOpenAI {
	if len(raw) == 0 || string(raw) == "null" {
		return toolChoiceOpenAI{Mode: "auto"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(s) {
		case "none":
			return toolChoiceOpenAI{Mode: "none"}
		default:
			return toolChoiceOpenAI{Mode: "auto"}
		}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		switch strings.ToLower(obj.Type) {
		case "none":
			return toolChoiceOpenAI{Mode: "none"}
		case "function":
			return toolChoiceOpenAI{Mode: "function", Function: obj.Name}
		}
	}
	return toolChoiceOpenAI{Mode: "auto"}
}
