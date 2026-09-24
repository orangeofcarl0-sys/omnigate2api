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
	// IncludeUsage 对应 OpenAI 的 stream_options.include_usage：流式回合是否在
	// [DONE] 前追加一个只带 usage 的 chunk（OpenAI 只在显式请求时下发）。
	IncludeUsage bool
	// Gen 客户端的生成参数（白名单透传给上游，见 parseGenParams）。
	// 早前上游 body 只拼 model/stream/messages/tools，客户端设的 temperature/max_tokens
	// 等**全部静默失效**——这里补上。
	Gen map[string]any
}

// generateParamPolicy 发送侧形态纪律（SPEC §33.4）：**我们发什么由官方客户端的形态决定**，
// 而不是由"上游恰好接受"决定——上游多认一个字段，不代表客户端可以发它：请求形态本身就是
// 指纹，官方客户端没有的控件出现在我们的 body 里，就是可被识别的差异。
//
//   - forward：官方客户端自己会发（或其模型配置驱动的）参数 → 透传；
//   - default-only：官方没有该控件，但 OpenAI 客户端常带默认值 → **默认值静默放行、非默认值拒绝**
//     （既不产生形态差异，也不做静默失效）；
//   - 其余键（n>1 / logit_bias / user …）一律拒绝。
type genPolicy int

const (
	genForward     genPolicy = iota // 透传
	genDefaultOnly                  // 仅默认值放行
)

// genParamOrder 固定处理顺序（别名归一取先者，故不能用 map 遍历）。
var genParamOrder = []string{
	"max_tokens", "max_completion_tokens", "max_output_tokens",
	"temperature", "top_p", "reasoning_effort",
	"stop", "stop_sequences", "seed",
	"frequency_penalty", "presence_penalty", "logprobs", "top_logprobs",
	"n", "response_format",
}

// genParamPolicy 各入站键的策略与上游字段名。
var genParamPolicy = map[string]struct {
	upstream string
	policy   genPolicy
}{
	"max_tokens":            {"max_tokens", genForward},
	"max_completion_tokens": {"max_tokens", genForward},
	"max_output_tokens":     {"max_tokens", genForward},
	"temperature":           {"temperature", genForward},
	"top_p":                 {"top_p", genForward},
	"reasoning_effort":      {"reasoning_effort", genForward},

	"stop":              {"stop", genDefaultOnly},
	"stop_sequences":    {"stop", genDefaultOnly},
	"seed":              {"seed", genDefaultOnly},
	"frequency_penalty": {"frequency_penalty", genDefaultOnly},
	"presence_penalty":  {"presence_penalty", genDefaultOnly},
	"logprobs":          {"logprobs", genDefaultOnly},
	"top_logprobs":      {"top_logprobs", genDefaultOnly},
	"n":                 {"n", genDefaultOnly},
	"response_format":   {"response_format", genDefaultOnly},
}

// isDefaultValue 判定"该键取默认值"（默认值不改变输出，故放行且不透传）。
func isDefaultValue(key string, v any) bool {
	switch key {
	case "stop", "stop_sequences":
		if v == nil {
			return true
		}
		if arr, ok := v.([]any); ok {
			return len(arr) == 0
		}
		return false
	case "seed", "frequency_penalty", "presence_penalty", "top_logprobs":
		f, ok := v.(float64)
		return ok && f == 0
	case "logprobs":
		b, ok := v.(bool)
		return ok && !b
	case "n":
		f, ok := v.(float64)
		return ok && f == 1
	case "response_format":
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		t, _ := m["type"].(string)
		return t == "" || t == "text"
	}
	return false
}

// parseGenParams 提取要透传给上游的生成参数；返回 (参数, 违规键)。
// 违规键（非默认值且官方客户端形态里没有）→ 调用方回 400，**绝不静默丢弃**。
func parseGenParams(body []byte) (map[string]any, []string) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return nil, nil
	}
	out := map[string]any{}
	var rejected []string
	for _, in := range genParamOrder {
		rv, ok := raw[in]
		if !ok {
			continue
		}
		var v any
		if json.Unmarshal(rv, &v) != nil || v == nil {
			continue
		}
		if sv, isStr := v.(string); isStr && strings.TrimSpace(sv) == "" {
			continue
		}
		spec := genParamPolicy[in]
		if spec.policy == genDefaultOnly {
			if !isDefaultValue(in, v) {
				rejected = append(rejected, in)
			}
			continue // 默认值：放行但不透传（不给上游添官方没有的字段）
		}
		if _, exists := out[spec.upstream]; exists {
			continue
		}
		out[spec.upstream] = v
	}
	if len(out) == 0 {
		out = nil
	}
	return out, rejected
}

// unsupportedParamsError 违规参数的统一错误文案（列出键名与原因，便于调用方自查）。
func unsupportedParamsError(keys []string) string {
	return "unsupported parameter(s): " + strings.Join(keys, ", ") +
		" — 本网关按官方客户端请求形态转发（发送形态即指纹，SPEC §33.4）；" +
		"请移除这些参数后重试（默认值可保留）"
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
		StreamOptions  *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
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
		IncludeUsage:   raw.StreamOptions != nil && raw.StreamOptions.IncludeUsage,
	}
	gen, rejected := parseGenParams(body)
	if len(rejected) > 0 {
		return nil, errors.New(unsupportedParamsError(rejected))
	}
	req.Gen = gen
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
