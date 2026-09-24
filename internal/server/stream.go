// 协议无关流式出站（SPEC §16.1）：上游 SSE 归一化为
// (content/reasoning/tool_calls/finish) 中间流后，由各协议 writer（sink）
// 重建为 chat / anthropic / responses 线格式。工具围栏解析（toolStreamFilter）
// 同样以 sink 为输出，三协议共用。
package server

import (
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// streamProtocol 入站协议标识（SPEC §13）。
type streamProtocol string

const (
	protoChat      streamProtocol = "chat"
	protoAnthropic streamProtocol = "anthropic"
	protoResponses streamProtocol = "responses"
)

// streamSink 协议 writer：中间流事件 → 各自 SSE 线格式。
type streamSink interface {
	Text(s string) error                      // 正文增量
	Reasoning(s string) error                 // 思考增量（协议无对应事件时丢弃）
	ToolCall(idx int, c openAIToolCall) error // 完整工具调用（模拟层一次性解析出 arguments）
	Finish(fin string) error                  // 终止（含各自协议的结束标记）
	Error(msg string) error                   // 上游错误帧 → 各协议错误事件（终止流）
}

// chatSink OpenAI SSE chunk 线格式。
type chatSink struct {
	sw *upstream.SSEChunkWriter
}

func (s *chatSink) Text(t string) error {
	return s.sw.Delta(map[string]any{"content": t})
}

func (s *chatSink) Reasoning(t string) error {
	return s.sw.Delta(map[string]any{"reasoning_content": t})
}

func (s *chatSink) ToolCall(idx int, c openAIToolCall) error {
	return s.sw.Delta(map[string]any{"tool_calls": []map[string]any{{
		"index": idx, "type": "function", "id": c.ID,
		"function": map[string]any{"name": c.Name, "arguments": c.Arguments},
	}}})
}

func (s *chatSink) Finish(fin string) error {
	return s.sw.Finish(fin)
}

func (s *chatSink) Error(msg string) error { return s.sw.Error(msg) }

// newSink 按入站协议构造 writer；isRateLimit 为限流分类器（Profile 驱动）。
func newSink(proto streamProtocol, w http.ResponseWriter, model string, isRateLimit upstream.RateClassifier) streamSink {
	switch proto {
	case protoChat:
		return &chatSink{sw: upstream.NewSSEChunkWriter(w, model, isRateLimit)}
	case protoAnthropic:
		return newAnthropicSink(w, model)
	case protoResponses:
		return newResponsesSink(w, model)
	}
	return nil
}

// writeProtoError 各协议错误信封（SPEC §13.4）：anthropic 顶层 type:error、
// responses OpenAI 风格 error 信封、chat 沿用现有。
// streamOut 统一流式管线：上游 SSE → 协议无关增量 → sink。
//
// 行为契约（SPEC §16.1）：
//   - 上游错误帧 → sink.Error 终止：限流走软冷却、其余计一次账号错误，回合失败；
//   - 正常流 → 循环后冲刷围栏（flt.Close），合成 finish（工具→tool_calls，
//     上游终帧，空→stop）后 sink.Finish；转录回声/无上下文守卫检测按中间流文本执行。
//
// sink.Finish 必须晚于 flt.Close（残留缓冲冲刷可能产生 text/tool_call 增量，
// 帧序契约：正文/调用增量全部发出后才允许终止帧）。
func (h *Handler) streamOut(sink streamSink, acct *pool.Account, model string, profile *adapt.UpstreamProfile, matchedKey string, rc io.ReadCloser) bool {
	flt := newToolStreamFilter(sink)
	// 围栏模拟是 **Profile 声明的能力**（Tool.FenceOpen，如华为 codearts）；原生 tools
	// 上游（腾讯 roles）的正文**不得**经过围栏过滤器：
	//   过滤器为了识别围栏前缀必须保留尾部未完成行（≤fenceRetainBytes），而原生
	//   tool_calls 是绕过过滤器直发的 → "工具调用之前那行没有换行结尾的正文"被推迟到
	//   流尾才随 flt.Close() 释放，客户端收到「工具调用 → 正文」——帧序颠倒。
	//   实测（ZCode）：工具卡片跑到说明文字前面，且模型越常先说一句再调工具越容易发生。
	fenceMode := profile != nil && profile.Tool.FenceOpen != ""
	terminal := false
	finishSeen := ""
	var lastUpErr string
	var echo strings.Builder
	nativeTools := false // 原生 tool_calls 路径（roles 上游）已发出 ≥1 调用（§28.4 决策 D）
	_, serr := upstream.StreamDeltasWithTools(rc, func(content, reason, finish string, upErr error) error {
		echo.WriteString(content)
		h.cfg.Pool.PingKeepalive(acct.Name) // 长流期间保活
		if upErr != nil {
			terminal = true
			lastUpErr = upErr.Error()
			return sink.Error(lastUpErr)
		}
		if reason != "" {
			if err := sink.Reasoning(reason); err != nil {
				return err
			}
		}
		if content != "" {
			if fenceMode {
				if err := flt.FeedContent(content); err != nil {
					return err
				}
			} else if err := sink.Text(content); err != nil {
				return err // 原生路径：正文按到达顺序直发（保持与 tool_calls 的相对次序）
			}
		}
		if finish != "" {
			finishSeen = finish
		}
		return nil
	}, func(t upstream.ToolCall) error {
		nativeTools = true
		// 单帧完整参数直发（帧序：全部 tool_calls 先于终止帧，由
		// StreamDeltasWithTools 保证）；文本围栏路径经 flt 互不影响。
		return sink.ToolCall(t.Index, openAIToolCall{ID: t.ID, Name: t.Name, Arguments: t.Arguments})
	})
	rc.Close()
	h.cfg.Pool.ReleaseLock(acct.Name)
	if serr != nil {
		log.Printf("chat stream account=%s error: %v", acct.Name, serr)
		if !isClientCancel(serr) {
			h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
		}
		h.turnFailure(profile, matchedKey)
		return false
	}
	if terminal {
		log.Printf("chat stream account=%s upstream error frame: %s", acct.Name, lastUpErr)
		switch {
		case isQuotaError(lastUpErr):
			// 额度不足（华为 MaaS 福利 4291 分钟级限流）：软冷却 60s 不累计
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, 60*time.Second, lastUpErr)
		case profile.IsRateLimit(lastUpErr):
			if acct.ProfileID == "workbuddy" && upstream.IsModelRateLimit(lastUpErr) {
				h.settleModelRateLimit(acct, model, lastUpErr) // 模型级：只冷却 (账号,模型)
			} else {
				h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, 45*time.Second, lastUpErr)
			}
		default:
			h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
		}
		h.turnFailure(profile, matchedKey)
		return false
	}
	// 冲刷围栏缓冲（post 态残留正文属于抑制区，不发出）
	if err := flt.Close(); err != nil {
		log.Printf("chat stream account=%s flush error: %v", acct.Name, err)
		return false
	}
	fin := finishSeen
	if flt.Found() || nativeTools {
		fin = "tool_calls" // 回合语义在工具调用处结束
	}
	if fin == "" {
		fin = "stop"
	}
	if err := sink.Finish(fin); err != nil {
		log.Printf("chat stream account=%s finish error: %v", acct.Name, err)
		return false
	}
	if transcriptEcho(echo.String()) {
		log.Printf("TRANSCRIPT_ECHO model=%s chars=%d", model, len([]rune(echo.String())))
		h.dumpEcho(model, echo.String())
	}
	if h.cfg.SessionMode == "incremental" && noContextEcho(echo.String()) {
		h.recordGuard(profile, adapt.GuardNoInstruction, time.Now())
	}
	return true
}
