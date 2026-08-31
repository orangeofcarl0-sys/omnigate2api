package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"omnigate2api/internal/upstream"
)

// 逐字节喂入 CJK 文本：切割点落在多字节字符中间时不得产生 U+FFFD。
func TestToolStreamFilterNoMojibake(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := upstream.NewSSEChunkWriter(rec, "test-model")
	f := newToolStreamFilter(&chatSink{sw: sw})

	text := "按照以下步骤继续：G18 exit report §4 点名的六项收尾事项——tarball 共六个包，构建流程（test:package 消耗时间、verify:release 仍是占位）。先确认这两个脚本与清单的一致性。"
	// 逐字节喂入，模拟最恶劣的分块
	for i := 0; i < len(text); i++ {
		if err := f.FeedContent(text[i : i+1]); err != nil {
			t.Fatalf("FeedContent: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body := rec.Body.String()
	got := extractStreamedContent(t, body)
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("output contains U+FFFD mojibake:\n%s", got)
	}
	if got != text {
		t.Fatalf("content mismatch:\n want=%q\n got =%q", text, got)
	}
}

// 围栏跨块：起始围栏逐字节到达也要能识别并下发 tool_calls。
func TestToolStreamFilterFenceAcrossChunks(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := upstream.NewSSEChunkWriter(rec, "test-model")
	f := newToolStreamFilter(&chatSink{sw: sw})

	full := "好的，马上读取。```tool_call\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"F:/x/y.mjs\"}}\n```"
	for i := 0; i < len(full); i++ {
		if err := f.FeedContent(full[i : i+1]); err != nil {
			t.Fatalf("FeedContent: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !f.Found() {
		t.Fatalf("tool call not detected; body=%s", rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"name":"Read"`) {
		t.Fatalf("tool_calls chunk missing:\n%s", body)
	}
	if got := extractStreamedContent(t, body); strings.Contains(got, "tool_call") {
		t.Fatalf("fence text leaked into content:\n%s", got)
	}
}

// Windows 裸反斜杠路径的 tool_call JSON 应被修复后成功解析。
func TestRepairBareBackslashes(t *testing.T) {
	block := `{"name":"Read","arguments":{"file_path":"F:\Codex_Work_Space\DSH plugin\ordarium\tools\verify-release.mjs"}}`
	calls, ok := parseCallBlock(block)
	if !ok {
		t.Fatalf("repair failed to parse block: %s", block)
	}
	if calls[0].Name != "Read" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, `F:\\Codex_Work_Space`) {
		t.Fatalf("arguments not repaired: %s", calls[0].Arguments)
	}
}

// 用户实测异常：模型漏写闭合围栏，把 ```tool_call 当分隔符，中间夹零宽字符。
// 两个调用都应被解析，正文里不得泄漏围栏文本。
func TestToolStreamFilterFenceAsSeparator(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := upstream.NewSSEChunkWriter(rec, "test-model")
	f := newToolStreamFilter(&chatSink{sw: sw})

	zw := "\u200b"
	full := "先核账再收尾：```tool_call\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"F:/x/verify-release.mjs\"}}" +
		zw + "```tool_call\n{\"name\":\"Bash\",\"arguments\":{\"command\":\"git status --short\"}}" +
		"\n```"
	for i := 0; i < len(full); i++ {
		if err := f.FeedContent(full[i : i+1]); err != nil {
			t.Fatalf("FeedContent: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !f.Found() {
		t.Fatalf("tool calls not detected; body=%s", rec.Body.String())
	}
	body := rec.Body.String()
	if n := strings.Count(body, `"tool_calls"`); n != 2 {
		t.Fatalf("expected 2 tool_calls deltas, got %d:\n%s", n, body)
	}
	if got := extractStreamedContent(t, body); strings.Contains(got, "tool_call") || strings.Contains(got, "```") {
		t.Fatalf("fence text leaked into content:\n%s", got)
	}
}

// 聚合路径同样的分隔符围栏模式。
func TestExtractToolCallsFenceAsSeparator(t *testing.T) {
	zw := "\u200b"
	text := "正文。```tool_call\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"F:/x/a.mjs\"}}" + zw +
		"```tool_call\n{\"name\":\"Bash\",\"arguments\":{\"command\":\"git log\"}}\n```"
	calls, rest, found := extractToolCalls(text)
	if !found || len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d found=%v rest=%q", len(calls), found, rest)
	}
	if calls[0].Name != "Read" || calls[1].Name != "Bash" {
		t.Fatalf("calls = %+v", calls)
	}
	if strings.Contains(rest, "tool_call") {
		t.Fatalf("fence leaked into rest: %q", rest)
	}
}

// 实测异常：调用发出后模型继续在正文里编造 [[工具 … 返回结果]] 假结果与
// 乱序围栏。post 态必须抑制这些内容，只保留调用前的正文。
func TestToolStreamFilterPostCallSuppression(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := upstream.NewSSEChunkWriter(rec, "test-model")
	f := newToolStreamFilter(&chatSink{sw: sw})

	full := "我先查看该文件夹的内容结构。```tool_call\n" +
		`{"name":"Bash","arguments":{"command":"ls -la \"F:/x\""}}` + "\n```\n" +
		"[[工具 Bash 返回结果]] total 24\ndrwxr-xr-x fake output\n" +
		"tool_call```\n{\"name\":\"Read\",\"arguments\":{\"file_path\":\"F:/y\"}}\n```"
	for i := 0; i < len(full); i++ {
		if err := f.FeedContent(full[i : i+1]); err != nil {
			t.Fatalf("FeedContent: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !f.Found() {
		t.Fatalf("tool call not detected")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"Bash"`) {
		t.Fatalf("real tool call missing:\n%s", body)
	}
	got := extractStreamedContent(t, body)
	for _, bad := range []string{"[", "返回结果", "tool_call", "fake output", "Read"} {
		if strings.Contains(got, bad) {
			t.Fatalf("post-call content leaked into stream (found %q):\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "我先查看该文件夹的内容结构") {
		t.Fatalf("pre-call preamble lost:\n%s", got)
	}
}

// 非流式路径同样的回合语义：首个调用之后的编造内容不进入 content。
func TestExtractToolCallsPostCallTruncation(t *testing.T) {
	text := "先看结构。```tool_call\n{\"name\":\"Bash\",\"arguments\":{\"command\":\"ls\"}}\n```\n" +
		"[[工具 Bash 返回结果]] fake result"
	calls, rest, found := extractToolCalls(text)
	if !found || len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d found=%v", len(calls), found)
	}
	if strings.Contains(rest, "fake result") || strings.Contains(rest, "返回结果") {
		t.Fatalf("post-call fabrication leaked into rest: %q", rest)
	}
	if !strings.Contains(rest, "先看结构") {
		t.Fatalf("pre-call text lost: %q", rest)
	}
}

// 实测异常（sess_459675e6 L3）：模型不用围栏，直接用折叠历史私标记叙述：
// [助手调用工具 Bash 参数 {json}] / [工具 call_… 返回结果]。
// 叙述行必须转为真实 tool_calls，假结果行必须被抑制。
func TestToolStreamFilterBracketNarration(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := upstream.NewSSEChunkWriter(rec, "test-model")
	f := newToolStreamFilter(&chatSink{sw: sw})

	full := "文档已读完，快速补齐后再分析。\n" +
		"[助手调用工具 Bash 参数 {\"command\":\"find \\\"F:/x/jlw\\\" -maxdepth 3 | head -60\"}]\n" +
		"[助手调用工具 Bash 参数 {\"command\":\"head -5 F:/x/_dist.txt\"}]\n\n" +
		"[工具 call_5be85f65670a4b399db 返回结果]\n" +
		"jlw_replication 下有 audit/ downloads/ 两个子目录。\n\n" +
		"总结：分析完成。"
	for i := 0; i < len(full); i++ {
		if err := f.FeedContent(full[i : i+1]); err != nil {
			t.Fatalf("FeedContent: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !f.Found() {
		t.Fatalf("narration calls not converted")
	}
	body := rec.Body.String()
	if n := strings.Count(body, `"tool_calls"`); n != 2 {
		t.Fatalf("expected 2 converted calls, got %d:\n%s", n, body)
	}
	got := extractStreamedContent(t, body)
	if !strings.Contains(got, "文档已读完") {
		t.Fatalf("pre-narration text lost:\n%s", got)
	}
	for _, bad := range []string{"助手调用工具", "返回结果", "find \\\"F:/x/jlw\\\"", "子目录", "总结：分析完成"} {
		if strings.Contains(got, bad) {
			t.Fatalf("narration/results leaked into stream (found %q):\n%s", bad, got)
		}
	}
}

// 非流式：括号叙述同样转换并截断结果叙述。
func TestExtractToolCallsBracketNarration(t *testing.T) {
	text := "先补齐。\n" +
		"[助手调用工具 Bash 参数 {\"command\":\"find F:/x -maxdepth 2\"}]\n\n" +
		"[工具 call_aa 返回结果]\n" +
		"F:/x 下有 a/ b/。\n"
	calls, rest, found := extractToolCalls(text)
	if !found || len(calls) != 1 {
		t.Fatalf("found=%v calls=%+v", found, calls)
	}
	if calls[0].Name != "Bash" || !strings.Contains(calls[0].Arguments, "find F:/x") {
		t.Fatalf("call=%+v", calls[0])
	}
	if !strings.Contains(rest, "先补齐") {
		t.Fatalf("pre text lost: %q", rest)
	}
	if strings.Contains(rest, "返回结果") || strings.Contains(rest, "a/ b/") {
		t.Fatalf("result narration leaked: %q", rest)
	}
}

// extractStreamedContent 从 SSE body 里重组 content delta（测试辅助）。
func extractStreamedContent(t *testing.T, body string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			if c, ok := chunk.Choices[0].Delta["content"].(string); ok {
				sb.WriteString(c)
			}
		}
	}
	return sb.String()
}

// cutLineHead 不变量: rune 边界 + 行整缓冲。
func TestCutLineHead(t *testing.T) {
	// 行未完成: 整行缓冲
	if h, r := cutLineHead("中文段落没有换行", 20); h != "" || r != "中文段落没有换行" {
		t.Fatalf("unfinished line must buffer: h=%q r=%q", h, r)
	}
	// 完整行: 从换行处切, 头部不含切断的 rune
	h, r := cutLineHead("第一行内容。\n第二行标记[助手调用工具 Bash 参数 {\"a\":1}]", 6)
	if h != "第一行内容。\n" || r != "第二行标记[助手调用工具 Bash 参数 {\"a\":1}]" {
		t.Fatalf("line cut wrong: h=%q r=%q", h, r)
	}
	// 超行防呆: 长单行强制释放且 rune 边界安全
	long := strings.Repeat("中", 1<<17)
	h2, r2 := releaseOversizedLine(long, 1<<16)
	if h2 == "" || !utf8.ValidString(h2) || !utf8.ValidString(r2) || len(h2)+len(r2) != len(long) {
		t.Fatalf("oversize release invalid: hlen=%d rlen=%d", len(h2), len(r2))
	}
}
