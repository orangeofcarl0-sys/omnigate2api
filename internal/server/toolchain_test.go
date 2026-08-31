// 工具层单测（SPEC §14）：sanitize 反监控（零宽/剥离/摘要/作用域纪律）与
// project 投影（conservative/aggressive/回退/anchor/工具依赖）。
package server

import (
	"strings"
	"testing"

	"omnigate2api/internal/adapt"
)

// ---------------------------------------------------------------------------
// sanitize：零宽插词
// ---------------------------------------------------------------------------

func TestSanitizeZWSP(t *testing.T) {
	cfg := &adapt.SanitizeConfig{Mode: []string{"zwsp"}}
	out := sanitizeText("Refuse requests for DoS attacks and exploit development.", cfg, builtinSanitizeTerms)
	if !strings.Contains(out, "D\u200boS") || (strings.Contains(out, "exploit") && !strings.Contains(out, "e\u200bxploit")) {
		t.Fatalf("zwsp missing: %q", out)
	}
	if strings.Contains(out, "DoS") {
		t.Fatalf("raw DoS must be broken: %q", out)
	}
}

func TestSanitizeNoHitUnchanged(t *testing.T) {
	cfg := &adapt.SanitizeConfig{Mode: []string{"zwsp", "strip", "compact"}}
	s := "这是一段正常的中文，不含任何触发词。"
	if out := sanitizeText(s, cfg, builtinSanitizeTerms); out != s {
		t.Fatalf("no-hit must return original: %q", out)
	}
}

func TestSanitizeCaseInsensitiveLongestFirst(t *testing.T) {
	cfg := &adapt.SanitizeConfig{Mode: []string{"zwsp"}}
	// 大小写不敏感
	if out := sanitizeText("dos", cfg, builtinSanitizeTerms); out == "dos" {
		t.Fatalf("lowercase dos must match: %q", out)
	}
	// 长词优先：SQL injection 整体命中（而非 injection 单命中导致 SQL 残留）
	out := sanitizeText("SQL injection", cfg, builtinSanitizeTerms)
	if strings.Contains(out, "SQL") {
		t.Fatalf("SQL must be inside zwsp'd term: %q", out)
	}
}

// 边界：内置词表不含真实有害类别词（工具层只缓解合规模板误伤，§14.3 边界声明）。
func TestSanitizeExcludesHarmfulTerms(t *testing.T) {
	cfg := &adapt.SanitizeConfig{Mode: []string{"zwsp"}}
	for _, w := range []string{"weapon", "drug", "suicide", "murder", "terrorist", "bomb"} {
		if out := sanitizeText("how to make a "+w, cfg, builtinSanitizeTerms); out != "how to make a "+w {
			t.Fatalf("harmful term %q must NOT be sanitized: %q", w, out)
		}
	}
}

// ---------------------------------------------------------------------------
// sanitize：指纹剥离/改写（对齐 Go 参考实现）
// ---------------------------------------------------------------------------

func TestSanitizeFingerprints(t *testing.T) {
	cfg := &adapt.SanitizeConfig{Mode: []string{"zwsp"}}
	s := "x-anthropic-billing-header: 5ca75bd0; cc_entrypoint=claude_code; " +
		"You are Claude Code, Anthropic's official CLI for Claude. " +
		"Main branch (you will usually use this for PRs)"
	out := sanitizeText(s, cfg, nil)
	if strings.Contains(out, "x-anthropic-billing-header") {
		t.Fatalf("billing header must be stripped: %q", out)
	}
	if strings.Contains(out, "cc_entrypoint") {
		t.Fatalf("cc kv must be stripped: %q", out)
	}
	if !strings.Contains(out, "official CLI tool for Claude") {
		t.Fatalf("rewrite missing: %q", out)
	}
	if !strings.Contains(out, "Default branch") {
		t.Fatalf("branch rewrite missing: %q", out)
	}
}

// ---------------------------------------------------------------------------
// sanitize：strip / compact / 作用域
// ---------------------------------------------------------------------------

