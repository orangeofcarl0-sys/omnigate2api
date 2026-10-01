// (账号, 模型) 窗口用量观测（SPEC §28.5）——模型限流阈值的唯一可归因数据源。
package server

import (
	"testing"
	"time"
)

func TestModelUsageWindowAndHitSnapshot(t *testing.T) {
	s := newModelUsageStats("", 0)
	t0 := time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)

	// 三次成功请求（窗口起点 = 第一次）
	s.NoteRequest("acct-A", "deepseek-v4.1-flash", 100, 50, t0)
	s.NoteRequest("acct-A", "deepseek-v4.1-flash", 200, 80, t0.Add(time.Minute))
	s.NoteRequest("acct-A", "deepseek-v4.1-flash", 0, 0, t0.Add(2*time.Minute))

	// 撞限：快照 = 阈值观测值（3 次 / 430 tokens / 窗口起点 t0）
	reset := t0.Add(24 * time.Hour)
	reqs, toks, since := s.NoteLimit("acct-A", "deepseek-v4.1-flash", reset, t0.Add(3*time.Minute))
	if reqs != 3 || toks != 430 {
		t.Fatalf("撞限快照应为 3 次/430 tokens，实际 %d/%d", reqs, toks)
	}
	if !since.Equal(t0) {
		t.Fatalf("窗口起点应为首次请求时刻，实际 %s", since)
	}

	// 另一个模型/账号互不干扰（限流是按 (账号, 模型) 的）
	s.NoteRequest("acct-A", "hy3", 7, 3, t0.Add(4*time.Minute))
	s.NoteRequest("acct-B", "deepseek-v4.1-flash", 9, 1, t0.Add(5*time.Minute))
	a := s.ForAccount("acct-A")
	if a["hy3"].Requests != 1 || a["deepseek-v4.1-flash"].Requests != 3 {
		t.Fatalf("按账号归因不对: %+v", a)
	}
	if a["hy3"].Hits != 0 || a["deepseek-v4.1-flash"].Hits != 1 {
		t.Fatalf("撞限次数应只记在被限的那个 (账号,模型): %+v", a)
	}
	if len(s.ForAccount("acct-B")) != 1 {
		t.Fatalf("另一账号应有自己的窗口: %+v", s.ForAccount("acct-B"))
	}

	// 上游声明的 reset 已过 → 下一个请求开新窗口（计数归零），但**快照保留**（那是观测值）
	s.NoteRequest("acct-A", "deepseek-v4.1-flash", 10, 5, reset.Add(time.Minute))
	row := s.ForAccount("acct-A")["deepseek-v4.1-flash"]
	if row.Requests != 1 || row.Tokens != 15 {
		t.Fatalf("reset 后应开新窗口: %+v", row)
	}
	if row.HitRequests != 3 || row.HitTokens != 430 || row.Hits != 1 {
		t.Fatalf("撞限快照必须保留（阈值观测值）: %+v", row)
	}
}

// 同一窗口内重复撞限：快照刷新为最新一次，hits 累加（用于判断是硬阈值还是渐变）。
func TestModelUsageRepeatedHits(t *testing.T) {
	s := newModelUsageStats("", 0)
	t0 := time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		s.NoteRequest("a", "m", 10, 0, t0.Add(time.Duration(i)*time.Second))
	}
	reset := t0.Add(24 * time.Hour)
	s.NoteLimit("a", "m", reset, t0.Add(10*time.Second))
	s.NoteLimit("a", "m", reset, t0.Add(20*time.Second)) // 同一 reset：客户端仍在重试
	row := s.ForAccount("a")["m"]
	if row.Hits != 2 || row.HitRequests != 5 {
		t.Fatalf("重复撞限应累加 hits 且快照为最新窗口计数: %+v", row)
	}
	if row.HitResetAt == "" {
		t.Fatal("应记录上游声明的重置时刻")
	}
}

// 空账号/空模型不得panic也不得建行（调用方可能没有账号上下文）。
func TestModelUsageIgnoresEmptyKeys(t *testing.T) {
	s := newModelUsageStats("", 0)
	s.NoteRequest("", "m", 1, 1, time.Now())
	s.NoteRequest("a", "", 1, 1, time.Now())
	if got := len(s.Snapshot()); got != 0 {
		t.Fatalf("空键不该建行，实际 %d 行", got)
	}
	if _, _, _ = s.NoteLimit("", "m", time.Time{}, time.Now()); len(s.Snapshot()) != 0 {
		t.Fatal("空键的撞限也不该建行")
	}
}

