// OpenAI chat.completions 请求解析。
//
// 支持完整 OpenAI 消息模型：system/user/assistant/tool 四种角色、
// string 与分片 content、assistant 的 tool_calls、tool 结果的 tool_call_id。
// 会话指纹（路线 D）见 session.go：本文件只做请求解析与 JSON 归一化。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// openAIToolCall 一次函数调用（assistant 历史或响应里用）。
type openAIToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串（OpenAI 线格式）
}

// openAIMessage 拍平后的一条消息。
type openAIMessage struct {
	Role       string
	Text       string
	ToolCalls  []openAIToolCall
	ToolCallID string // role=tool 时对应的 call id
	Name       string // role=tool 时的函数名（部分客户端会带）
	// 多模态媒体（SPEC §30.3）：parse 期结构化，渲染期按 Profile.media 分叉——
	// placeholder 折叠为占位文本 / passthrough 转换后分片透传。
	Images   []imagePart     // 图片块（可透传）
	Deferred []deferredMedia // 不可透传的非文本块 / 超限图 → 渲染期占位
}

// imagePart 归一化图片：URL 为 canonical 形态（data URI 或 http(s) URL）。
type imagePart struct {
	URL    string
	Detail string // OpenAI detail 提示（auto/low/high）；空 = 缺省
}

// deferredMedia 不可透传的非文本块：Fixed 非空 = 固定占位文本（如超限图），
// 否则按块类型走 media_placeholder 模板。
type deferredMedia struct {
	Type  string
	Fixed string
}

// deferredOversize 固定占位文本（SPEC §30.7 超限降级）。
const deferredOversize = "[图片超限被省略]"

// toolChoiceOpenAI 归一化后的 tool_choice。
type toolChoiceOpenAI struct {
	Mode     string // "" / "none" / "auto" / "required" / "function"
	Function string // Mode=="function" 时指定的函数名
}

// chatRequest 完整请求体。
type chatRequest struct {
	Model          string
	Stream         bool
	Messages       []openAIMessage
	Tools          []map[string]any
	ToolChoice     toolChoiceOpenAI
	ConversationID string
	Provider       string `json:"provider"` // 多上游路由(SPEC §4.4),缺省 codearts

}

// parseChatRequest 解析并校验请求体。
func parseChatRequest(body []byte) (*chatRequest, error) {
	var raw struct {
		Model          string            `json:"model"`
		Stream         bool              `json:"stream"`
		ConversationID string            `json:"conversation_id"`
		Messages       []json.RawMessage `json:"messages"`
		Tools          []map[string]any  `json:"tools"`
		ToolChoice     json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	if len(raw.Messages) == 0 {
		return nil, errors.New("messages is empty")
	}
	req := &chatRequest{
		Model:          raw.Model,
		Stream:         raw.Stream,
		ConversationID: raw.ConversationID,
		Tools:          raw.Tools,
		ToolChoice:     parseToolChoice(raw.ToolChoice),
	}
	for i, rm := range raw.Messages {
		m, err := parseMessage(rm)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		req.Messages = append(req.Messages, m)
	}
	// 至少要有一条 user 或 tool 消息（否则没有可发送的内容）。
	hasSendable := false
	for _, m := range req.Messages {
		if m.Role == "user" || m.Role == "tool" {
			hasSendable = true
			break
		}
	}
	if !hasSendable {
		return nil, errors.New("no user or tool message found")
	}
	return req, nil
}

// parseMessage 解析单条消息：content 支持 string 与分片数组（SPEC §30.3——
// 文本入 Text、图片结构化入 Images、其余非文本块入 Deferred 渲染期占位）。
func parseMessage(rm json.RawMessage) (openAIMessage, error) {
	var probe struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  json.RawMessage `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
	}
	if err := json.Unmarshal(rm, &probe); err != nil {
		return openAIMessage{}, err
	}
	m := openAIMessage{
		Role:       strings.ToLower(strings.TrimSpace(probe.Role)),
		ToolCallID: probe.ToolCallID,
		Name:       probe.Name,
	}
	if len(probe.Content) > 0 && string(probe.Content) != "null" {
		var s string
		if err := json.Unmarshal(probe.Content, &s); err == nil {
			m.Text = s
		} else {
			m.parseChatContentParts(probe.Content)
		}
	}
	if len(probe.ToolCalls) > 0 && string(probe.ToolCalls) != "null" {
		var calls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(probe.ToolCalls, &calls); err == nil {
			for _, c := range calls {
				m.ToolCalls = append(m.ToolCalls, openAIToolCall{
					ID:        c.ID,
					Name:      c.Function.Name,
					Arguments: rawJSONString(c.Function.Arguments),
				})
			}
		}
	}
	return m, nil
}

// parseChatContentParts OpenAI chat 分片 content：text → Text、image_url →
// Images（超限/畸形降级 Deferred，SPEC §30.7）、其余类型块 → Deferred。
func (m *openAIMessage) parseChatContentParts(raw json.RawMessage) {
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL    string `json:"url"`
			Detail string `json:"detail"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return // 非 string 非数组的畸形 content：维持既有语义（空文本）
	}
	var sb strings.Builder
	for _, p := range parts {
		switch {
		case p.Type == "text" && p.Text != "":
			sb.WriteString(p.Text)
		case p.Type == "image_url" && p.ImageURL != nil:
			m.appendImage(p.ImageURL.URL, p.ImageURL.Detail)
		case p.Type != "":
			m.Deferred = append(m.Deferred, deferredMedia{Type: p.Type})
		}
	}
	m.Text = sb.String()
}

