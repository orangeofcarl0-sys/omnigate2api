// 工具层：project 有损投影（SPEC §14.2）——agentic 检测后 conservative
// 截断或 aggressive（历史摘要/裁尾/anchor user/工具依赖向前扩展/工具
// schema 白名单），空结果回退保守截断。纯函数；有损 ⇒ 强制 native。
package server

import (
	"strconv"
	"strings"

	"omnigate2api/internal/adapt"
)

// agenticToolNames project 的 agentic CLI 判定工具名（§14.2）。
var agenticToolNames = map[string]bool{
	"exec_command": true, "write_stdin": true, "update_plan": true, "apply_patch": true,
	"bash": true, "shell": true, "run_command": true, "read_file": true, "write_file": true,
}

// projectStats 变换统计（可观测性，§14.1）。
type projectStats struct {
	Mode            string `json:"mode"` // conservative | aggressive
	OriginalChars   int    `json:"original_chars"`
	ProjectedChars  int    `json:"projected_chars"`
	DroppedHarness  int    `json:"dropped_harness"`
	SummarizedCount int    `json:"summarized_count"`
	AnchorPreserved bool   `json:"anchor_preserved"`
}

// looksAgentic 判定 agentic CLI 请求（工具名或 harness 标记命中，§14.2）。
func looksAgentic(msgs []openAIMessage, tools []map[string]any) bool {
	for _, t := range tools {
		if fn, ok := t["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); agenticToolNames[name] {
				return true
			}
		}
	}
	for _, m := range msgs {
		if isHarnessText(m.Text) {
			return true
		}
	}
	return false
}

// projectDefaults 缺省参数（SPEC §14.2）。
func projectDefaults(c *adapt.ProjectConfig) {
	if c.HistoryItems <= 0 {
		c.HistoryItems = 10
	}
	if c.HistoryChars <= 0 {
		c.HistoryChars = 2200
	}
	if c.TailMessages <= 0 {
		c.TailMessages = 8
	}
	if c.TailChars <= 0 {
		c.TailChars = 7000
	}
	if c.ToolMaxChars <= 0 {
		c.ToolMaxChars = 1600
	}
}

const (
	maxSystemChars    = 1200
	maxUserChars      = 3200
	maxAssistantChars = 1800
)

func truncateText(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…[已截断]"
}

// projectRequest 有损投影（纯函数）：返回压缩后的消息数组与统计。
func projectRequest(msgs []openAIMessage, tools []map[string]any, cfg *adapt.ProjectConfig) ([]openAIMessage, projectStats) {
	projectDefaults(cfg)
	stats := projectStats{}
	origChars := 0
	for _, m := range msgs {
		origChars += len([]rune(m.Text))
	}
	stats.OriginalChars = origChars

	// tools 投影：description 截断、schema 深度限制
	projectTools(tools, cfg.ToolMaxChars)

	if !looksAgentic(msgs, tools) {
		return projectConservative(msgs, stats)
	}

	// aggressive：丢弃 harness system/user；保留 guidance（≤2 条，各 ≤maxSystemChars）
	var guidance []string
	var conversation []openAIMessage
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			if isHarnessText(m.Text) {
				stats.DroppedHarness++
				continue
			}
			g := truncateText(m.Text, maxSystemChars)
			if g != "" {
				guidance = append(guidance, g)
			}
			continue
		}
		if m.Role == "user" && isHarnessText(m.Text) {
			stats.DroppedHarness++
			continue
		}
		conversation = append(conversation, m)
	}
	if len(guidance) > 2 {
		guidance = guidance[len(guidance)-2:]
	}

	// tail 保留（最后 ≤tail_messages 条且总字符 ≤tail_chars，从尾部向前回溯）
	tailStart := len(conversation)
	chars := 0
	count := 0
	for i := len(conversation) - 1; i >= 0; i-- {
		chars += len([]rune(conversation[i].Text))
		count++
		if count > cfg.TailMessages || chars > cfg.TailChars {
			break
		}
		tailStart = i
	}
	// 工具依赖：tail 中 tool 消息对应的 assistant 调用未在尾内则向前扩
	extended := true
	for extended {
		extended = false
		for _, m := range conversation[tailStart:] {
			if m.Role != "tool" || m.ToolCallID == "" {
				continue
			}
			for i := tailStart - 1; i >= 0; i-- {
				a := conversation[i]
				if a.Role != "assistant" {
					continue
				}
				for _, c := range a.ToolCalls {
					if c.ID == m.ToolCallID && i < tailStart {
						tailStart = i
						extended = true
						break
					}
				}
				if extended {
					break
				}
			}
			if extended {
				break
			}
		}
	}

	// anchor user：最早被投影掉的 user 原样保留
	anchorIdx := -1
	for i, m := range conversation {
		if m.Role == "user" {
			if i < tailStart {
				anchorIdx = i
				stats.AnchorPreserved = true
			}
			break
		}
	}

	// 历史摘要（tail 之前除 anchor 外的消息）
	var summary []string
	summaryChars := 0
	omitted := 0
	for i := 0; i < tailStart; i++ {
		if i == anchorIdx {
			continue
		}
		omitted++
		if len(summary) >= cfg.HistoryItems {
			continue
		}
		line := summarizeLine(conversation[i])
		if summaryChars+len([]rune(line)) > cfg.HistoryChars {
			continue
		}
		summary = append(summary, line)
		summaryChars += len([]rune(line))
	}
	if omitted > len(summary) {
		summary = append(summary, "further condensed: "+strconv.Itoa(omitted-len(summary))+" messages")
	}
	stats.SummarizedCount = omitted

	out := []openAIMessage{}
	for _, g := range guidance {
		out = append(out, openAIMessage{Role: "system", Text: g})
	}
	if len(summary) > 0 {
		out = append(out, openAIMessage{Role: "system", Text: "Earlier conversation summary (condensed):\n" + strings.Join(summary, "\n")})
	}
	if anchorIdx >= 0 {
		out = append(out, conversation[anchorIdx])
	}
	out = append(out, conversation[tailStart:]...)
	if len(out) == 0 {
		// 投影后为空（全部为 harness 噪音）→ 回退保守截断（§14.2 回退规则）
		return projectConservative(msgs, stats)
	}
	stats.Mode = "aggressive"
	stats.ProjectedChars = charsOf(out)
	return out, stats
}

