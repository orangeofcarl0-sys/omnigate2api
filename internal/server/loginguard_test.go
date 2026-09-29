// 登录授权频次闸（SPEC §24.6）——这是 2026-09-28「账号访问受限」事故的防线，所以三条规则
// 都要有确定性测试：连点（最小间隔）、点不停（窗口上限）、失败还点（连续失败冷却）。
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testGuard() *loginGuard {
	return newLoginGuard(loginGuardConfig{
		MinInterval: 60 * time.Second, Window: time.Hour, MaxPerWindow: 100,
		FailureThreshold: 3, FailureCooldown: 20 * time.Minute,
	})
}

// 连点必须被拦，且要给出还要等多久（面板据此倒计时）。
func TestLoginGuardMinInterval(t *testing.T) {
	g := testGuard()
	ch := guardChanTencGL
	t0 := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)

	if d := g.allow(ch, t0); !d.OK {
		t.Fatalf("第一次应放行: %+v", d)
	}
	g.recordStart(ch, 10*time.Minute, t0)
	g.recordDone(ch, "u1", t0.Add(time.Second)) // 登录很快完成

	// 事故形态：59 秒内第 2/3/4 次授权
	for _, off := range []time.Duration{5 * time.Second, 24 * time.Second, 42 * time.Second} {
		d := g.allow(ch, t0.Add(off))
		if d.OK {
			t.Fatalf("t+%s 连点必须被拦（2026-09-28 就是 59s 内 4 次把登录页打成受限的）", off)
		}
		if d.RetryAfter <= 0 {
			t.Fatalf("被拦时必须给出等待时长: %+v", d)
		}
	}
	// 间隔过后放行
	if d := g.allow(ch, t0.Add(61*time.Second)); !d.OK {
		t.Fatalf("过了最小间隔应放行: %+v", d)
	}
}

// 窗口上限：即使每次都在最小间隔之外，一小时内的总量也要有上限。
func TestLoginGuardWindowCap(t *testing.T) {
	// 这一条专测窗口上限，所以把上限压到 3（其它用例用宽松上限，避免互相干扰）。
	g := newLoginGuard(loginGuardConfig{
		MinInterval: 60 * time.Second, Window: time.Hour, MaxPerWindow: 3,
		FailureThreshold: 3, FailureCooldown: 20 * time.Minute,
	})
	ch := guardChanHuawei
	t0 := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		at := t0.Add(time.Duration(i) * 2 * time.Minute)
		if d := g.allow(ch, at); !d.OK {
			t.Fatalf("第 %d 次应放行: %+v", i+1, d)
		}
		g.recordStart(ch, 15*time.Minute, at)
		g.recordDone(ch, "u1", at.Add(time.Second))
	}
	// 第 4 次（间隔足够）应被窗口上限拦下
	d := g.allow(ch, t0.Add(7*time.Minute))
	if d.OK {
		t.Fatal("超过窗口上限必须被拦")
	}
	if d.RetryAfter <= 0 {
		t.Fatalf("要给出等待时长: %+v", d)
	}
	// 窗口滑出后可再放行
	if d := g.allow(ch, t0.Add(61*time.Minute)); !d.OK {
		t.Fatalf("窗口滑出后应放行: %+v", d)
	}
}

