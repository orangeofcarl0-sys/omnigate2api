// 模型级限流（429 + code 6004）识别与解封时刻解析（SPEC §28.4 决策 B 补充）。
package upstream

import (
	"testing"
	"time"
)

const rateLimitCN = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-22 09:46:39 UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"x"}`
const rateLimitEN = `{"code":6004,"msg":"Your usage has exceeded the rate limit. It will reset at 2026-09-05 01:57:00 UTC+8.","requestId":"y"}`

func TestIsModelRateLimit(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"中文 6004", rateLimitCN, true},
		{"英文 6004", rateLimitEN, true},
		{"只有文案标记无业务码", `{"msg":"抱歉，使用量已超出频率限制，请稍后再试"}`, true},
		{"裸 429 无证据", `{"error":"rate limited"}`, false},
		{"业务码其它", `{"code":14018,"msg":"额度已用尽"}`, false},
		{"正常响应", `{"choices":[]}`, false},
	}
	for _, c := range cases {
		if got := IsModelRateLimit(c.body); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if k := ClassifyTencent(429, rateLimitCN); k != TencentErrModelRateLimit {
		t.Fatalf("ClassifyTencent must classify 6004 as model rate limit, got %v", k)
	}
}

// 额度耗尽（14018「额度已用尽」）必须归入硬额度：旧标记表里的「额度用尽」匹配不到
// 「额度已用尽」（中间隔着「已」）。
func TestClassifyTencentQuotaExhausted(t *testing.T) {
	for _, body := range []string{`{"code":14018,"msg":"额度已用尽"}`, `{"code":14018,"msg":"Credits exhausted"}`} {
		if k := ClassifyTencent(429, body); k != TencentErrHardCredit {
			t.Fatalf("quota exhausted must map to hard credit: %s → %v", body, k)
		}
	}
}

func TestParseTencentResetAt(t *testing.T) {
	now := time.Date(2026, 9, 22, 8, 0, 0, 0, cstZone)
	for _, body := range []string{rateLimitCN, rateLimitEN} {
		at, ok := ParseTencentResetAt(body, now)
		if !ok {
			t.Fatalf("must parse reset time from %s", body)
		}
		if at.Location() != cstZone && at.In(cstZone).Location().String() != "CST" {
			t.Fatalf("reset time must be CST: %v", at)
		}
	}
	// 只有时刻（无日期）：取当天，若已过则顺延到明天
	at, ok := ParseTencentResetAt(`{"msg":"将在 09:30:00 UTC+8 重置"}`, now)
	if !ok || at.Sub(now) != 90*time.Minute {
		t.Fatalf("time-only reset must be today 09:30, got %v ok=%v", at, ok)
	}
	at, ok = ParseTencentResetAt(`{"msg":"将在 07:00:00 UTC+8 重置"}`, now)
	if !ok || at.Sub(now) != 23*time.Hour {
		t.Fatalf("past time-only reset must roll to tomorrow, got %v", at)
	}
	if _, ok := ParseTencentResetAt(`{"msg":"no time here"}`, now); ok {
		t.Fatal("unparseable body must report failure")
	}
}

// 冷却截止：优先上游声明；解析失败退默认；超出上限被钳住（防离谱时间废掉账号）。
func TestModelRateLimitUntil(t *testing.T) {
	now := time.Date(2026, 9, 22, 8, 0, 0, 0, cstZone)
	until := ModelRateLimitUntil(rateLimitCN, now, time.Minute, 24*time.Hour)
	if want := time.Date(2026, 9, 22, 9, 46, 39, 0, cstZone); !until.Equal(want) {
		t.Fatalf("declared reset must win: got %v want %v", until, want)
	}
	if got := ModelRateLimitUntil(`{"msg":"nothing"}`, now, 45*time.Second, 24*time.Hour); got.Sub(now) != 45*time.Second {
		t.Fatalf("fallback window must apply: %v", got.Sub(now))
	}
	far := `{"code":6004,"msg":"将在 2030-01-01 00:00:00 UTC+8 重置"}`
	if got := ModelRateLimitUntil(far, now, time.Minute, 24*time.Hour); got.Sub(now) != 24*time.Hour {
		t.Fatalf("horizon must clamp absurd reset times: %v", got.Sub(now))
	}
}
