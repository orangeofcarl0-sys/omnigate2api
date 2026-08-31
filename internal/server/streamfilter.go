// 入站规范层（inbound/tools）：流式工具调用增量解析与 post 抑制（ToolProfile 驱动面）。
// 工具模拟的整体设计见 tools.go。
package server

import (
	"strings"
	"unicode/utf8"
)

const toolFenceOpen = "```tool_call"

// toolStreamFilter 在上游正文增量流中识别 tool_call 围栏：
//   - 首个调用前的文本即时以 content delta 透传（保留可能构成起始围栏前缀的尾部）；
//   - 围栏内缓冲，闭合后解析并以 tool_calls delta 一次性下发（多次调用按 index 递增）；
//   - 发出任一调用后进入 post 态：按 OpenAI 语义回合应在 tool_calls 处结束，
//     其后正文（实测为模型编造的 [[工具 … 返回结果]]、乱序围栏等幻觉）一律抑制，
//     仅继续扫描后续起始围栏以支持一流多调；
//   - 围栏未闭合（流被截断）时按闭合处理；首个调用解析失败仍按纯文本透传。
//
// 输出目标是协议无关 sink（stream.go）：三协议（chat/anthropic/responses）
// 共用同一解析器，由各自的 writer 决定线格式。
type toolStreamFilter struct {
	sink      streamSink
	pending   string // 围栏外暂存
	inTool    bool
	inPost    bool // 已发出至少一个 tool_call，后续正文抑制
	toolBuf   string
	callIndex int
	found     bool
}

// fenceRetainBytes 标记/围栏前缀的最小保留长度：``` + 杂散零宽字符 +
// "tool_call" 部分前缀的最坏情况。
const fenceRetainBytes = 20

// cutLineHead 行缓冲头部切割，守住两个不变量：
//
//	① 切割点必须落在 UTF-8 rune 边界——多字节字符被切开后，前后半分别经
//	   json.Marshal 会各自变成 U+FFFD（永久乱码）；
//	② 当前行未完成时整行缓冲——括号标记行整体 60+ 字节，按字节保留会把
//	   标记开头冲掉，导致标记行永远不完整而漏识别。
//
// 返回（可发出的头部, 剩余）。无可切头部时头部为空串。
func cutLineHead(s string, keep int) (string, string) {
	if len(s) <= keep {
		return "", s
	}
	n := len(s) - keep
	nl := strings.LastIndex(s, "\n")
	if nl < 0 || nl+1 > n {
		return "", s // 当前行未完成：整行缓冲
	}
	n = nl + 1
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], s[n:]
}

// releaseOversizedLine 不变量③：单行超过 limit 时强制释放（防无界缓冲），
// 头部同样按 rune 边界切割。
func releaseOversizedLine(s string, limit int) (string, string) {
	if len(s) <= limit {
		return "", s
	}
	n := len(s) - 4096
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], s[n:]
}

func newToolStreamFilter(sink streamSink) *toolStreamFilter {
	return &toolStreamFilter{sink: sink}
}

// Found 是否已发出 tool_calls。
func (f *toolStreamFilter) Found() bool { return f.found }

// FeedReason 透传思考增量。
func (f *toolStreamFilter) FeedReason(s string) error {
	if s == "" {
		return nil
	}
	return f.sink.Reasoning(s)
}

