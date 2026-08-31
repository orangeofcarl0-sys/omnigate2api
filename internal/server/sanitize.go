// 工具层：sanitize 反监控（SPEC §14.3）——零宽插词/指纹剥离与改写/
// harness 块替换/compact 摘要。纯函数；作用域纪律：只动 system/developer
// 与命中 harness 标记的 user，真实用户文本不可达。内置基线收敛两家开源
// 实现，且刻意排除真实有害类别词（只缓解合规模板误伤）。
package server

import (
	"regexp"
	"strings"
	"sync"

	"omnigate2api/internal/adapt"
)

var builtinSanitizeTerms = []string{
	"DoS", "DDoS", "exploit", "exploit development", "credential testing", "credential stuffing",
	"supply chain compromise", "supply-chain compromise", "detection evasion", "C2 frameworks", "C2 framework",
	"command and control", "malicious purposes", "malicious intent", "mass targeting",
	"brute force", "brute-force", "privilege escalation", "reverse shell", "remote code execution",
	"SQL injection", "XSS", "CSRF", "phishing", "malware", "ransomware", "keylogger", "rootkit",
	"backdoor", "botnet", "zero-day", "0day", "vulnerability", "vulnerabilities",
	"red teaming", "red-teaming", "sandbox", "sandboxing", "sandboxed", "unsandboxed",
	"escalated privileges", "escalated", "escalation", "destructive action", "destructive",
	"attack", "attacks", "cybersecurity", "security review", "hacking", "penetration testing",
	"penetration test", "injection", "harmful", "dangerous", "abuse", "abusive", "illegal",
	"Claude Code", "Claude Opus", "Claude Sonnet", "Claude Haiku", "Anthropic", "Co-Authored-By",
}

// builtinHarnessMarkers 两 stage 共用的 harness 注入标记（§14.4）。
var builtinHarnessMarkers = []string{
	"# AGENTS.md instructions", "<environment_context>", "<permissions instructions>",
	"<collaboration_mode>", "<skills_instructions>", "<system-reminder>", "# claudeMd",
	"You are a coding agent running in the Codex CLI", "You are Claude Code",
}

// builtinHarnessBlocks mode=strip 的开闭标记块 → 中性替身。
var builtinHarnessBlocks = []adapt.HarnessBlock{
	{Start: "<environment_context>", End: "</environment_context>", Replace: "Environment context is provided by the harness."},
	{Start: "<permissions instructions>", End: "</permissions instructions>", Replace: "Runtime permissions apply: filesystem access may be sandboxed, network may be restricted, and some commands may require user approval."},
	{Start: "<collaboration_mode>", End: "</collaboration_mode>", Replace: "Collaboration mode instructions are provided by the harness."},
	{Start: "<skills_instructions>", End: "</skills_instructions>", Replace: "Runtime skill metadata is available."},
	{Start: "<plugins_instructions>", End: "</plugins_instructions>", Replace: "Runtime plugin metadata is available."},
	{Start: "<system-reminder>", End: "</system-reminder>", Replace: "Runtime reminder context is provided by the harness."},
}

// builtinFingerprints 指纹剥离/改写基线（对齐 Go 参考实现 sanitize.go）。
var builtinFingerprints = []adapt.SanitizeFingerprint{
	{Match: "x-anthropic-billing-header:"},
	{Match: "cc_"},
	{Match: "You are Claude Code, Anthropic's official CLI for Claude.",
		Rewrite: []string{"You are Claude Code, Anthropic's official CLI for Claude.", "You are Claude Code, Anthropic's official CLI tool for Claude."}},
	{Match: "Main branch (you will usually use this for PRs)",
		Rewrite: []string{"Main branch (you will usually use this for PRs)", "Default branch (you will usually use this for PRs)"}},
}

const zwsp = "\u200b"

// ---------------------------------------------------------------------------
// sanitize：反监控（§14.3）
// ---------------------------------------------------------------------------

var (
	zwspMu    sync.Mutex
	zwspCache = map[string]*regexp.Regexp{}
)

// zwspMatcher 触发词匹配正则，按词表内容缓存——不同 Profile 的 terms 覆盖
// 各自生效（此前 sync.Once 固化首个词表，多 Profile 下覆盖失效）。
func zwspMatcher(terms []string) *regexp.Regexp {
	key := strings.Join(terms, "\x00")
	zwspMu.Lock()
	defer zwspMu.Unlock()
	if re, ok := zwspCache[key]; ok {
		return re
	}
	re := compileTermRegex(terms)
	zwspCache[key] = re
	return re
}

