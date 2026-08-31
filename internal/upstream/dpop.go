// DPoP（RFC 9449）证明 JWT：ES256 + P-256，供 oauth2/tokens 请求头使用。
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
