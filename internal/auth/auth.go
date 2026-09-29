// Package auth 管理 auths/ 目录下的 CodeArts 凭证文件 codearts-{user_id}.json。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 单个账号凭证（登录 oauth2/tokens 返回）。
// 华为账号：CloudDragonTok=STS security_token + AK/SK 签名；
// 腾讯（workbuddy）账号：CloudDragonTok=accessToken、RefreshToken=refreshToken、
// EnterpriseID/Domain 为 X-Enterprise-Id/X-Domain 头值（SPEC §24.1 字段位复用）。
type Auth struct {
	UserID          string `json:"user_id"`
	UserName        string `json:"user_name"`
	DomainID        string `json:"domain_id"`
	CloudDragonTok  string `json:"cloud_dragon_token"` // = STS security_token / 腾讯 accessToken
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Expiration      string `json:"expiration"` // RFC3339
	RefreshToken    string `json:"refresh_token"`
	CodeVerifier    string `json:"code_verifier"` // PKCE verifier，refresh 需要
	// DpopKey 华为 DPoP 私钥（JWK JSON，SPEC §24.5）。授权码登录时生成、随凭证落盘：
	// `refresh_token` 与 DPoP 公钥绑定，刷新必须复用同一把，换新密钥会被上游拒
	// （`STS5.1806 … InvalidDPoPHeader`）。腾讯留空。
	DpopKey string `json:"dpop_key,omitempty"`
	// 华为 ticket 登录通道（HANDOFF §6.5 / 华为文档「配置账号30天免登录」）：
	// 登录时由门户下发，用于轮询登录结果。
	//
	// **2026-09-27 实测更正**：ticket 只在登录窗口内有效——登录后约 21.5h 用同一
	// ticket_id 再轮询 → `TM.00001001 无效ticketId`。它**不是续期路**：STS 24h 到期
	// 仍需人工重登（"30 天档"只让重登免密，不让网关静默换发）。
	TicketID     string `json:"ticket_id,omitempty"`
	TicketSecret string `json:"ticket_secret,omitempty"`
	// TicketVerifiedAt 最近一次 ticket 静默换发**成功**的时间戳（0 = 从未成功）。面板
	// 只据此标注"续期 ticket"——能力必须实证过才允许宣称（HANDOFF §6.5.1 教训）。
	TicketVerifiedAt int64 `json:"ticket_verified_at,omitempty"`
	UpdatedAt        int64 `json:"updated_at"`
	// 腾讯专用（华为留空）
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Domain       string `json:"domain,omitempty"`
	// Profile 归属（auths 文件名前缀决定；序列化保留便于对齐）
	Profile string `json:"profile,omitempty"`

	path string
	mu   sync.Mutex
}

// New 构造 Auth（登录落盘用）。
func New(userID, userName, domainID, token, ak, sk, expiration, refreshToken, codeVerifier string) *Auth {
	return &Auth{
		UserID:          userID,
		UserName:        userName,
		DomainID:        domainID,
		CloudDragonTok:  token,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		Expiration:      expiration,
		RefreshToken:    refreshToken,
		CodeVerifier:    codeVerifier,
		UpdatedAt:       time.Now().Unix(),
	}
}

// FileName 返回 auth 文件名（按 Profile 前缀命名空间，SPEC §24.1：
// codearts-*.json 华为 / workbuddy-*.json 腾讯）。
func (a *Auth) FileName() string {
	prefix := "codearts"
	if a.Profile == "workbuddy" {
		prefix = "workbuddy"
	}
	id := strings.ReplaceAll(a.UserID, string(filepath.Separator), "_")
	if id == "" {
		id = "unknown"
	}
	return fmt.Sprintf("%s-%s.json", prefix, id)
}

// Token 返回 cloud_dragon_token（并发安全快照）。
func (a *Auth) Token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CloudDragonTok
}