// 连续"发起但没完成"达到阈值 → 冷却（**失败自停**：登录页一旦开始拒绝，继续重试只会加深风控）。
// 形态就是 2026-09-28 的事故：发起 → 登录页报错 → 没完成 → 再发起…
func TestLoginGuardFailureCooldown(t *testing.T) {
	g := testGuard()
	ch := guardChanTencCN
	t0 := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	ttl := 10 * time.Minute

	// 两轮完整形态：发起 → 没人完成 → 超时后被结算成失败
	for i := 0; i < 2; i++ {
		at := t0.Add(time.Duration(i) * 11 * time.Minute)
		if d := g.allow(ch, at); !d.OK {
			t.Fatalf("第 %d 轮应放行: %+v", i+1, d)
		}
		g.recordStart(ch, ttl, at)
	}
	// 第三轮：发起后超时未完成 → 累计第 3 次失败 → 冷却 20 分钟
	at := t0.Add(22 * time.Minute)
	if d := g.allow(ch, at); !d.OK {
		t.Fatalf("第 3 次失败结算前应放行: %+v", d)
	}
	g.recordStart(ch, ttl, at)

	d := g.allow(ch, t0.Add(33*time.Minute)) // 这里结算第 3 次失败 → 冷却
	if d.OK {
		t.Fatal("连续 3 次未完成后必须冷却（否则就是失败的无限重试）")
	}
	if d.RetryAfter < 19*time.Minute {
		t.Fatalf("冷却时长应接近配置值: %+v", d)
	}
	// 冷却中：再等多久都不放行（把"失败就再点"彻底掐死）
	if d := g.allow(ch, t0.Add(38*time.Minute)); d.OK {
		t.Fatal("冷却期内必须一律拦下")
	}
	// 冷却期满：放行一次（失败计数已清零，给"再试一次"的机会）
	if d := g.allow(ch, t0.Add(54*time.Minute)); !d.OK {
		t.Fatalf("冷却期满应放行: %+v", d)
	}
}

// 上一次授权还没结束就再发起 → 直接拦下（并说明还剩多久超时）。
// 没有这条，失败结算会被"覆盖 open"吞掉，冷却永远不会触发。
func TestLoginGuardBlocksWhilePreviousOpen(t *testing.T) {
	g := testGuard()
	ch := guardChanTencGL
	t0 := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	if d := g.allow(ch, t0); !d.OK {
		t.Fatalf("首次应放行: %+v", d)
	}
	g.recordStart(ch, 10*time.Minute, t0)

	// 过了最小间隔，但上一次仍未完成 → 仍要拦（事故里用户就是这样连点 4 次的）
	d := g.allow(ch, t0.Add(2*time.Minute))
	if d.OK {
		t.Fatal("上一次未完成时不得再发起")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > 10*time.Minute {
		t.Fatalf("要给出剩余超时时间: %+v", d)
	}
	// 超时后：结算成一次失败并放行（连续失败计数开始累积）
	if d := g.allow(ch, t0.Add(11*time.Minute)); !d.OK {
		t.Fatalf("超时后应放行: %+v", d)
	}
}

// 明确失败（换码失败等）立即计入，不必等 TTL。
func TestLoginGuardRecordFailedImmediate(t *testing.T) {
	g := testGuard()
	ch := guardChanHuawei
	t0 := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		at := t0.Add(time.Duration(i) * 5 * time.Minute)
		if d := g.allow(ch, at); !d.OK {
			t.Fatalf("第 %d 次应放行: %+v", i+1, d)
		}
		g.recordStart(ch, 15*time.Minute, at)
		g.recordFailed(ch, "换取凭证失败", at.Add(2*time.Second))
	}
	if d := g.allow(ch, t0.Add(16*time.Minute)); d.OK {
		t.Fatal("3 次明确失败后必须冷却")
	}
}

// 成功登录会把连续失败计数清零：偶发失败不该攒成冷却。
func TestLoginGuardSuccessResetsStreak(t *testing.T) {
	g := testGuard()
	ch := guardChanHuawei
	t0 := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		at := t0.Add(time.Duration(i) * 5 * time.Minute)
		g.allow(ch, at)
		g.recordStart(ch, 15*time.Minute, at)
		g.recordFailed(ch, "瞬时失败", at)
	}
	// 第三次成功
	at := t0.Add(10 * time.Minute)
	if d := g.allow(ch, at); !d.OK {
		t.Fatalf("应放行: %+v", d)
	}
	g.recordStart(ch, 15*time.Minute, at)
	g.recordDone(ch, "u1", at.Add(time.Second))

	// 之后两次失败也不该触发冷却（计数已清零）
	for i := 0; i < 2; i++ {
		at = at.Add(5 * time.Minute)
		if d := g.allow(ch, at); !d.OK {
			t.Fatalf("成功已清零计数，应继续放行: %+v", d)
		}
		g.recordStart(ch, 15*time.Minute, at)
		g.recordFailed(ch, "失败", at)
	}
	if d := g.allow(ch, at.Add(20*time.Minute)); !d.OK {
		t.Fatalf("只有连续 3 次才冷却: %+v", d)
	}
}

