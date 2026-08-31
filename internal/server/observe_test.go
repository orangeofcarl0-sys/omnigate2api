package server

import "testing"

func TestNoContextEcho(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"目前这段对话里没有可供总结的先前上下文", true},
		{"这是本轮会话的第一条用户消息", true},
		{"There's no explicit user question in the current turn", true},
		{"已成功读取并验证文件内容", false},
	}
	for _, c := range cases {
		if got := noContextEcho(c.in); got != c.want {
			t.Fatalf("noContextEcho(%q)=%v want %v", c.in, got, c.want)
		}
	}
}
