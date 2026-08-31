// 入站规范层（inbound/tools）：围栏与转录标记解析、JSON 修复（ToolProfile 驱动面）。
// 工具模拟的整体设计见 tools.go。
package server

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// extractToolCalls 从模型输出中提取工具调用（围栏/转录标记/裸 JSON 三种形态）。
func extractToolCalls(text string) (calls []openAIToolCall, rest string, found bool) {
	// 0. 转录标记行（模型叙述模式）：[助手调用工具 Name 参数 {json}] 连续行
	//    逐个转换为调用；首个标记行前的正文保留，其后的"结果叙述"截断。
	var bcalls []openAIToolCall
	scan := text
	beforeFirst := ""
	captured := false
	for {
		cs, pre, post, ok := parseBracketCallLine(scan)
		if !ok {
			break
		}
		if !captured && pre != "" {
			beforeFirst = pre
			captured = true
		}
		bcalls = append(bcalls, cs...)
		scan = post
	}
	if len(bcalls) > 0 {
		return bcalls, strings.TrimSpace(beforeFirst), true
	}
	rest = text
	// 1. 优先找 ```tool_call ... ``` 代码块（可能多个）。
	for {
		start, bodyStart := findTolerantOpener(rest)
		if start < 0 {
			break
		}
		end, opener, after := nextFence(rest, bodyStart)
		if end < 0 {
			break // 未闭合，不吞
		}
		block := stripZeroWidth(rest[bodyStart:end])
		if c, ok := parseCallBlock(block); ok {
			if !captured {
				beforeFirst = rest[:start]
				captured = true
			}
			calls = append(calls, c...)
			if opener {
				// 该 "```" 实为下一个调用的起始围栏（模型漏写闭合围栏）：
				// 保留它在原位，下一轮迭代从这里继续开块。
				rest = strings.TrimSpace(rest[:start] + " " + rest[end:])
			} else {
				rest = strings.TrimSpace(rest[:start] + " " + rest[after:])
			}
		} else {
			break // 有围栏但解析失败，不硬吞，交给上层按纯文本处理
		}
	}
	if len(calls) > 0 {
		// 回合语义在首个 tool_call 处结束：其后正文多为编造的工具结果/续演，
		// 不进入 content（与流式路径的 post 抑制一致）。
		return calls, strings.TrimSpace(beforeFirst), true
	}
	// 2. 兜底：找含 "tool_calls" 键的 JSON 对象（裸 JSON 回复）。
	if idx := strings.Index(rest, "\"tool_calls\""); idx >= 0 {
		if objStart := strings.LastIndex(rest[:idx], "{"); objStart >= 0 {
			if obj, end := scanJSONObject(rest, objStart); obj != "" {
				if c, ok := parseToolCallsJSON(obj); ok {
					return c, strings.TrimSpace(rest[:objStart] + " " + rest[end:]), true
				}
			}
		}
	}
	// 3. 兜底：单个 {"name":...,"arguments":...} 对象。
	if idx := strings.Index(rest, "\"arguments\""); idx >= 0 {
		if objStart := strings.LastIndex(rest[:idx], "{"); objStart >= 0 {
			if obj, end := scanJSONObject(rest, objStart); obj != "" {
				if c, ok := parseCallBlock(obj); ok {
					return c, strings.TrimSpace(rest[:objStart] + " " + rest[end:]), true
				}
			}
		}
	}
	return nil, text, false
}

// parseCallBlock 解析单个或多个 {"name","arguments"} 对象（也兼容 {"tool_calls":[...]} 包装）。
// 严格解析失败时先做一次裸反斜杠修复再试——模型输出 Windows 路径时常漏转义
// （F:\Codex → F:\\Codex），这类围栏不应整体退化为纯文本。
func parseCallBlock(block string) ([]openAIToolCall, bool) {
	block = stripZeroWidth(strings.TrimSpace(block))
	if block == "" {
		return nil, false
	}
	if calls, ok := parseToolCallsJSON(block); ok {
		return calls, true
	}
	var single struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(block), &single); err != nil {
		repaired := repairBareBackslashes(block)
		if err2 := json.Unmarshal([]byte(repaired), &single); err2 != nil || single.Name == "" {
			return nil, false
		}
	}
	if single.Name == "" {
		return nil, false
	}
	return []openAIToolCall{{Name: single.Name, Arguments: rawJSONString(single.Arguments)}}, true
}

