// 提示词组装：把 OpenAI 消息数组折叠为适合上游的单条文本（工具模拟注入用）。
//
// 上游上下文按 chat_id 会话维持；工具模拟需要把工具清单注入提示词，
// 因此提供全量折叠（renderFullPrompt）与尾部折叠（renderTailPrompt）两种形态。
//   - 新会话：system + 完整历史转录折叠进首条消息（renderFullPrompt）
//   - 续接会话：只发送未吸收的增量消息（renderTailPrompt）
package server

// 入站规范层（inbound/folding）：折叠渲染与护栏（见 docs/SPEC-adaptation-layer.md §4 MessageProfile）。

import (
	"strings"
)

// renderFullPrompt 新会话：折叠全部消息（system 提前，其余按序转录）。
func renderFullPrompt(msgs []openAIMessage, toolsBlock string) string {
	var sys, dialogue []string
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			if strings.TrimSpace(m.Text) != "" {
				sys = append(sys, m.Text)
			}
			continue
		}
		if s := formatMessage(m); s != "" {
			dialogue = append(dialogue, s)
		}
	}
	var sb strings.Builder
	if len(sys) > 0 {
		sb.WriteString("[系统指令]\n")
		sb.WriteString(strings.Join(sys, "\n\n"))
		sb.WriteString("\n\n")
	}
	if len(dialogue) > 1 {
		sb.WriteString("[对话历史]\n")
		sb.WriteString(strings.Join(dialogue[:len(dialogue)-1], "\n\n"))
		sb.WriteString("\n\n[当前消息]\n")
		sb.WriteString(dialogue[len(dialogue)-1])
	} else if len(dialogue) == 1 {
		sb.WriteString(dialogue[0])
	}
	sb.WriteString(toolsBlock)
	sb.WriteString(guardBlock)
	return sb.String()
}

// renderTailPrompt 续接会话：只折叠增量消息（通常是 tool 结果 + 最新 user 消息）。
func renderTailPrompt(tail []openAIMessage, toolsBlock string) string {
	var parts []string
	for _, m := range tail {
		if s := formatMessage(m); s != "" {
			parts = append(parts, s)
		}
	}
	// 与 renderFullPrompt 相同的收尾护栏。
	return strings.Join(parts, "\n\n") + toolsBlock + guardBlock
}

// guardBlock 提示词收尾护栏（近因位置）：划定转录边界，禁止模仿转录格式、
// 编造工具结果——尤其针对弱模型「跳过真实工具调用、直接虚构执行现场」的
// 失败模式（如凭空输出 📝 [TOOL_RESULT] 假 ls 结果）。
const guardBlock = "\n\n[回复要求]\n现在轮到你以 [助手] 身份回复本轮：只输出本轮回答正文与（需要时）```tool_call 块，然后停止等待系统回传结果。" +
	"[对话历史]、[助手]、[助手调用工具 …]、[工具 … 返回结果] 等标记是系统注入的转录格式，" +
	"你的回复中绝对禁止出现这些标记，禁止续写或编造任何转录内容。\n" +
	"需要任何真实世界的信息（文件内容、命令输出、目录列表等），必须发起真实的工具调用获取；" +
	"绝对禁止在正文里虚构任何形式的工具执行过程或结果（无论格式，包括 [TOOL_RESULT]、📝 装饰等）。" +
	"没有真实调用，就没有结果——宁可只说明你将调用什么工具，也不要编造它的输出。\n"

// formatMessage 单条消息转录文本。
func formatMessage(m openAIMessage) string {
	switch m.Role {
	case "user":
		return m.Text
	case "system", "developer":
		if strings.TrimSpace(m.Text) == "" {
			return ""
		}
		return "[系统指令]\n" + m.Text
	case "assistant":
		var sb strings.Builder
		sb.WriteString("[助手]")
		if strings.TrimSpace(m.Text) != "" {
			sb.WriteString("\n" + m.Text)
		}
		for _, c := range m.ToolCalls {
			sb.WriteString("\n[助手调用工具 " + c.Name + " 参数 " + normalizeArgs(c.Arguments) + "]")
		}
		if sb.Len() == len("[助手]") {
			return ""
		}
		return sb.String()
	case "tool":
		name := m.Name
		if name == "" {
			name = m.ToolCallID
		}
		if name == "" {
			name = "unknown"
		}
		return "[工具 " + name + " 返回结果]\n" + m.Text
	default:
		return "[" + m.Role + "]\n" + m.Text
	}
}