// projectConservative 非 agentic（或 aggressive 回退）：单条消息截断。
func projectConservative(msgs []openAIMessage, stats projectStats) ([]openAIMessage, projectStats) {
	out := make([]openAIMessage, len(msgs))
	for i, m := range msgs {
		out[i] = m
		limit := maxAssistantChars
		switch m.Role {
		case "system", "developer":
			limit = maxSystemChars
		case "user":
			limit = maxUserChars
		}
		out[i].Text = truncateText(m.Text, limit)
	}
	stats.Mode = "conservative"
	stats.ProjectedChars = charsOf(out)
	return out, stats
}

// summarizeLine 单条消息 → 摘要行（SPEC §14.2 规则）。
func summarizeLine(m openAIMessage) string {
	text := truncateText(m.Text, 120)
	switch m.Role {
	case "user":
		if text != "" {
			return "User asked: " + text
		}
		return "User asked a question"
	case "assistant":
		if text != "" {
			return "Assistant replied: " + text
		}
		if len(m.ToolCalls) > 0 {
			return "Assistant called tool " + m.ToolCalls[0].Name
		}
		return "Assistant replied"
	case "tool":
		name := m.Name
		if name == "" {
			name = m.ToolCallID
		}
		return "Tool " + name + " returned: " + text
	default:
		return "[" + m.Role + "]: " + text
	}
}

// projectTools 工具 schema 投影：description 截断、递归深度限制（§14.2）。
func projectTools(tools []map[string]any, maxChars int) {
	for _, t := range tools {
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if d, _ := fn["description"].(string); len([]rune(d)) > maxChars {
			fn["description"] = truncateText(d, maxChars)
		}
		if p, ok := fn["parameters"]; ok {
			fn["parameters"] = shrinkSchema(p, 0)
		}
	}
}

func shrinkSchema(v any, depth int) any {
	if depth > 6 {
		return map[string]any{"type": "object"}
	}
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			// schema 白名单（SPEC §14.2：16 键）
			switch k {
			case "type", "properties", "required", "items", "enum", "oneOf", "anyOf",
				"allOf", "additionalProperties", "format", "min", "max",
				"minItems", "maxItems", "description":
				out[k] = shrinkSchema(val, depth+1)
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, item := range x {
			out = append(out, shrinkSchema(item, depth+1))
		}
		return out
	default:
		return v
	}
}

func charsOf(msgs []openAIMessage) int {
	n := 0
	for _, m := range msgs {
		n += len([]rune(m.Text))
	}
	return n
}

// toolchainEnabled 解析工具层开关：OMNIGATE_TOOLCHAIN 覆盖优先级高于 Profile 声明