// repairBareBackslashes 把后跟非合法 JSON 转义字符的反斜杠补成双反斜杠。
// 合法转义（\" \\ \/ \b \f \n \r \t \uXXXX）保持原样。
func repairBareBackslashes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't', 'u':
				sb.WriteByte(s[i]) // 合法转义，保持
				continue
			default:
				sb.WriteString(`\\`) // 裸反斜杠，补转义
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// parseToolCallsJSON 解析 {"tool_calls":[{"name","arguments"},...]} 包装形态。
func parseToolCallsJSON(raw string) ([]openAIToolCall, bool) {
	var wrap struct {
		ToolCalls []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil || len(wrap.ToolCalls) == 0 {
		return nil, false
	}
	var out []openAIToolCall
	for _, c := range wrap.ToolCalls {
		if c.Name == "" {
			return nil, false
		}
		out = append(out, openAIToolCall{Name: c.Name, Arguments: rawJSONString(c.Arguments)})
	}
	return out, true
}

// scanJSONObject 从 s[start]（须为 '{'）开始扫描配对的 JSON 对象，
// 返回对象文本与结束下标（exclusive）。失败返回 "", start。
func scanJSONObject(s string, start int) (string, int) {
	if start < 0 || start >= len(s) || s[start] != '{' {
		return "", start
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// 字符串内的花括号不计
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], i + 1
			}
		}
	}
	return "", start
}

// assignCallIDs 给解析出的调用分配 OpenAI 风格 id（call_xxx）。
func assignCallIDs(calls []openAIToolCall) {
	for i := range calls {
		if calls[i].ID == "" {
			calls[i].ID = "call_" + randHex(24)
		}
	}
}

// ---------------------------------------------------------------------------
// 围栏扫描助手：容忍模型在围栏标记里夹带零宽字符/空格，并区分
// 闭合围栏与「下一个调用的起始围栏」（模型漏写闭合时 ```tool_call
// 会直接充当分隔符）。
// ---------------------------------------------------------------------------

func fenceMarkerJunk(r rune) bool {
	switch r {
	case ' ', '\t', '\u200b', '\u200c', '\u200d', '\ufeff':
		return true
	}
	return false
}

// stripZeroWidth 移除零宽字符（JSON 里的隐形杂质，破坏解析）。
func stripZeroWidth(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\ufeff':
			return -1
		}
		return r
	}, s)
}

// findTolerantOpener 找起始围栏 "```" + 杂散字符* + "tool_call"，
// 返回（围栏起点，body 起点）；找不到返回 (-1, -1)。
func findTolerantOpener(s string) (int, int) {
	for i := 0; i+3 <= len(s); {
		j := strings.Index(s[i:], "```")
		if j < 0 {
			return -1, -1
		}
		start := i + j
		k := start + 3
		for k < len(s) {
			r, sz := utf8.DecodeRuneInString(s[k:])
			if !fenceMarkerJunk(r) {
				break
			}
			k += sz
		}
		if strings.HasPrefix(s[k:], "tool_call") {
			return start, k + len("tool_call")
		}
		i = start + 3
	}
	return -1, -1
}

// nextFence 从 s[from:] 找下一个 "```"，判断它是闭合围栏还是下一个调用的
// 起始围栏（``` 后紧跟 tool_call）。返回（``` 位置, 是否起始围栏, 标记之后下标）。
func nextFence(s string, from int) (int, bool, int) {
	j := strings.Index(s[from:], "```")
	if j < 0 {
		return -1, false, -1
	}
	idx := from + j
	k := idx + 3
	for k < len(s) {
		r, sz := utf8.DecodeRuneInString(s[k:])
		if !fenceMarkerJunk(r) {
			break
		}
		k += sz
	}
	if strings.HasPrefix(s[k:], "tool_call") {
		return idx, true, k + len("tool_call")
	}
	return idx, false, idx + 3
}