// appendImage 归一化入队一张图片：canonical URL 校验 + 单消息数量上限 +
// base64 体积上限（SPEC §30.7）。
func (m *openAIMessage) appendImage(rawURL, detail string) {
	if len(m.Images) >= maxImagesPerMsg {
		m.Deferred = append(m.Deferred, deferredMedia{Type: "image"})
		return
	}
	if ip, dfr := newImagePart(rawURL, detail); dfr == nil {
		m.Images = append(m.Images, ip)
	} else {
		m.Deferred = append(m.Deferred, *dfr)
	}
}

// newImagePart canonical 图片构造：仅接受 data URI 与 http(s) URL；
// data URI 超 maxImageB64 → 超限降级。detail 白名单外丢弃（§30.5）。
func newImagePart(rawURL, detail string) (imagePart, *deferredMedia) {
	switch {
	case rawURL == "":
		return imagePart{}, &deferredMedia{Type: "image"}
	case strings.HasPrefix(rawURL, "data:"):
		if len(rawURL) > maxImageB64 {
			return imagePart{}, &deferredMedia{Fixed: deferredOversize}
		}
	case !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://"):
		return imagePart{}, &deferredMedia{Type: "image"}
	}
	return imagePart{URL: rawURL, Detail: normalizeImageDetail(detail)}, nil
}

// normalizeImageDetail detail 白名单（§30.2）：auto/low/high 之外丢弃。
func normalizeImageDetail(d string) string {
	switch d {
	case "auto", "low", "high":
		return d
	}
	return ""
}

// countImages 统计消息数组中的图片总数（观测/测试用）。
func countImages(msgs []openAIMessage) int {
	n := 0
	for i := range msgs {
		n += len(msgs[i].Images)
	}
	return n
}

// rawJSONString 把 arguments 统一成 JSON 字符串（兼容字符串与对象两种线格式）。
func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "{}"
	}
	out, err := json.Marshal(v) // Go map marshal 按 key 排序，天然归一化
	if err != nil {
		return "{}"
	}
	return string(out)
}

// parseToolChoice 归一化 tool_choice：字符串或 {"type":"function","function":{"name":...}}。
func parseToolChoice(raw json.RawMessage) toolChoiceOpenAI {
	if len(raw) == 0 || string(raw) == "null" {
		return toolChoiceOpenAI{Mode: "auto"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(s) {
		case "none":
			return toolChoiceOpenAI{Mode: "none"}
		case "required":
			return toolChoiceOpenAI{Mode: "required"}
		default:
			return toolChoiceOpenAI{Mode: "auto"}
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Function.Name != "" {
		return toolChoiceOpenAI{Mode: "function", Function: obj.Function.Name}
	}
	return toolChoiceOpenAI{Mode: "auto"}
}

// normalizeArgs 归一化 arguments（key 排序、去空白）。
func normalizeArgs(args string) string {
	if args == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args
	}
	out, err := json.Marshal(v)
	if err != nil {
		return args
	}
	return string(out)
}