// FeedContent 喂入正文增量。
func (f *toolStreamFilter) FeedContent(s string) error {
	if f.inTool {
		f.toolBuf += s
		return f.tryCloseTool(false)
	}
	f.pending += s
	// 1) 转录标记调用行: [助手调用工具 Name 参数 {json}] → 转为真实 tool_call。
	//    谓「叙述即意图」——模型在长会话下用括号标记复述想执行的命令；
	//    post 态下同样转换（背靠背多行叙述对应多个调用）。
	if calls, pre, post, ok := parseBracketCallLine(f.pending); ok {
		if !f.inPost {
			if err := f.emitText(pre); err != nil {
				return err
			}
		}
		assignCallIDs(calls)
		for _, c := range calls {
			f.callIndex++
			if err := f.sink.ToolCall(f.callIndex-1, c); err != nil {
				return err
			}
		}
		f.found = true
		f.inPost = true
		f.pending = post
		return f.FeedContent("")
	}
	// 2) 转录标记结果行: [工具 … 返回结果] → 自证的假结果，整行抑制。
	if ls, le := findBracketResultLine(f.pending); ls >= 0 {
		f.pending = f.pending[:ls] + f.pending[le:]
	}
	if f.inPost {
		// post 态：正文抑制（多为编造的工具结果/续演），仅扫描后续起始围栏。
		if start, bodyStart := findTolerantOpener(f.pending); start >= 0 {
			f.toolBuf = f.pending[bodyStart:]
			f.pending = ""
			f.inTool = true
			return f.tryCloseTool(false)
		}
		// 与 textMode 相同的行边界保留（不变量见 cutLineHead）。
		_, f.pending = cutLineHead(f.pending, fenceRetainBytes)
		return nil
	}
	if start, bodyStart := findTolerantOpener(f.pending); start >= 0 {
		if err := f.emitText(f.pending[:start]); err != nil {
			return err
		}
		f.toolBuf = f.pending[bodyStart:]
		f.pending = ""
		f.inTool = true
		return f.tryCloseTool(false)
	}
	// 未见完整围栏：仅保留可能构成标记行/围栏前缀的尾部，其余立即透传。
	// 三个不变量集中实现在 cutLineHead/releaseOversizedLine。
	head, rest := cutLineHead(f.pending, fenceRetainBytes)
	if head == "" {
		head, rest = releaseOversizedLine(f.pending, 1<<16)
	}
	if head != "" {
		if err := f.emitText(head); err != nil {
			return err
		}
		f.pending = rest
	}
	return nil
}

// Close 流结束时冲刷残留缓冲：强制把悬置的围栏按闭合处理；
// post 态下残留正文属抑制区，不再透传。
func (f *toolStreamFilter) Close() error {
	if f.inTool {
		if err := f.tryCloseTool(true); err != nil {
			return err
		}
		f.inTool = false
	}
	if f.inPost {
		f.pending = ""
		f.toolBuf = ""
		return nil
	}
	if f.toolBuf != "" {
		if err := f.emitText(toolFenceOpen + f.toolBuf); err != nil {
			return err
		}
		f.toolBuf = ""
	}
	return f.emitText(f.pending)
}

func (f *toolStreamFilter) emitText(s string) error {
	if s == "" {
		return nil
	}
	return f.sink.Text(s)
}

// tryCloseTool 尝试在缓冲中定位围栏边界并下发 tool_calls。
// 缓冲里的 "```" 可能是闭合围栏，也可能是下一个调用的起始围栏
// （模型漏写闭合时把它当分隔符用）。判定依赖围栏之后的字节：
// 流式下字节未到齐时先等待（!force），流结束由 Close 强制按闭合处理。
func (f *toolStreamFilter) tryCloseTool(force bool) error {
	for {
		end, opener, after := nextFence(f.toolBuf, 0)
		if end < 0 {
			return nil // 未闭合，继续缓冲
		}
		// "```" 之后的字节是 "tool_call" 的前缀（含空）时无法判定语义，
		// 等待更多数据；force（流已结束）按闭合围栏处理。
		if trailing := f.toolBuf[end+3:]; strings.HasPrefix("tool_call", trailing) {
			if !force {
				return nil
			}
			opener, after = false, end+3
		}
		block := stripZeroWidth(f.toolBuf[:end])
		if calls, ok := parseCallBlock(block); ok {
			assignCallIDs(calls)
			for _, c := range calls {
				f.callIndex++
				if err := f.sink.ToolCall(f.callIndex-1, c); err != nil {
					return err
				}
			}
			f.found = true
			if opener {
				f.toolBuf = f.toolBuf[after:] // 起始围栏：继续缓冲下一个调用的 body
				continue
			}
			f.inTool = false
			f.inPost = true // 回合语义在 tool_calls 处结束：其后正文进入抑制态
			f.pending = f.toolBuf[after:]
			f.toolBuf = ""
			return f.FeedContent("")
		}
		// 解析失败：首个调用前按纯文本透传；已发出过调用（post 态）则抑制
		f.inTool = false
		if opener {
			f.pending = f.toolBuf[after:]
		} else {
			f.pending = f.toolBuf[end+3:]
		}
		f.toolBuf = ""
		if !f.found {
			if err := f.emitText("```tool_call" + block + "```"); err != nil {
				return err
			}
		}
		f.inPost = f.found
		return f.FeedContent("")
	}
}
