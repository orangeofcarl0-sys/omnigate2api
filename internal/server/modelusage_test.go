// (账号, 模型) 窗口用量观测（SPEC §28.5）——模型限流阈值的唯一可归因数据源。
package server

import (
	"testing"
	"time"
)

func TestModelUsageWindowAndHitSnapshot(t *testing.T) {
	s := newModelUsageStats()
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
	s := newModelUsageStats()
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
	s := newModelUsageStats()
	s.NoteRequest("", "m", 1, 1, time.Now())
	s.NoteRequest("a", "", 1, 1, time.Now())
	if got := len(s.Snapshot()); got != 0 {
		t.Fatalf("空键不该建行，实际 %d 行", got)
	}
	if _, _, _ = s.NoteLimit("", "m", time.Time{}, time.Now()); len(s.Snapshot()) != 0 {
		t.Fatal("空键的撞限也不该建行")
	}
}
