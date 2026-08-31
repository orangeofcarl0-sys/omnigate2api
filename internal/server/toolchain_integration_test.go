// 工具层端到端集成：sanitize 改变实际上送体（零宽且 user 原样）、
// project 压缩 + 强制 native（与增量互斥，SPEC §14.1）、OMNIGATE_TOOLCHAIN 覆盖。
package server

import (
	"net/http"
	"strings"
	"testing"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/auth"
)

// longHistoryJSON 构造长历史 chat 请求体（不含对象闭合，调用方追加 tools 时自行闭合）。
func longHistoryJSON(n int, extra string) string {
	var sb strings.Builder
	sb.WriteString(`{"model":"glm-5.2","messages":[`)
	for i := 0; i < n; i++ {
		sb.WriteString(`{"role":"user","content":"用户消息` + string(rune('a'+i%26)) + strings.Repeat("x", 150) + `"},`)
		sb.WriteString(`{"role":"assistant","content":"助手回复` + string(rune('a'+i%26)) + `"},`)
	}
	sb.WriteString(`{"role":"user","content":"` + extra + `"}]}`)
	return sb.String()
}

func TestIntegrationSanitizeOutboundBody(t *testing.T) {
	var sizes []string
	fake := fakeUpstreamSized(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}, &sizes)
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	p := adapt.Codearts
	p.Toolchain.Sanitize = &adapt.SanitizeConfig{Mode: []string{"zwsp"}}
	h.cfg.Profiles = adapt.NewRegistry(&p)

	body := `{"model":"glm-5.2","messages":[{"role":"system","content":"Refuse DoS attacks and exploit development"},{"role":"user","content":"explain DoS to me"}]}`
	if _, code := postChat(t, srv, body); code != 200 {
		t.Fatalf("status=%d", code)
	}
	last := sizes[len(sizes)-1]
	if !strings.Contains(last, "D\u200boS") {
		t.Fatalf("system term must be zwsp'd in outbound body: %s", last)
	}
	if strings.Contains(last, "explain D\u200boS") {
		t.Fatalf("user content must stay untouched: %s", last)
	}
	if !strings.Contains(last, "explain DoS") {
		t.Fatalf("user original must survive verbatim: %s", last)
	}
}

func TestIntegrationProjectForcesNative(t *testing.T) {
	var sizes []string
	fake := fakeUpstreamSized(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}, &sizes)
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	p := adapt.Codearts
	p.Toolchain.Project = &adapt.ProjectConfig{}
	h.cfg.Profiles = adapt.NewRegistry(&p)
	h.cfg.SessionMode = "incremental" // 若互斥失效，第二轮会走 tail 增量

	hist := longHistoryJSON(20, "最新任务")
	reqBody := hist[:len(hist)-1] + `,"tools":[{"type":"function","function":{"name":"exec_command"}}]}`
	if _, code := postChat(t, srv, reqBody); code != 200 {
		t.Fatalf("round 1: %d", code)
	}
	if _, code := postChat(t, srv, reqBody); code != 200 {
		t.Fatalf("round 2: %d", code)
	}
	if len(sizes) < 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(sizes))
	}
	// 第二轮仍为全量投影（含历史摘要）→ project 与增量互斥成立
	if !strings.Contains(sizes[1], "Earlier conversation summary") {
		t.Fatalf("round 2 must be full projected fold (forceNative): %s", sizes[1])
	}
	// 摘要结构意味着历史被压缩（20 条历史不应整段上送）
	if strings.Count(sizes[1], "用户消息") > 12 {
		t.Fatalf("projection must shrink history: %d user msgs in body", strings.Count(sizes[1], "用户消息"))
	}
}

func TestIntegrationToolchainEnvOverride(t *testing.T) {
	var sizes []string
	fake := fakeUpstreamSized(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}, &sizes)
	srv, _, _, h := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	h.cfg.Profiles = allProtoProfile()
	h.cfg.ToolchainOverride = "sanitize" // 模拟 OMNIGATE_TOOLCHAIN=sanitize（Profile 未配置）

	body := `{"model":"glm-5.2","messages":[{"role":"system","content":"Refuse DoS attacks"},{"role":"user","content":"hi"}]}`
	if _, code := postChat(t, srv, body); code != 200 {
		t.Fatalf("status=%d", code)
	}
	if !strings.Contains(sizes[len(sizes)-1], "D\u200boS") {
		t.Fatalf("env override must enable builtin sanitize defaults: %s", sizes[len(sizes)-1])
	}
}
