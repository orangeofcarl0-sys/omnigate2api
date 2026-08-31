// 可观测性：折叠观测、转录回声检测与样本落盘。
package server

// 输出规范层（outbound/observability）：折叠观测、转录回声检测、样本落盘（SPEC §7）。

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"omnigate2api/internal/upstream"
)

// transcriptEcho 检测模型输出中的转录模仿（复述/编造 [助手]/[工具 …] 标记，
// 或弱模型自创的 [TOOL_RESULT] 假执行现场）。
func transcriptEcho(s string) bool {
	if strings.Contains(s, "[助手调用工具 ") || strings.Contains(s, "[助手]\n") || strings.Contains(s, "[TOOL_RESULT]") {
		return true
	}
	return strings.Contains(s, "[工具 ") && strings.Contains(s, " 返回结果]")
}

// dumpEcho 把回声样本落到 prompt dump 目录（与 OMNIGATE_DEBUG_PROMPTS 同目录）。
func (h *Handler) dumpEcho(model, content string) {
	writeDump(h.cfg.DebugPromptDir,
		fmt.Sprintf("echo-%s-%s-%d.txt", time.Now().Format("150405.000"), model, len([]rune(content))),
		[]byte(content))
}

// writeDump 落盘调试样本（OMNIGATE_DEBUG_PROMPTS 目录；目录为空时关闭）。
func writeDump(dir, name string, body []byte) {
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, name), body, 0o644)
}

// logFold 记录折叠提示词尺寸，可选落盘（OMNIGATE_DEBUG_PROMPTS=1 → data/prompts/）。
func (h *Handler) logFold(model string, reqMsgs []openAIMessage, msgs []upstream.ChatMessage, toolsOn, continueMode bool) {
	var promptChars int
	for _, m := range msgs {
		promptChars += len([]rune(m.Content))
	}
	mode := h.cfg.SessionMode
	if mode == "" {
		mode = "native"
	}
	log.Printf("chat fold model=%s msgs=%d prompt_chars=%d tools=%v continue=%v mode=%s", model, len(reqMsgs), promptChars, toolsOn, continueMode, mode)
	writeDump(h.cfg.DebugPromptDir,
		fmt.Sprintf("%s-%s-%d.txt", time.Now().Format("150405.000"), model, promptChars),
		[]byte(msgs[0].Content))
}

// noContextEcho 模型自述"无上下文/第一条用户消息"类失忆指纹
// （SPEC §5.3 GuardNoInstruction 采样面）。
func noContextEcho(s string) bool {
	samples := []string{
		"没有可供总结的先前上下文", "本轮会话的第一条用户消息", "没有先前上下文",
		"第一条用户消息", "没有可供总结", "no explicit user question",
		"there was no user message", "this is the first message of the conversation",
	}
	for _, k := range samples {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}
