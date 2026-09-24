// 流式帧序：原生 tool_calls 与正文的先后关系（SPEC §28.4 决策 D 帧序契约）。
//
// 实测缺陷（2026-09-24，ZCode 里表现为"工具卡片出现在说明文字之前"）：
// 原生 tools 上游（腾讯 roles）的正文曾一律经过围栏过滤器；过滤器为识别 ```tool_call
// 前缀必须保留尾部未完成行（≤fenceRetainBytes），而原生 tool_calls 是**绕过过滤器直发**的
// → "工具调用之前那行没有换行结尾的正文"被推迟到流尾随 flt.Close() 才释放，
// 客户端收到的是「工具调用 → 正文」，与上游帧序（正文 → tool_calls）相反。
// 模型越常"先说一句再调工具"越容易命中，故在 agentic 客户端里频繁出现。
//
// 另一半是对称场景：工具调用**之后**才到的正文（收尾说明）若即时透传，会跑在
// 工具调用前面（后者在流末才一次性发出）——上游侧一并延后处理。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/auth"
)

// orderFrame 构造一帧 OpenAI 风格 SSE（用 json.Marshal 避免手写转义出错）。
func orderFrame(delta map[string]any, finish string) string {
	ch := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		ch["finish_reason"] = finish
	}
	b, _ := json.Marshal(map[string]any{"id": "c1", "model": "m", "choices": []any{ch}})
	return "data: " + string(b) + "\n\n"
}

func orderUpstream(t *testing.T, stream string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	t.Cleanup(up.Close)
	t.Setenv("OMNIGATE_TENCENT_BASE", up.URL) // NewTencent 构造时读取
	return up
}

// orderChat 经网关发一次带 tools 的流式请求，返回 SSE 原文。
func orderChat(t *testing.T, up *httptest.Server) string {
	t.Helper()
	srv, _, _, h := buildTestServer(t, up.URL, []*auth.Auth{tencentFakeAuth("u2", "tok2")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)
	body := `{"model":"kimi-k2.7","stream":true,"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"北京天气"}]}`
	raw, code := postChat(t, srv, body)
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, truncateText(string(raw), 200))
	}
	return string(raw)
}

// 正文先于 tool_calls 到达：网关必须保持「正文 → 调用」（原生路径不得让正文
// 经过围栏过滤器的尾部保留，否则正文会被推迟到流尾）。
func TestStreamOrderTextBeforeNativeToolCall(t *testing.T) {
	up := orderUpstream(t, orderFrame(map[string]any{"content": "我先查一下北京的天气。"}, "")+
		orderFrame(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": ""}}}}, "")+
		orderFrame(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "function": map[string]any{"arguments": `{"city":"Beijing"}`}}}}, "")+
		orderFrame(map[string]any{}, "tool_calls")+"data: [DONE]\n\n")

	out := orderChat(t, up)
	textAt := strings.Index(out, "我先查一下北京的天气。")
	toolAt := strings.Index(out, `"tool_calls"`)
	if textAt < 0 {
		t.Fatalf("正文未下发：%s", truncateText(out, 300))
	}
	if toolAt < 0 {
		t.Fatalf("tool_calls 未下发：%s", truncateText(out, 300))
	}
	if textAt > toolAt {
		t.Fatalf("帧序颠倒：正文出现在 tool_calls 之后（text@%d > tool@%d）\n%s",
			textAt, toolAt, truncateText(out, 600))
	}
}

// 对称场景：正文在 tool_calls 之后到达（收尾说明）——必须仍排在调用之后。
func TestStreamOrderTextAfterNativeToolCallStaysAfter(t *testing.T) {
	up := orderUpstream(t, orderFrame(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": "get_weather", "arguments": `{"city":"Beijing"}`}}}}, "")+
		orderFrame(map[string]any{"content": "以上是查询结果。"}, "")+
		orderFrame(map[string]any{}, "tool_calls")+"data: [DONE]\n\n")

	out := orderChat(t, up)
	toolAt := strings.Index(out, `"tool_calls"`)
	textAt := strings.Index(out, "以上是查询结果。")
	if toolAt < 0 {
		t.Fatalf("tool_calls 未下发：%s", truncateText(out, 300))
	}
	if textAt < 0 {
		t.Fatalf("收尾正文未下发（延后正文必须补发，不能丢）：%s", truncateText(out, 300))
	}
	if textAt < toolAt {
		t.Fatalf("帧序颠倒：正文跑到 tool_calls 之前（tool@%d text@%d）\n%s",
			toolAt, textAt, truncateText(out, 600))
	}
}

// 围栏路径（华为 codearts）行为不变：正文经围栏过滤器识别后一次性下发调用，
// 且正常闭合的围栏标记不得作为正文泄漏给客户端。
func TestStreamOrderFencePathUnchanged(t *testing.T) {
	fence := "```tool_call"
	args := `{"name":"Read","arguments":{"path":"/a"}}`
	// 收尾围栏是**裸** ```（起始围栏才带 tool_call）；收尾若也写成带 tool_call，
	// 会命中过滤器对"未闭合围栏"的回退（按纯文本透传）——那是设计行为，不是缺陷。
	content := "好的。\n" + fence + "\n" + args + "\n```\n"
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, orderFrame(map[string]any{"content": content}, "")+
			orderFrame(map[string]any{}, "stop")+"data: [DONE]\n\n")
	}})
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	body := `{"model":"glm-5.2","stream":true,"tools":[{"type":"function","function":{"name":"Read","description":"r","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hi"}]}`
	raw, code := postChat(t, srv, body)
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	out := string(raw)
	if !strings.Contains(out, `"tool_calls"`) || !strings.Contains(out, "Read") {
		t.Fatalf("围栏路径应仍能下发工具调用：%s", truncateText(out, 300))
	}
	if strings.Contains(out, fence) {
		t.Fatalf("已闭合围栏不得把标记泄漏成正文：%s", truncateText(out, 400))
	}
}