// Refresh 返回 refresh_token。
func (a *Auth) Refresh() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// Verifier 返回 PKCE code_verifier。
func (a *Auth) Verifier() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CodeVerifier
}

// Dpop 返回落盘的 DPoP 私钥（JWK JSON；空 = 该凭证没有固定密钥，refresh 时临时生成）。
func (a *Auth) Dpop() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.DpopKey
}

// SetCredentials 授权码登录/刷新成功后写回凭证三件套（SPEC §24.5）：
// refresh_token（**一次性轮换**，必须回写新值）、PKCE verifier、DPoP 私钥。
// 空值不清空既有字段（refresh 响应可能只轮换 refresh_token）。
func (a *Auth) SetCredentials(refreshToken, codeVerifier, dpopKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if refreshToken != "" {
		a.RefreshToken = refreshToken
	}
	if codeVerifier != "" {
		a.CodeVerifier = codeVerifier
	}
	if dpopKey != "" {
		a.DpopKey = dpopKey
	}
}

// TicketCreds 返回 ticket 免登录通道凭证（ticket_id + secret）。
func (a *Auth) TicketCreds() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.TicketID, a.TicketSecret
}

// SetTicketCreds 登录成功后更新 ticket 登录通道凭证（新 ticket 未实证换发能力，
// 一并清零 TicketVerifiedAt：上一轮会话的实证结论不适用于新会话）。
func (a *Auth) SetTicketCreds(id, secret string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.TicketID, a.TicketSecret = id, secret
	a.TicketVerifiedAt = 0
}

// TicketVerified 报告该 ticket 通道是否**实证**换发过 STS（面板据此标注续期能力）。
func (a *Auth) TicketVerified() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.TicketVerifiedAt > 0
}

// MarkTicketVerified 记录一次成功的 ticket 静默换发（换发成功才允许宣称可续期）。
func (a *Auth) MarkTicketVerified() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.TicketVerifiedAt = time.Now().Unix()
}

// ExpiresAt 返回 token 过期时间。
func (a *Auth) ExpiresAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, err := time.Parse(time.RFC3339, a.Expiration); err == nil {
		return t
	}
	return time.Time{}
}

// Remaining 返回剩余有效期。
func (a *Auth) Remaining() time.Duration {
	t := a.ExpiresAt()
	if t.IsZero() {
		return 24 * time.Hour
	}
	return time.Until(t)
}

// Expired 报告 token 是否已过期。
func (a *Auth) Expired() bool { return a.Remaining() <= 0 }

// ExpiringSoon 报告 token 是否将在 within 内过期。
func (a *Auth) ExpiringSoon(within time.Duration) bool {
	r := a.Remaining()
	return r > 0 && r <= within
}

// Save 原子写回 auth 文件（0600）。
func (a *Auth) Save() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return saveLocked(a.path, a)
}

// SetPath 设置文件路径（LoadDir 内部调用）。
func (a *Auth) SetPath(p string) { a.path = p }

// LoadDir 加载 auths 目录下全部凭证（多上游命名空间：codearts-*.json /
// workbuddy-*.json，SPEC §24.1），解析失败静默跳过。
func LoadDir(dir string) ([]*Auth, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Auth
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		profile := ""
		switch {
		case strings.HasPrefix(e.Name(), "workbuddy-"):
			profile = "workbuddy"
		case strings.HasPrefix(e.Name(), "codearts-"):
			profile = "codearts"
		default:
			continue
		}
		p := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var a Auth
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		if a.CloudDragonTok == "" {
			continue
		}
		a.Profile = profile
		a.path = p
		out = append(out, &a)
	}
	return out, nil
}

// SaveNew 把新账号写入 auths 目录。
func SaveNew(dir string, a *Auth) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	a.path = filepath.Join(dir, a.FileName())
	return a.Save()
}

func saveLocked(path string, v any) error {
	if path == "" {
		return fmt.Errorf("auth: empty path")
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
