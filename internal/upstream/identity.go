// 授权码通道的身份解析（SPEC §24.5）。
//
// 为什么需要：`grant_type=authorization_code` 的响应**只给凭证，不给身份**——实测
// `user_id/user_name/domain_id` 全为空（旧的 ticket 通道反而是带的）。没有身份就没法给
// 账号命名/落盘（会写出 `codearts-unknown.json` + 池里多一个空名账号），所以必须自己解：
//
//  1. 首选**离线**解 `refresh_token`（JWT，RS256）里的 `user_profile` 声明——实测它含
//     `principal_id`（= 账号 uid）、`account_name`（= 用户名，hid_ 开头）、
//     `account_id`（= 租户 domain_id），且 `exp` 恰好是 30 天后（华为「30 天免登录」的来源）。
//  2. 兜底再打一次 `/snap-manager/v1/current/user`（官方内核 `getUserInfoByCredentials`
//     就是这么做的）。
package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// refreshTokenProfile refresh_token JWT 里 user_profile 的字段（实测形状）。
type refreshTokenProfile struct {
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	PrincipalID string `json:"principal_id"`
}

// RefreshTokenIdentity 从 refresh_token（JWT）离线解出账号身份。
// 失败不致命——调用方会退到 CurrentUser。
func RefreshTokenIdentity(refreshToken string) (userID, userName, domainID string, err error) {
	parts := strings.Split(strings.TrimSpace(refreshToken), ".")
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("refresh_token is not a JWT")
	}
	payload, err := b64dec(parts[1])
	if err != nil {
		return "", "", "", fmt.Errorf("decode refresh_token payload: %w", err)
	}
	var claims struct {
		UserProfile string `json:"user_profile"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", "", fmt.Errorf("parse refresh_token claims: %w", err)
	}
	if claims.UserProfile == "" {
		return "", "", "", fmt.Errorf("refresh_token has no user_profile")
	}
	raw, err := b64dec(claims.UserProfile)
	if err != nil {
		return "", "", "", fmt.Errorf("decode user_profile: %w", err)
	}
	var p refreshTokenProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", "", fmt.Errorf("parse user_profile: %w", err)
	}
	if p.PrincipalID == "" {
		return "", "", "", fmt.Errorf("user_profile has no principal_id")
	}
	return p.PrincipalID, p.AccountName, p.AccountID, nil
}

// b64dec 尽量宽容地解 base64（url-safe / 标准、有无 padding 都试）。
func b64dec(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not valid base64")
}

// CurrentUser 用凭证查 /snap-manager/v1/current/user（身份兜底）。
// 字段名容错（user_id/userId 等）——该端点没有公开文档。
func (c *Client) CurrentUser(ctx context.Context, cfg LoginConfig, cred SignCredential) (userID, userName, domainID string, err error) {
	endpoint := strings.TrimRight(cfg.SnapManager, "/") + EpCurrentUser
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Accept", "application/json")
	signRequest(req, nil, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", "", &ApiError{Code: resp.StatusCode, Status: resp.StatusCode,
			Message: truncateStr(string(raw), 300), Path: EpCurrentUser}
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", "", "", fmt.Errorf("parse current/user: %w", err)
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	return pick("user_id", "userId", "id"),
		pick("user_name", "userName", "name", "nickname"),
		pick("domain_id", "domainId", "domain"), nil
}

// enrichIdentity 给授权码通道的凭证补身份：先离线解 refresh_token，再退到 current/user。
// 两者都拿不到 → 报错（宁可这次登录失败，也不要落一个无名账号：会写出
// `codearts-unknown.json` 并在池里多出一个空名条目）。
func (c *Client) enrichIdentity(ctx context.Context, cfg LoginConfig, tok *TokenResponse) error {
	if tok == nil {
		return fmt.Errorf("empty token response")
	}
	if tok.UserID != "" {
		return nil
	}
	if uid, name, did, err := RefreshTokenIdentity(tok.RefreshToken); err == nil && uid != "" {
		tok.UserID, tok.UserName, tok.DomainID = uid, name, did
		return nil
	} else if err == nil {
		return nil
	}
	uid, name, did, err := c.CurrentUser(ctx, cfg, SignCredential{
		AccessKeyID:     tok.Credentials.AccessKeyID,
		SecretAccessKey: tok.Credentials.SecretAccessKey,
		SecurityToken:   tok.Credentials.SecurityToken,
	})
	if err != nil {
		return fmt.Errorf("resolve identity failed (refresh_token 无 user_profile 且 current/user 调用失败: %w)", err)
	}
	if uid == "" {
		return fmt.Errorf("resolve identity failed: upstream returned no user_id")
	}
	tok.UserID = uid
	if tok.UserName == "" {
		tok.UserName = name
	}
	if tok.DomainID == "" {
		tok.DomainID = did
	}
	return nil
}