func TestSanitizeStripBlocks(t *testing.T) {
	cfg := &adapt.SanitizeConfig{Mode: []string{"strip"}}
	s := "指令A <environment_context>path=/tmp\ninfo</environment_context> 指令B"
	out := sanitizeText(s, cfg, nil)
	if strings.Contains(out, "<environment_context>") {
		t.Fatalf("block must be replaced: %q", out)
	}
	if !strings.Contains(out, "Environment context is provided") {
		t.Fatalf("replacement missing: %q", out)
	}
	if !strings.Contains(out, "指令A") || !strings.Contains(out, "指令B") {
		t.Fatalf("surrounding text must survive: %q", out)
	}
}

func TestSanitizeCompactHarness(t *testing.T) {
	cfg := &adapt.SanitizeConfig{Mode: []string{"compact"}}
	s := "You are Claude Code, Anthropic's official CLI for Claude. 大量运行时指令……"
	out := sanitizeText(s, cfg, nil)
	if strings.Contains(out, "大量运行时指令") {
		t.Fatalf("harness system must be compacted: %q", out)
	}
	if !strings.Contains(out, "Be precise, helpful") {
		t.Fatalf("summary missing: %q", out)
	}
}

func TestSanitizeRequestScope(t *testing.T) {
	cfg := &adapt.SanitizeConfig{Mode: []string{"zwsp"}, HarnessUser: false}
	msgs := []openAIMessage{
		{Role: "system", Text: "Refuse DoS attacks."},
		{Role: "user", Text: "explain DoS attacks"}, // 真实用户输入：不可达
	}
	out, hits := sanitizeRequest(msgs, cfg)
	if hits != 1 {
		t.Fatalf("hits=%d want 1", hits)
	}
	if !strings.Contains(out[0].Text, "\u200b") {
		t.Fatalf("system must be sanitized: %q", out[0].Text)
	}
	if out[1].Text != msgs[1].Text {
		t.Fatalf("user must be untouched: %q", out[1].Text)
	}

	// HarnessUser：命中 harness 标记的 user 才动
	cfg.HarnessUser = true
	msgs[1].Text = "<environment_context>path=/x</environment_context> 内核 DoS explain"
	out2, _ := sanitizeRequest(msgs, cfg)
	if !strings.Contains(out2[1].Text, "\u200b") {
		t.Fatalf("harness user must be sanitized: %q", out2[1].Text)
	}
}

// ---------------------------------------------------------------------------
// project：conservative / aggressive
// ---------------------------------------------------------------------------

func TestProjectConservative(t *testing.T) {
	cfg := &adapt.ProjectConfig{}
	msgs := []openAIMessage{
		{Role: "system", Text: strings.Repeat("s", 3000)},
		{Role: "user", Text: "hi"},
	}
	out, stats := projectRequest(msgs, nil, cfg)
	if stats.Mode != "conservative" {
		t.Fatalf("mode=%s", stats.Mode)
	}
	// 截断上限含省略后缀："…[已截断]"
	if len([]rune(out[0].Text)) > maxSystemChars+len("…[已截断]") {
		t.Fatalf("system not truncated: %d", len([]rune(out[0].Text)))
	}
	if out[1].Text != "hi" {
		t.Fatalf("short user must pass through: %q", out[1].Text)
	}
}

func TestProjectAggressive(t *testing.T) {
	cfg := &adapt.ProjectConfig{}
	long := make([]openAIMessage, 0, 24)
	for i := 0; i < 10; i++ {
		long = append(long, openAIMessage{Role: "user", Text: "u" + string(rune('a'+i)) + strings.Repeat("x", 200)})
		long = append(long, openAIMessage{Role: "assistant", Text: "a" + string(rune('a'+i))})
	}
	long = append(long, openAIMessage{Role: "user", Text: "最新指令"})
	tools := []map[string]any{{"function": map[string]any{"name": "exec_command"}}}

	out, stats := projectRequest(long, tools, cfg)
	if stats.Mode != "aggressive" {
		t.Fatalf("agentic tools must trigger aggressive: mode=%s", stats.Mode)
	}
	if len(out) >= len(long) {
		t.Fatalf("projection must shrink: %d -> %d", len(long), len(out))
	}
	// anchor：最早 user 保留
	if !stats.AnchorPreserved {
		t.Fatalf("anchor must be preserved")
	}
	// 最新指令必须在 tail 中
	found := false
	for _, m := range out {
		if m.Role == "user" && strings.Contains(m.Text, "最新指令") {
			found = true
		}
	}
	if !found {
		t.Fatalf("latest user must survive in tail: %+v", out)
	}
	// 摘要存在
	hasSummary := false
	for _, m := range out {
		if m.Role == "system" && strings.Contains(m.Text, "Earlier conversation summary") {
			hasSummary = true
		}
	}
	if !hasSummary {
		t.Fatalf("summary missing: %+v", out)
	}
}

