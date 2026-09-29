// DPoP（RFC 9449）证明 JWT：ES256 + P-256，供 oauth2/tokens 请求头使用。
//
// **密钥必须落盘复用**（SPEC §24.5）：`refresh_token` 与 DPoP 公钥绑定，换新密钥再刷新
// 会被上游拒（`STS5.1806 … InvalidDPoPHeader`）。所以授权码登录时生成一次、随凭证存
// `dpop_key`，之后每次 refresh 都用同一把。
package upstream

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// dpopKeyPair DPoP 密钥对。
type dpopKeyPair struct {
	PrivateKey *ecdsa.PrivateKey
	PublicJWK  map[string]string
}

func newDpopKeyPair() (*dpopKeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	x := padded(key.PublicKey.X, 32)
	y := padded(key.PublicKey.Y, 32)
	jwk := map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(x),
		"y":   base64.RawURLEncoding.EncodeToString(y),
	}
	return &dpopKeyPair{PrivateKey: key, PublicJWK: jwk}, nil
}

// dpopPrivateJWK DPoP 私钥的落盘形态（`d` 为私钥标量）。公钥 x/y 冗余存一份，
// 便于解析失败时对账（解析时会校验 x/y 是否真由 d 推出）。
type dpopPrivateJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

// NewDpopKeyJSON 生成 DPoP 私钥并序列化为 JWK JSON（登录取码时调用一次，随凭证落盘）。
func NewDpopKeyJSON() (string, error) {
	kp, err := newDpopKeyPair()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(dpopPrivateJWK{
		Kty: "EC", Crv: "P-256",
		X: kp.PublicJWK["x"], Y: kp.PublicJWK["y"],
		D: base64.RawURLEncoding.EncodeToString(padded(kp.PrivateKey.D, 32)),
	})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// dpopKeyFromJSON 解析落盘的 DPoP 私钥。空串 → (nil, nil)：调用方自行临时生成
// （旧 ticket 通道没有 refresh_token，也就不需要固定密钥）。
func dpopKeyFromJSON(s string) (*dpopKeyPair, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var j dpopPrivateJWK
	if err := json.Unmarshal([]byte(s), &j); err != nil {
		return nil, fmt.Errorf("parse dpop key: %w", err)
	}
	if j.X == "" || j.Y == "" || j.D == "" {
		return nil, fmt.Errorf("dpop key incomplete (need x/y/d)")
	}
	xb, err1 := base64.RawURLEncoding.DecodeString(j.X)
	yb, err2 := base64.RawURLEncoding.DecodeString(j.Y)
	db, err3 := base64.RawURLEncoding.DecodeString(j.D)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, fmt.Errorf("dpop key base64 invalid")
	}
	// P-256 的坐标/标量固定 32 字节（我们的写入侧一律 padded 到 32）——长度不对就是落盘坏了。
	if len(xb) != 32 || len(yb) != 32 || len(db) != 32 {
		return nil, fmt.Errorf("dpop key must be 32-byte P-256 values (x=%d y=%d d=%d)", len(xb), len(yb), len(db))
	}
	d := new(big.Int).SetBytes(db)
	px, py := elliptic.P256().ScalarBaseMult(db)
	// 自检：x/y 必须真由 d 推出，否则落盘损坏会让每次刷新都白打一次上游
	if px.Cmp(new(big.Int).SetBytes(xb)) != 0 || py.Cmp(new(big.Int).SetBytes(yb)) != 0 {
		return nil, fmt.Errorf("dpop key inconsistent (x/y do not match d)")
	}
	return &dpopKeyPair{
		PrivateKey: &ecdsa.PrivateKey{
			PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: px, Y: py},
			D:         d,
		},
		PublicJWK: map[string]string{"kty": "EC", "crv": "P-256", "x": j.X, "y": j.Y},
	}, nil
}

// signDpopProof 生成 DPoP JWT（htm=POST，htu=token 端点）。
func signDpopProof(kp *dpopKeyPair, htu string) (string, error) {
	header := map[string]any{
		"alg": "ES256",
		"typ": "dpop+jwt",
		"jwk": kp.PublicJWK,
	}
	headerJSON, _ := json.Marshal(header)
	payload := map[string]any{
		"htm": "POST",
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": randomHexLower(16),
	}
	payloadJSON, _ := json.Marshal(payload)
	input := b64(headerJSON) + "." + b64(payloadJSON)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, kp.PrivateKey, digest[:])
	if err != nil {
		return "", err
	}
	// 低 S 归一化（jose 默认低 S，服务端兼容性更好）
	n := elliptic.P256().Params().N
	halfN := new(big.Int).Rsh(n, 1)
	if s.Cmp(halfN) > 0 {
		s.Sub(n, s)
	}
	sig := append(padded(r, 32), padded(s, 32)...)
	return input + "." + b64(sig), nil
}

func padded(b *big.Int, size int) []byte {
	out := make([]byte, size)
	raw := b.Bytes()
	copy(out[size-len(raw):], raw)
	return out
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomHexLower(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
