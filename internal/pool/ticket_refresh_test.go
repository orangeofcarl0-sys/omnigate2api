// 华为 ticket 30 天免登录续期（华为文档「配置账号30天免登录」+ HANDOFF §6.5）：
// refresh_token 缺失时，用持久化的 ticket+secret 静默换发新 STS。
package pool

import (
	"context"
	"errors"
	"io"
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

// ticket 兜底：无 refresh_token 时换发新 STS，ticket 凭证保留（登录窗口期内可再用），
// 并**标记实证换发成功**（面板据此才敢显示"续期 ticket"）。
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
	if !a.TicketVerified() {
		t.Fatal("换发成功必须标记实证（否则面板会把可续期的号显示成需重登）")
	}
}

// ticket 换发失败 → **按 errNoRenewalPath 分型**（2026-09-27 实测：登录后约 21.5h 再轮询
// 得 `TM.00001001 无效ticketId`）。落 default 分支会让账号在 STS 还没过期时被
// "refresh failed: …" 提前禁用，且原因指向不了动作；原始上游错误必须留在消息里以便排障。
func TestRefreshAuthCredsTicketFailure(t *testing.T) {
	acct, _ := ticketTestAccount(t)
	stub := &stubTicketPoller{err: errors.New("codearts api code=400 msg=无效ticketId")}
	err := refreshAuthCreds(acct, stub)
	if !errors.Is(err, errNoRenewalPath) {
		t.Fatalf("ticket 换发失败必须按 errNoRenewalPath 分型: %v", err)
	}
	if !contains(err.Error(), "ticket re-poll failed") || !contains(err.Error(), "无效ticketId") {
		t.Fatalf("必须保留 ticket 原始错误（排障）: %v", err)
	}
}

// stubCodeartsClient 实现 upstream.ChatAPI + ticketPoller，用来驱动 Validate 的续期分支
// （Validate 内部走 acct.Client 的类型断言，无法注入 pollerOverride）。
type stubCodeartsClient struct {
	pollErr  error
	pollResp *upstream.TokenResponse
}

func (s *stubCodeartsClient) ChatStream(ctx context.Context, chatID string, messages []upstream.ChatMessage,
	traceID string, cred upstream.SignCredential, userName, model string,
	tools []map[string]any, toolChoice string, gen map[string]any) (io.ReadCloser, error) {
	return nil, errors.New("not used in this test")
}

func (s *stubCodeartsClient) RefreshToken(ctx context.Context, cfg upstream.LoginConfig,
	refreshToken, codeVerifier, domain string) (*upstream.TokenResponse, error) {
	return nil, errors.New("no refresh token")
}

func (s *stubCodeartsClient) PollTicket(ctx context.Context, cfg upstream.LoginConfig,
	ticketID, secret string) (*upstream.TokenResponse, error) {
	if s.pollErr != nil {
		return nil, s.pollErr
	}
	return s.pollResp, nil
}