// ---------------------------------------------------------------------------
// 转录标记行识别：模型在长会话中会用折叠历史的私标记「叙述」而非围栏——
//   [助手调用工具 <name> 参数 {json}]  → 转为真实 tool_call（叙述即意图）；
//   [工具 <xxx> 返回结果] / [[工具 … 返回结果]] → 自证的假结果，整行抑制。
// ---------------------------------------------------------------------------

const bracketCallMarker = "[助手调用工具"

// parseBracketCallLine 识别一行 [助手调用工具 <name> 参数 {json}]（容忍 [[ 双括号），
// 返回（调用, 行前正文, 行后剩余, 是否命中）。仅当标记位于行首方可识别，
// 避免正文叙述里提到该格式时误转换。
func parseBracketCallLine(s string) ([]openAIToolCall, string, string, bool) {
	i := strings.Index(s, bracketCallMarker)
	if i < 0 {
		return nil, "", "", false
	}
	lineStart := strings.LastIndex(s[:i], "\n") + 1
	if strings.TrimSpace(s[lineStart:i]) != "" {
		return nil, "", "", false // 行首带正文，不识别
	}
	rest := s[i+len(bracketCallMarker):]
	nameEnd := strings.Index(rest, " 参数 ")
	if nameEnd <= 0 {
		return nil, "", "", false
	}
	name := strings.Trim(strings.TrimSpace(rest[:nameEnd]), `"`)
	argsStart := i + len(bracketCallMarker) + nameEnd + len(" 参数 ")
	obj, end := scanJSONObject(s, argsStart)
	if obj == "" {
		return nil, "", "", false
	}
	closeLen := 1
	if strings.HasPrefix(s[end:], "]]") {
		closeLen = 2
	} else if !strings.HasPrefix(s[end:], "]") {
		return nil, "", "", false
	}
	post := s[end+closeLen:]
	// 行内紧跟其它调用标记时不吞
	if !strings.HasPrefix(strings.TrimSpace(post), bracketCallMarker) && strings.Contains(strings.SplitN(post, "\n", 2)[0], " 参数 ") {
		return nil, "", "", false
	}
	calls, ok := parseCallBlock(obj)
	if !ok {
		// 括号行参数对象多为纯 arguments（{"command":…}），无 name/arguments 键，
		// 以行内提取的工具名直接构造调用。
		calls = []openAIToolCall{{Name: name, Arguments: rawJSONString(json.RawMessage(obj))}}
		ok = name != ""
	}
	if !ok {
		return nil, "", "", false
	}
	for i := range calls {
		if calls[i].Name == "" {
			calls[i].Name = name
		}
	}
	return calls, s[:lineStart], post, true
}

// findBracketResultLine 定位一行 [工具 … 返回结果]（容忍 [[ 双括号），
// 返回（行首, 含换行的行尾）。找不到返回 -1。
func findBracketResultLine(s string) (int, int) {
	for _, marker := range []string{"[[工具 ", "[工具 "} {
		i := strings.Index(s, marker)
		if i < 0 {
			continue
		}
		lineStart := strings.LastIndex(s[:i], "\n") + 1
		if strings.TrimSpace(s[lineStart:i]) != "" {
			continue
		}
		nl := strings.Index(s[i:], "\n")
		lineEnd := len(s)
		if nl >= 0 {
			lineEnd = i + nl
		}
		tail := strings.TrimSpace(s[i:lineEnd])
		if strings.HasSuffix(tail, "]]") || strings.HasSuffix(tail, "]") {
			incl := lineEnd
			if nl >= 0 {
				incl++
			}
			return lineStart, incl
		}
	}
	return -1, -1
}
