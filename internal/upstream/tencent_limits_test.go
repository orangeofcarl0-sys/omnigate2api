// 模型级限流（429 + code 6004）识别与解封时刻解析（SPEC §28.4 决策 B 补充）。
package upstream

import (
	"net/http"
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

// 11140 的两种形态必须**按文案**分野（不能按 code）：`request illegal` = 账号级授权
// 封禁（不自愈、需重登）；`rate-limiting` = 限流（软冷却）。口径与社区两个同目标项目一致。
func TestClassifyTencent11140Variants(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   TencentErrKind
	}{
		{"封禁：request illegal（本机 2026-09-27 实测形态；displayMsg 只是通用文案）",
			http.StatusForbidden,
			`{"code":11140,"msg":"request illegal","requestId":"22e3ed52","displayMsg":{"en":"The content did not pass the safety review. Please adjust and retry.","zh":"内容未通过安全审核，请调整后重试"}}`,
			TencentErrAccountBanned},
		{"限流：rate-limiting 变体（社区实测形态）",
			http.StatusOK,
			`{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`,
			TencentErrSoftRate},
		{"模型级限流仍是 6004",
			http.StatusTooManyRequests,
			`{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-22 09:46:39 UTC+8 重置，您也可以切换其他模型继续使用。"}`,
			TencentErrModelRateLimit},
		{"请求侧：内容策略拦截", http.StatusBadRequest, `{"code":11132,"msg":"request blocked by security policy"}`, TencentErrClientSide},
		{"请求侧：prompt too long", http.StatusBadRequest, `{"code":11115,"msg":"prompt is too long"}`, TencentErrClientSide},
		{"会话死亡", http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`, TencentErrSessionDead},
		{"其余归 Other", http.StatusBadRequest, `{"code":12345,"msg":"whatever"}`, TencentErrOther},
	}
	for _, c := range cases {
		if got := ClassifyTencent(c.status, c.body); got != c.want {
			t.Fatalf("%s: kind=%v want %v", c.name, got, c.want)
		}
	}
}
