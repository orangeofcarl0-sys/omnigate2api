// 腾讯设备流能力测试（SPEC §24.3）：state/token（pending→done）/account 三端点。
package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDeviceFlow 假设备流服务器：state 幂等；token 首次 pending、随后 done；account 需 Bearer。
func fakeDeviceFlow(t *testing.T) *httptest.Server {
	t.Helper()
	var polled int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/auth/state"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"state":"S1","authUrl":"https://auth.example/flow"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/auth/token"):
			polled++
			if polled == 1 {
				_, _ = io.WriteString(w, `{"code":11217,"msg":"login ing","data":null}`) // pending
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"accessToken":"at","refreshToken":"rt","expiresIn":7200,"domain":"www.codebuddy.cn"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/login/account"):
			if got := r.Header.Get("Authorization"); got != "Bearer at" {
				t.Fatalf("account must carry Bearer: %q", got)
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"uid":"u9","enterpriseId":"e9","nickname":"nick"}}`)
		default:
			t.Fatalf("unexpected: %s %s", r.Method, r.URL.Path)
		}
	}))
}

func TestTencentDeviceFlow(t *testing.T) {
	srv := fakeDeviceFlow(t)
	defer srv.Close()
	t.Setenv("OMNIGATE_TENCENT_BASE", srv.URL)
	c := NewTencent(5 * time.Second)

	authURL, state, err := c.DeviceFlowState("")
	if err != nil {
		t.Fatal(err)
	}
	if state != "S1" || authURL != "https://auth.example/flow" {
		t.Fatalf("state: %q %q", state, authURL)
	}

	tok, done, err := c.DeviceFlowToken("", state)
	if err != nil || done {
		t.Fatalf("first poll must be pending: done=%v err=%v", done, err)
	}
	tok, done, err = c.DeviceFlowToken("", state)
	if err != nil || !done {
		t.Fatalf("second poll must be done: done=%v err=%v", done, err)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" || tok.ExpiresIn != 7200 {
		t.Fatalf("token: %+v", tok)
	}

	uid, ent, nick, err := c.DeviceFlowAccount("", state, tok.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "u9" || ent != "e9" || nick != "nick" {
		t.Fatalf("account: %q %q %q", uid, ent, nick)
	}
}

// 设备流区域选择（HANDOFF §6.6）：区域由 base 决定——国内 copilot.tencent.com、
// 国际 www.workbuddy.ai；deviceFlowBase 不依赖凭证 domain（登录前没有 domain）。
func TestTencentDeviceFlowRealm(t *testing.T) {
	t.Setenv("OMNIGATE_TENCENT_BASE", "")

	cases := []struct{ realm, base, origin string }{
		{"", "https://copilot.tencent.com", "https://www.codebuddy.cn"},
		{"cn", "https://copilot.tencent.com", "https://www.codebuddy.cn"},
		{"global", "https://www.workbuddy.ai", "https://www.workbuddy.ai"},
	}
	for _, tc := range cases {
		base, origin := NewTencent(5 * time.Second).deviceFlowBase(tc.realm)
		if base != tc.base || origin != tc.origin {
			t.Fatalf("realm=%q base=%q origin=%q want %q %q", tc.realm, base, origin, tc.base, tc.origin)
		}
	}
	// 实验/测试覆盖优先于区域（stub 服务器两域共用）
	t.Setenv("OMNIGATE_TENCENT_BASE", "http://127.0.0.1:1/stub")
	if base, _ := NewTencent(5 * time.Second).deviceFlowBase("global"); base != "http://127.0.0.1:1/stub" {
		t.Fatalf("env override must win: %q", base)
	}
	// 账号域兜底：按区域给默认登录域（TencentRegion 按此后缀分流）
	if DeviceFlowDomain("global") != "www.workbuddy.ai" || DeviceFlowDomain("cn") != "www.codebuddy.cn" {
		t.Fatalf("default domains wrong: %q %q", DeviceFlowDomain("global"), DeviceFlowDomain("cn"))
	}
	if !TencentRegion(DeviceFlowDomain("global")) || TencentRegion(DeviceFlowDomain("cn")) {
		t.Fatalf("default domain must imply its realm")
	}
}