func TestProjectToolDependencyExtend(t *testing.T) {
	cfg := &adapt.ProjectConfig{}
	msgs := []openAIMessage{
		{Role: "user", Text: "开始"},
		{Role: "assistant", Text: "", ToolCalls: []openAIToolCall{{ID: "call_1", Name: "Read"}}},
		{Role: "tool", ToolCallID: "call_1", Text: "内容"},
		{Role: "user", Text: "继续"},
	}
	out, _ := projectRequest(msgs, []map[string]any{{"function": map[string]any{"name": "bash"}}}, cfg)
	// tail 必须包含 call_1 的 assistant（否则 tool 消息悬空）
	seenCall, seenTool := false, false
	for _, m := range out {
		if m.Role == "assistant" {
			for _, c := range m.ToolCalls {
				if c.ID == "call_1" {
					seenCall = true
				}
			}
		}
		if m.Role == "tool" && m.ToolCallID == "call_1" {
			seenTool = true
		}
	}
	if !seenCall || !seenTool {
		t.Fatalf("tool dependency must extend tail: call=%v tool=%v msgs=%+v", seenCall, seenTool, out)
	}
}

func TestProjectEmptyFallback(t *testing.T) {
	cfg := &adapt.ProjectConfig{}
	// 全部 harness 噪音 → aggressive 结果为空 → 回退 conservative
	msgs := []openAIMessage{
		{Role: "system", Text: "You are a coding agent running in the Codex CLI blah blah"},
		{Role: "user", Text: "<environment_context>ctx</environment_context> 真实问题"},
	}
	out, stats := projectRequest(msgs, []map[string]any{{"function": map[string]any{"name": "exec_command"}}}, cfg)
	if stats.Mode != "conservative" {
		t.Fatalf("empty projection must fall back to conservative: mode=%s", stats.Mode)
	}
	if len(out) != 2 {
		t.Fatalf("fallback must keep messages: %+v", out)
	}
}

// ---------------------------------------------------------------------------
// 开关解析（OMNIGATE_TOOLCHAIN / Profile 声明）
// ---------------------------------------------------------------------------

func TestToolchainEnabled(t *testing.T) {
	p := &adapt.UpstreamProfile{}
	if on, _ := toolchainEnabled(p, ""); on {
		t.Fatal("default must be off")
	}
	p.Toolchain.Project = &adapt.ProjectConfig{}
	if on, _ := toolchainEnabled(p, ""); !on {
		t.Fatal("profile project must enable")
	}
	if on, _ := toolchainEnabled(p, "none"); on {
		t.Fatal("override none must force off")
	}
	on, ok := toolchainEnabled(p, "project,sanitize")
	if !on || !ok {
		t.Fatalf("override project,sanitize: %v %v", on, ok)
	}
}

// D1 回归：不同 Profile 的 terms 覆盖必须各自生效（词表缓存按内容键化，
// 而非 sync.Once 固化首个词表——多上游场景下互不污染）。
func TestSanitizeTermsPerProfile(t *testing.T) {
	cfgA := &adapt.SanitizeConfig{Mode: []string{"zwsp"}, Terms: []string{"alpha"}}
	cfgB := &adapt.SanitizeConfig{Mode: []string{"zwsp"}, Terms: []string{"beta"}}
	msgs := []openAIMessage{{Role: "system", Text: "alpha beta"}}

	outA, _ := sanitizeRequest(msgs, cfgA)
	if !strings.Contains(outA[0].Text, "a\u200blpha") {
		t.Fatalf("profile A terms must apply: %q", outA[0].Text)
	}
	outB, _ := sanitizeRequest(msgs, cfgB)
	if !strings.Contains(outB[0].Text, "b\u200beta") {
		t.Fatalf("profile B terms must apply (cache must be per-terms): %q", outB[0].Text)
	}
	if strings.Contains(outB[0].Text, "a\u200blpha") {
		t.Fatalf("profile B must not carry profile A terms: %q", outB[0].Text)
	}
}