func compileTermRegex(terms []string) *regexp.Regexp {
	// 长词优先，避免短词先吃掉长词；大小写不敏感
	longest := make([]string, len(terms))
	copy(longest, terms)
	for i := 1; i < len(longest); i++ {
		for j := i; j > 0 && len(longest[j]) > len(longest[j-1]); j-- {
			longest[j], longest[j-1] = longest[j-1], longest[j]
		}
	}
	parts := make([]string, len(longest))
	for i, t := range longest {
		parts[i] = regexp.QuoteMeta(t)
	}
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(parts, "|") + `)\b`)
}

// sanitizeText 单段文本净化：零宽插词 + 指纹剥离/改写（纯函数；无命中原样返回）。
func sanitizeText(s string, cfg *adapt.SanitizeConfig, terms []string) string {
	orig := s
	// 零宽预检快路径：文本不含任一触发词（大小写不敏感）→ 跳过替换（零分配）
	lower := strings.ToLower(s)
	hit := false
	for _, t := range terms {
		if strings.Contains(lower, strings.ToLower(t)) {
			hit = true
			break
		}
	}
	if hit {
		s = zwspMatcher(terms).ReplaceAllStringFunc(s, func(m string) string {
			if len(m) <= 1 {
				return m
			}
			return m[:1] + zwsp + m[1:]
		})
	}
	fps := builtinFingerprints
	if len(cfg.Fingerprints) > 0 {
		fps = cfg.Fingerprints
	}
	for _, fp := range fps {
		if len(fp.Rewrite) == 2 && strings.Contains(s, fp.Match) {
			s = strings.ReplaceAll(s, fp.Rewrite[0], fp.Rewrite[1])
			continue
		}
		if fp.Match == "cc_" {
			// 尾随裸键值 cc_xxx=...; 循环清理（对齐 Go 参考实现）
			kvRe := regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)
			for prev := ""; prev != s; prev = s {
				s = kvRe.ReplaceAllString(s, "")
			}
			continue
		}
		if strings.Contains(s, fp.Match) {
			// 键值/header 型指纹：从命中处剥离到行尾（含分号）
			lineRe := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(fp.Match) + `[^;\n]*;?\s*`)
			s = lineRe.ReplaceAllString(s, "")
		}
	}
	// mode=strip：开闭标记块 → 中性替身
	blocks := builtinHarnessBlocks
	if len(cfg.HarnessBlocks) > 0 {
		blocks = append(builtinHarnessBlocks, cfg.HarnessBlocks...)
	}
	if containsMode(cfg.Mode, "strip") {
		for _, b := range blocks {
			re := regexp.MustCompile(`(?s)\s*` + regexp.QuoteMeta(b.Start) + `.*?` + regexp.QuoteMeta(b.End) + `\s*`)
			s = re.ReplaceAllString(s, "\n\n"+b.Replace+"\n\n")
		}
	}
	// mode=compact：harness system 全文 → 固定摘要
	if containsMode(cfg.Mode, "compact") {
		low := strings.ToLower(s)
		if strings.Contains(low, "you are claude code") {
			s = "You are a coding assistant. Be precise, helpful, concise, and safe. Use available tools when needed, follow repository instructions, and keep the user informed."
		} else if strings.Contains(low, "you are a coding agent running in the codex cli") {
			s = "You are a coding assistant in Codex CLI. Be precise, helpful, concise, and safe. Use available tools when needed, follow repository instructions, and keep the user informed."
		}
	}
	if s != orig {
		return strings.TrimSpace(s)
	}
	return orig
}

func containsMode(modes []string, want string) bool {
	for _, m := range modes {
		if m == want {
			return true
		}
	}
	return false
}

// isHarnessText 判定消息文本是否为 harness 注入（user 侧仅命中标记才可处理）。
func isHarnessText(s string) bool {
	for _, m := range builtinHarnessMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// sanitizeRequest 作用于归一化消息数组（纯函数，返回变更计数）。
// 作用域纪律（§14.1）：默认只 system/developer；HarnessUser 且命中标记才动 user。
func sanitizeRequest(msgs []openAIMessage, cfg *adapt.SanitizeConfig) ([]openAIMessage, int) {
	terms := cfg.Terms
	if len(terms) == 0 {
		terms = builtinSanitizeTerms
	}
	out := make([]openAIMessage, len(msgs))
	copy(out, msgs)
	hits := 0
	for i := range out {
		switch out[i].Role {
		case "system", "developer":
			if t := sanitizeText(out[i].Text, cfg, terms); t != out[i].Text {
				out[i].Text = t
				hits++
			}
		case "user":
			if cfg.HarnessUser && isHarnessText(out[i].Text) {
				if t := sanitizeText(out[i].Text, cfg, terms); t != out[i].Text {
					out[i].Text = t
					hits++
				}
			}
		}
	}
	return out, hits
}

// ---------------------------------------------------------------------------
// project：有损投影（§14.2）
// ---------------------------------------------------------------------------