// 渠道互相独立：腾讯国际被限不该影响华为或国内（各区站点不同、风控面也不同）。
func TestLoginGuardChannelsIndependent(t *testing.T) {
	g := testGuard()
	t0 := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	g.recordStart(guardChanTencGL, 10*time.Minute, t0)
	g.recordDone(guardChanTencGL, "u1", t0)
	if d := g.allow(guardChanTencGL, t0.Add(time.Second)); d.OK {
		t.Fatal("同渠道连点应被拦")
	}
	if d := g.allow(guardChanTencCN, t0.Add(time.Second)); !d.OK {
		t.Fatalf("另一渠道不该受影响: %+v", d)
	}
	if d := g.allow(guardChanHuawei, t0.Add(time.Second)); !d.OK {
		t.Fatalf("华为渠道不该受影响: %+v", d)
	}
}

// 面板提示用：最近一次成功登录的 uid 与时间要对得上。
func TestLoginGuardRecentLogin(t *testing.T) {
	g := testGuard()
	ch := guardChanTencCN
	t0 := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	if _, _, ok := g.recentLogin(ch, t0); ok {
		t.Fatal("还没登过不该有 recent")
	}
	g.recordStart(ch, 10*time.Minute, t0)
	g.recordDone(ch, "abc-123", t0)
	uid, ago, ok := g.recentLogin(ch, t0.Add(3*time.Minute))
	if !ok || uid != "abc-123" || ago != 3*time.Minute {
		t.Fatalf("recent=%q ago=%s ok=%v", uid, ago, ok)
	}
}

// humanDur 是面板倒计时文案的一部分，别写出 "0s" 之外的怪东西。
func TestHumanDur(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "0s",
		45 * time.Second: "45s",
		90 * time.Second: "1m30s",
		2 * time.Minute:  "2m",
		61 * time.Minute: "1h1m",
		-5 * time.Second: "0s",
	}
	for in, want := range cases {
		if got := humanDur(in); got != want {
			t.Fatalf("humanDur(%s)=%q want %q", in, got, want)
		}
	}
}

// 端点契约：被闸门拒绝时返回 ok=false + throttled + retry_after_seconds（面板据此锁按钮倒计时）。
// 注意这里用的是**真实默认档**——buildTestServer 默认装的是宽松档（测试要连登）。
func TestOAuthStartThrottledByGuard(t *testing.T) {
	up := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	srv, _, _, h := buildTestServer(t, up.URL, nil)
	h.cfg.AuthDir = t.TempDir()
	h.loginGuard = newLoginGuard(loginGuardConfig{
		MinInterval: time.Minute, Window: time.Hour, MaxPerWindow: 10,
		FailureThreshold: 3, FailureCooldown: time.Minute,
	})
	post := func() map[string]any {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/admin/api/oauth/start", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	first := post()
	if first["ok"] != true {
		t.Fatalf("第一次应放行: %v", first)
	}
	second := post()
	if second["throttled"] != true || second["ok"] != false {
		t.Fatalf("连点必须被拦并标记 throttled: %v", second)
	}
	ra, _ := second["retry_after_seconds"].(float64)
	if ra <= 0 {
		t.Fatalf("必须给出 retry_after_seconds（面板锁按钮倒计时用）: %v", second)
	}
	if msg, _ := second["message"].(string); msg == "" {
		t.Fatalf("必须给出可读原因: %v", second)
	}
}
