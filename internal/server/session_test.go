package server

import "testing"

func TestFingerprintMatch(t *testing.T) {
	idx := newSessionIndex(5)
	mk := func(roles ...string) []openAIMessage {
		var out []openAIMessage
		for i, r := range roles {
			out = append(out, openAIMessage{Role: r, Text: "msg" + string(rune('a'+i))})
		}
		return out
	}
	turn1 := mk("system", "user")
	mk1 := fingerprint(turn1)
	turn2 := append(append([]openAIMessage{}, turn1...), openAIMessage{Role: "assistant", Text: "ok"})
	mk2 := fingerprint(turn2)
	idx.register(mk1[len(mk1)-1], len(turn1), "acct", "chat1")

	ref, tailStart, key, reason, ok := idx.match(turn2, mk2)
	if !ok || reason != MatchOK || ref.chatID != "chat1" || tailStart != 2 || key != mk1[len(mk1)-1] {
		t.Fatalf("match failed: ok=%v reason=%v ref=%v tailStart=%d", ok, reason, ref, tailStart)
	}

	// 同长度重复（重试）不续接
	if _, _, _, _, ok := idx.match(turn1, mk1); ok {
		t.Fatalf("retry must not match")
	}
	// 完全不同前缀不匹配
	turn3 := mk("user", "user")
	if _, _, _, _, ok := idx.match(turn3, fingerprint(turn3)); ok {
		t.Fatalf("unrelated must not match")
	}
}

func TestFingerprintTailsLimit(t *testing.T) {
	idx := newSessionIndex(5)
	turn1 := []openAIMessage{{Role: "user", Text: "a"}, {Role: "assistant", Text: "b"}}
	mk1 := fingerprint(turn1)
	idx.register(mk1[len(mk1)-1], 2, "acct", "c1")
	for i := 0; i < fpMaxTails; i++ {
		turn1 = append(turn1, openAIMessage{Role: "user", Text: "more"})
		ch := fingerprint(turn1)
		if _, _, mkey, _, ok := idx.match(turn1, ch); !ok {
			t.Fatalf("iteration %d should match", i)
		} else {
			idx.register(ch[len(ch)-1], len(turn1), "acct", "c1")
			_ = mkey
		}
	}
	// 超过连续增量上限 → 拒绝续接（强制全量重锚定）
	final := append(turn1, openAIMessage{Role: "user", Text: "last"})
	if _, _, _, _, ok := idx.match(final, fingerprint(final)); ok {
		t.Fatalf("tails limit must force re-anchor")
	}
}

// 工具循环 tail 无 user 时，起点必须回退到最近 user（否则模型"无指令可依"反问）。
func TestTailAnchorIndex(t *testing.T) {
	u := openAIMessage{Role: "user", Text: "改文件"}
	a := openAIMessage{Role: "assistant", Text: "读"}
	tool := openAIMessage{Role: "tool", Text: "alpha = 1"}
	msgs := []openAIMessage{u, a}
	if got := tailAnchorIndex(msgs, 2); got != 0 {
		t.Fatalf("matches beyond tail should anchor to user: %d", got)
	}
	msgs2 := []openAIMessage{u, a, tool}
	if got := tailAnchorIndex(msgs2, 1); got != 0 {
		t.Fatalf("tool-loop tail must anchor to user: %d", got)
	}
	msgs3 := []openAIMessage{u, a, tool, {Role: "user", Text: "继续"}, {Role: "assistant", Text: "a2"}}
	_ = msgs3[3]
	if got := tailAnchorIndex(msgs3, 3); got != 3 {
		t.Fatalf("latest user anchor expected at 3: %d", got)
	}
}

// 增量不做上下文裁剪：起点只锚定最近 user 指令,不因窗口而后退到历史。
func TestTailNoHardWindow(t *testing.T) {
	var msgs []openAIMessage
	for i := 0; i < 30; i++ {
		msgs = append(msgs, openAIMessage{Role: "user", Text: "u"}, openAIMessage{Role: "assistant", Text: "a"})
	}
	// k=58 匹配在历史中部;tail 起点由锚点决定:最近 user 恰在 k-1(k 为偶数索引,
	// user 位于偶下标) → 锚点=k(不后退窗口)
	if got := tailAnchorIndex(msgs, 58); got != 58 {
		t.Fatalf("expected anchor at 58, got %d", got)
	}
}

// 跨会话同历史(同哈希)身份翻转：不同 chatID 注册同一 key 后,
// 两个会话的续接都必须被拒绝(回退全量)——否则后注册会话会吞掉
// 先到会话的续接(上下文串线)。
func TestFingerprintContended(t *testing.T) {
	idx := newSessionIndex(5)
	turn := []openAIMessage{{Role: "user", Text: "同任务"}}
	ch := fingerprint(turn)
	idx.register(ch[len(ch)-1], 1, "acct-a", "chat-a")
	// 会话 B(不同账号/chatID)重新注册同一历史 → 身份翻转
	idx.register(ch[len(ch)-1], 1, "acct-b", "chat-b")
	next := append(turn, openAIMessage{Role: "assistant", Text: "reply"})
	if _, _, _, _, ok := idx.match(next, fingerprint(next)); ok {
		t.Fatalf("contended key must refuse continuation")
	}
	// 会话 A 侧的续接同样被拒(单向安全)
	if _, _, _, _, ok := idx.match(append([]openAIMessage{}, turn...), ch); ok {
		t.Fatalf("original session must also fall back")
	}
}
