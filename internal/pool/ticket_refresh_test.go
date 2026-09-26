// 华为 ticket 30 天免登录续期（华为文档「配置账号30天免登录」+ HANDOFF §6.5）：
// refresh_token 缺失时，用持久化的 ticket+secret 静默换发新 STS。
package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

type stubTicketPoller struct {
	resp               *upstream.TokenResponse
	err                error
	calls              int
	lastID, lastSecret string
}

func (s *stubTicketPoller) PollTicket(ctx context.Context, cfg upstream.LoginConfig, ticketID, secret string) (*upstream.TokenResponse, error) {
	s.calls++
	s.lastID, s.lastSecret = ticketID, secret
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func ticketTestAccount(t *testing.T) (*Account, *auth.Auth) {
	a := auth.New("u1", "n1", "d1", "sts-old", "ak", "sk",
		"2026-09-25T00:34:10Z", "", "verifier") // refresh_token 为空：ticket 通道不发
	a.SetTicketCreds("tid-1", "tsec-1")
	// 给落盘路径：SaveNew 在 TempDir 建档（顺带验证 ticket 字段随凭证持久化）
	if err := auth.SaveNew(t.TempDir(), a); err != nil {
		t.Fatal(err)
	}
	return &Account{Name: "a1", Auth: a}, a
}

// ticket 兜底：无 refresh_token 时静默换发新 STS，ticket 凭证保留（30 天会话期内可再换发）。
func TestRefreshAuthCredsTicketFallback(t *testing.T) {
	acct, a := ticketTestAccount(t)
	exp := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	stub := &stubTicketPoller{resp: &upstream.TokenResponse{
		UserID: "u1", UserName: "n1",
		Credentials: upstream.Credentials{
			SecurityToken: "sts-new", AccessKeyID: "ak2", SecretAccessKey: "sk2", Expiration: exp,
		},
	}}
	if err := refreshAuthCreds(acct, stub); err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 || stub.lastID != "tid-1" || stub.lastSecret != "tsec-1" {
		t.Fatalf("poll calls=%d id=%q secret=%q", stub.calls, stub.lastID, stub.lastSecret)
	}
	if a.CloudDragonTok != "sts-new" || a.Expiration != exp {
		t.Fatalf("sts not applied: tok=%q exp=%v", a.CloudDragonTok, a.Expiration)
	}
	if id, sec := a.TicketCreds(); id != "tid-1" || sec != "tsec-1" {
		t.Fatalf("ticket creds must be kept for next re-mint: %q/%q", id, sec)
	}
}

// ticket 会话失效（门户拒绝）→ 错误上抛，不静默吞掉。
func TestRefreshAuthCredsTicketFailure(t *testing.T) {
	acct, _ := ticketTestAccount(t)
	stub := &stubTicketPoller{err: errors.New("session expired")}
	err := refreshAuthCreds(acct, stub)
	if err == nil || !contains(err.Error(), "ticket re-poll failed") {
		t.Fatalf("want ticket re-poll error, got %v", err)
	}
}

// 两条续期路都没有 → errNoRenewalPath（调用方据此打"需要人工重登"日志）。
func TestRefreshAuthCredsNoPath(t *testing.T) {
	a := auth.New("u1", "n1", "d1", "sts", "ak", "sk", "2099-01-01T00:00:00Z", "", "")
	a.SetTicketCreds("", "")
	acct := &Account{Name: "a2", Auth: a}
	err := refreshAuthCreds(acct, nil)
	if !errors.Is(err, errNoRenewalPath) {
		t.Fatalf("want errNoRenewalPath, got %v", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// Validate 对"无续期路径"分型处置（2026-09-27 事故）：**已过期** → 判死并写明
// "re-login required"（否则面板显示健康、每个请求白打一次上游 401 才被禁用）；
// **仅临近过期** → 保持可用（给 ticket 会话/人工重登留时间窗）。
func TestValidateNoRenewalPathSplitsByExpiry(t *testing.T) {
	past := time.Now().Add(-12 * time.Hour).UTC().Format(time.RFC3339)
	soon := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)

	// 已过期 + 无 refresh_token/ticket → 禁用
	expired := &auth.Auth{UserID: "e1", UserName: "huawei-expired", Profile: "codearts",
		CloudDragonTok: "tok", Expiration: past}
	p, err := New([]*auth.Auth{expired}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := p.Validate(p.Get("e1")); ok {
		t.Fatal("expired token with no renewal path must not validate ok")
	}
	row := p.List()[0]
	if row["disabled"] != true {
		t.Fatalf("must be disabled: %+v", row)
	}
	if reason, _ := row["reason"].(string); !strings.Contains(reason, "re-login required") {
		t.Fatalf("reason must say re-login required: %+v", row["reason"])
	}

	// 临近过期（未过期）+ 无续期路 → 仍可用（不判死）
	soonAcct := &auth.Auth{UserID: "s1", UserName: "huawei-soon", Profile: "codearts",
		CloudDragonTok: "tok", Expiration: soon}
	p2, err := New([]*auth.Auth{soonAcct}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := p2.Validate(p2.Get("s1")); !ok {
		t.Fatal("still-valid token must stay usable until it actually expires")
	}
	if p2.List()[0]["disabled"] == true {
		t.Fatalf("must not disable before expiry: %+v", p2.List()[0])
	}
}

// renewalKind / List 暴露续期能力：refresh_token / ticket / 无（面板据此显示"到期需重登"）。
func TestListRenewalKind(t *testing.T) {
	withRT := &auth.Auth{UserID: "r1", UserName: "rt", Profile: "workbuddy",
		CloudDragonTok: "t", RefreshToken: "rt", Expiration: "2099-01-01T00:00:00Z"}
	withTicket := &auth.Auth{UserID: "k1", UserName: "ticket", Profile: "codearts",
		CloudDragonTok: "t", Expiration: "2099-01-01T00:00:00Z"}
	withTicket.SetTicketCreds("tid", "tsecret")
	none := &auth.Auth{UserID: "n1", UserName: "none", Profile: "codearts",
		CloudDragonTok: "t", Expiration: "2099-01-01T00:00:00Z"}

	p, err := New([]*auth.Auth{withRT, withTicket, none}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, row := range p.List() {
		got[row["uid"].(string)] = row["renewal"]
	}
	if got["r1"] != "refresh_token" || got["k1"] != "ticket" || got["n1"] != "" {
		t.Fatalf("renewal kinds wrong: %+v", got)
	}
}
