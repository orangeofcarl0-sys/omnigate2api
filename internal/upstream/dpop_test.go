// DPoP 私钥落盘与复用（SPEC §24.5）：refresh_token 与 DPoP 公钥绑定，刷新必须用同一把，
// 换新密钥会被上游拒（`STS5.1806 … InvalidDPoPHeader`）。所以这里测三件事：
//  1. 生成的 JWK 能解析回同一把密钥（公钥逐字一致）；
//  2. 解析出的密钥能签出可校验的 DPoP proof（证明私钥真能用，而不只是"解出了数"）；
//  3. 损坏/不一致的落盘内容必须报错（宁可当场失败，也别拿临时密钥去撞上游）。
package upstream

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

func TestDpopKeyJSONRoundTrip(t *testing.T) {
	raw, err := NewDpopKeyJSON()
	if err != nil {
		t.Fatal(err)
	}
	kp, err := dpopKeyFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if kp == nil {
		t.Fatal("non-empty key must parse to a key pair")
	}
	// 公钥 JWK 必须与"从私钥重新推出"的一致
	x := base64.RawURLEncoding.EncodeToString(padded(kp.PrivateKey.PublicKey.X, 32))
	y := base64.RawURLEncoding.EncodeToString(padded(kp.PrivateKey.PublicKey.Y, 32))
	if kp.PublicJWK["x"] != x || kp.PublicJWK["y"] != y {
		t.Fatalf("public JWK mismatch: %v vs x=%s y=%s", kp.PublicJWK, x, y)
	}
	if _, err := dpopKeyFromJSON(raw); err != nil {
		t.Fatalf("key must stay parseable (refresh 每次都要用): %v", err)
	}
}

func TestDpopKeySignsVerifiableProof(t *testing.T) {
	raw, err := NewDpopKeyJSON()
	if err != nil {
		t.Fatal(err)
	}
	kp, err := dpopKeyFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := signDpopProof(kp, "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("proof must be a JWS compact string: %q", proof)
	}
	var hdr struct {
		Alg string            `json:"alg"`
		Typ string            `json:"typ"`
		JWK map[string]string `json:"jwk"`
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Alg != "ES256" || hdr.Typ != "dpop+jwt" || hdr.JWK["crv"] != "P-256" {
		t.Fatalf("header=%+v", hdr)
	}
	// 用 header 里的公钥验签（= 上游会做的检查）
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 64 {
		t.Fatalf("ES256 signature must be 64 bytes, got %d", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	pub := ecdsa.PublicKey{Curve: elliptic.P256(),
		X: new(big.Int).SetBytes(mustB64(t, hdr.JWK["x"])),
		Y: new(big.Int).SetBytes(mustB64(t, hdr.JWK["y"]))}
	if !ecdsa.Verify(&pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("DPoP proof signature must verify against the JWK in its header")
	}
}

func TestDpopKeyRejectsBrokenJSON(t *testing.T) {
	cases := map[string]string{
		"空对象":  `{}`,
		"缺 d":  `{"kty":"EC","crv":"P-256","x":"AA","y":"AA"}`,
		"非法长度": `{"kty":"EC","crv":"P-256","x":"AA","y":"AA","d":"AA"}`,
	}
	for name, raw := range cases {
		if _, err := dpopKeyFromJSON(raw); err == nil {
			t.Fatalf("%s：必须报错", name)
		}
	}
	// 空串是合法输入（旧 ticket 通道没有固定密钥 → 调用方临时生成）
	kp, err := dpopKeyFromJSON("")
	if err != nil || kp != nil {
		t.Fatalf("empty must yield (nil,nil), got %v %v", kp, err)
	}
	// x/y 与 d 不一致（落盘被改坏）必须报错，而不是拿着错公钥去请求
	good, err := NewDpopKeyJSON()
	if err != nil {
		t.Fatal(err)
	}
	var j map[string]string
	if err := json.Unmarshal([]byte(good), &j); err != nil {
		t.Fatal(err)
	}
	j["x"] = base64.RawURLEncoding.EncodeToString(make([]byte, 32)) // 全零点在曲线上不成立
	bad, _ := json.Marshal(j)
	if _, err := dpopKeyFromJSON(string(bad)); err == nil {
		t.Fatal("公钥与私钥不一致必须报错")
	}
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 授权码响应不带身份，身份靠解 refresh_token 的 user_profile（实测形状：principal_id =
// 账号 uid、account_name = hid_ 用户名、account_id = 租户 domain_id）。这是"能落盘"的前提。
func TestRefreshTokenIdentity(t *testing.T) {
	profile := `{"account_id":"domain-placeholder-0001","account_name":"hid_account_placeholder","features_switches":{"enable_pdp5":true},"principal_id":"uid-placeholder-0001","principal_is_root_user":true}`
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"user_profile":"` + base64.RawURLEncoding.EncodeToString([]byte(profile)) + `","exp":1793120774}`))
	jwt := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"RS256"}`)) + "." + payload + ".sig"

	uid, name, did, err := RefreshTokenIdentity(jwt)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "uid-placeholder-0001" {
		t.Fatalf("uid=%q（应取 principal_id）", uid)
	}
	if name != "hid_account_placeholder" || did != "domain-placeholder-0001" {
		t.Fatalf("name=%q did=%q", name, did)
	}
	// 非 JWT / 无 user_profile 必须报错（调用方据此退到 current/user）
	if _, _, _, err := RefreshTokenIdentity("not-a-jwt"); err == nil {
		t.Fatal("非 JWT 必须报错")
	}
	noProfile := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`))
	if _, _, _, err := RefreshTokenIdentity("h." + noProfile + ".s"); err == nil {
		t.Fatal("无 user_profile 必须报错")
	}
}