// 阈值对照口径：撞过限的 pair 用**实证值**（撞限时那一瞬的窗口计数），
// 没撞过的才回落到配置的**假设值**——两者不能混为一谈。
func TestModelUsageCapSource(t *testing.T) {
	const assumed = int64(200_000_000)
	s := newModelUsageStats("", assumed)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	s.NoteRequest("A", "dsv41f", 50_000_000, 0, t0)
	row := s.ForAccount("A")["dsv41f"]
	if row.CapSource != "assumed" || row.CapTokens != assumed {
		t.Fatalf("未撞限应回落到假设上限: %+v", row)
	}
	if row.UsedPct < 24.9 || row.UsedPct > 25.1 {
		t.Fatalf("5千万/2亿 应为 25%%，实际 %.2f", row.UsedPct)
	}
	if row.RemainingTokens != assumed-50_000_000 {
		t.Fatalf("剩余应可算出: %+v", row)
	}

	// 撞限 → 实证值（此例实证上限就是撞限时的 5 千万）覆盖假设值
	s.NoteLimit("A", "dsv41f", t0.Add(24*time.Hour), t0.Add(time.Minute))
	row = s.ForAccount("A")["dsv41f"]
	if row.CapSource != "observed" || row.CapTokens != 50_000_000 {
		t.Fatalf("撞限后应以实证值为上限: %+v", row)
	}
	if row.UsedPct < 99.9 || row.UsedPct > 100.1 {
		t.Fatalf("实证上限下用量占比应为 100%%，实际 %.2f", row.UsedPct)
	}
}

// 既没假设上限、也没撞过限 → 只报用量，不编造百分比（拿空气当分母比不显示更坏）。
func TestModelUsageNoCapNoPct(t *testing.T) {
	s := newModelUsageStats("", 0)
	s.NoteRequest("A", "m", 1_000_000, 0, time.Now())
	row := s.ForAccount("A")["m"]
	if row.CapTokens != 0 || row.UsedPct != 0 || row.CapSource != "" {
		t.Fatalf("无上限信息时不该编造百分比: %+v", row)
	}
	if row.Tokens != 1_000_000 {
		t.Fatalf("用量仍须照报: %+v", row)
	}
}

// 从未撞过限（拿不到上游 reset 时刻）时窗口按 horizon 兜底滚动——
// 否则"窗口用量"会退化成"历史总量"，百分比就不再是"这一窗口用了几成"。
func TestModelUsageRollsWithoutReset(t *testing.T) {
	s := newModelUsageStats("", 0)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.NoteRequest("A", "m", 100, 0, t0)
	s.NoteRequest("A", "m", 100, 0, t0.Add(time.Hour))
	if got := s.ForAccount("A")["m"].Tokens; got != 200 {
		t.Fatalf("同一窗口内应累加，实际 %d", got)
	}
	s.NoteRequest("A", "m", 7, 0, t0.Add(modelRateLimitHorizon+time.Minute))
	row := s.ForAccount("A")["m"]
	if row.Tokens != 7 || row.Requests != 1 {
		t.Fatalf("超 horizon 应开新窗口，实际 tokens=%d requests=%d", row.Tokens, row.Requests)
	}
}

// 落盘往返：窗口计数与撞限快照都要活过进程重启。
// 不落盘的话窗口每次重启清零，"这个号用了几成"只剩重启后那一小段（实测差三个数量级）。
func TestModelUsagePersistsAcrossRestart(t *testing.T) {
	file := t.TempDir() + "/model_usage.json"
	t0 := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)

	s1 := newModelUsageStats(file, 200_000_000)
	s1.NoteRequest("A", "dsv41f", 1_500_000, 500_000, t0)
	s1.NoteLimit("A", "dsv41f", t0.Add(24*time.Hour), t0.Add(time.Minute))
	s1.Flush()

	s2 := newModelUsageStats(file, 200_000_000)
	row := s2.ForAccount("A")["dsv41f"]
	if row.Tokens != 2_000_000 || row.Requests != 1 {
		t.Fatalf("窗口未持久化: %+v", row)
	}
	if row.Hits != 1 || row.HitTokens != 2_000_000 {
		t.Fatalf("撞限快照未持久化（阈值证据不能丢）: %+v", row)
	}
	if row.HitResetAt == "" {
		t.Fatalf("上游 reset 时刻未持久化: %+v", row)
	}
	// 另一个账号/模型不受影响
	if got := len(s2.ForAccount("B")); got != 0 {
		t.Fatalf("不该凭空多出账号: %d", got)
	}
}