// 端到端：华为号 ticket 换发失败 + **仅临近过期** → 保持可用（重登需要时间，提前判死会
// 白丢可用窗口），且 renewal 不得宣称 ticket（未实证换发成功）。
func TestValidateTicketUnusableKeepsAccountUntilExpiry(t *testing.T) {
	soon := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	a := auth.New("h1", "hid_huawei", "d", "tok", "ak", "sk", soon, "", "verifier")
	a.SetTicketCreds("tid", "tsec")
	if err := auth.SaveNew(t.TempDir(), a); err != nil {
		t.Fatal(err)
	}
	p, err := New([]*auth.Auth{a}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	acct := p.Get("h1")
	acct.Client = &stubCodeartsClient{pollErr: errors.New("TM.00001001 无效ticketId")}
	if ok, _ := p.Validate(acct); !ok {
		t.Fatal("仅临近过期必须保持可用（重登需要时间，提前判死会白丢可用窗口）")
	}
	row := p.List()[0]
	if row["disabled"] == true {
		t.Fatalf("未过期不得禁用: %+v", row)
	}
	if row["renewal"] != "" {
		t.Fatalf("未实证换发成功不得宣称可续期: %v", row["renewal"])
	}
}

// 端到端：ticket 换发失败 + **已过期** → 禁用，原因指向"需重登"（不是 refresh failed）。
func TestValidateTicketUnusableDisablesAfterExpiry(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	a := auth.New("h2", "hid_huawei", "d", "tok", "ak", "sk", past, "", "verifier")
	a.SetTicketCreds("tid", "tsec")
	if err := auth.SaveNew(t.TempDir(), a); err != nil {
		t.Fatal(err)
	}
	p, err := New([]*auth.Auth{a}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	acct := p.Get("h2")
	acct.Client = &stubCodeartsClient{pollErr: errors.New("TM.00001001 无效ticketId")}
	if ok, _ := p.Validate(acct); ok {
		t.Fatal("已过期且无续期路必须判死")
	}
	row := p.List()[0]
	if row["disabled"] != true {
		t.Fatalf("必须禁用: %+v", row)
	}
	reason, _ := row["reason"].(string)
	if !contains(reason, "re-login required") {
		t.Fatalf("原因必须指向重登: %q", reason)
	}
	if contains(reason, "refresh failed") {
		t.Fatalf("原因不该是 refresh failed（指向不了动作）: %q", reason)
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

// renewalKind / List 暴露续期能力：refresh_token / ticket（**须实证换发成功**）/ 无
// （面板据此显示"到期需重登"）。2026-09-27 实测 ticket 只在登录窗口内有效，所以
// "有 ticket 凭证"不等于"能续期"——未实证就标 ticket 会把需重登的号显示成健康可续。
func TestListRenewalKind(t *testing.T) {
	withRT := &auth.Auth{UserID: "r1", UserName: "rt", Profile: "workbuddy",
		CloudDragonTok: "t", RefreshToken: "rt", Expiration: "2099-01-01T00:00:00Z"}
	withTicket := &auth.Auth{UserID: "k1", UserName: "ticket", Profile: "codearts",
		CloudDragonTok: "t", Expiration: "2099-01-01T00:00:00Z"}
	withTicket.SetTicketCreds("tid", "tsecret")
	withTicket.MarkTicketVerified() // 实证换发成功过
	unverifiedTicket := &auth.Auth{UserID: "k2", UserName: "ticket-unverified", Profile: "codearts",
		CloudDragonTok: "t", Expiration: "2099-01-01T00:00:00Z"}
	unverifiedTicket.SetTicketCreds("tid", "tsecret") // 有凭证但从未换发成功
	none := &auth.Auth{UserID: "n1", UserName: "none", Profile: "codearts",
		CloudDragonTok: "t", Expiration: "2099-01-01T00:00:00Z"}

	p, err := New([]*auth.Auth{withRT, withTicket, unverifiedTicket, none}, Config{
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
	if got["r1"] != "refresh_token" || got["k1"] != "ticket" || got["k2"] != "" || got["n1"] != "" {
		t.Fatalf("renewal kinds wrong: %+v", got)
	}
}

// 重新登录（SetTicketCreds）必须清零实证标记：上一轮会话的实证结论不适用于新 ticket。
func TestSetTicketCredsClearsVerified(t *testing.T) {
	a := auth.New("u1", "n1", "d", "tok", "ak", "sk", "2099-01-01T00:00:00Z", "", "v")
	a.SetTicketCreds("tid", "tsec")
	a.MarkTicketVerified()
	if !a.TicketVerified() {
		t.Fatal("MarkTicketVerified must take effect")
	}
	a.SetTicketCreds("tid2", "tsec2")
	if a.TicketVerified() {
		t.Fatal("新 ticket 未实证，不得继承上一轮的实证标记")
	}
}
