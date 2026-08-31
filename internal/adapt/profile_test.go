package adapt

import "testing"

// 内置 codearts Profile 必须通过自校验（缺省可靠）。
func TestCodeartsProfileValid(t *testing.T) {
	if err := Codearts.Validate(); err != nil {
		t.Fatalf("codearts profile invalid: %v", err)
	}
	if got := Codearts.ReanchorEvery(); got != 5 {
		t.Fatalf("codearts trust=low should reanchor every 5, got %d", got)
	}
	if !Codearts.IsRateLimit("codearts error code=InferHub.MaaS.003002002.502 msg=provider API error (status 429)") {
		t.Fatalf("rate limit detection failed")
	}
	if Codearts.IsRateLimit("random failure") {
		t.Fatalf("false positive rate limit")
	}
}

// 校验矩阵（SPEC §4.2）：非法 Profile 必须被拒绝。
func TestProfileValidate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*UpstreamProfile)
	}{
		{"id required", func(p *UpstreamProfile) { p.ID = "" }},
		{"text-only needs folding", func(p *UpstreamProfile) { p.Message.Folding = nil }},
		{"implicit needs trust", func(p *UpstreamProfile) { p.Session.Trust = "" }},
		{"bad kind", func(p *UpstreamProfile) { p.Session.Kind = "magic" }},
		{"bad trust", func(p *UpstreamProfile) { p.Session.Trust = "extreme" }},
		{"no rate hints", func(p *UpstreamProfile) { p.Limits.RateLimitHints = nil }},
	}
	for _, c := range cases {
		p := Codearts
		c.mut(&p)
		if err := p.Validate(); err == nil {
			t.Fatalf("%s: expected validation error", c.name)
		}
	}
}

// 显式 reanchor_every 优先于 trust 缺省。
func TestReanchorOverride(t *testing.T) {
	p := Codearts
	p.Session.ReanchorEvery = 20
	if got := p.ReanchorEvery(); got != 20 {
		t.Fatalf("explicit reanchor must win: %d", got)
	}
}
