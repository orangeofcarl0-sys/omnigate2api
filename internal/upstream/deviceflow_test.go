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

	authURL, state, err := c.DeviceFlowState()
	if err != nil {
		t.Fatal(err)
	}
	if state != "S1" || authURL != "https://auth.example/flow" {
		t.Fatalf("state: %q %q", state, authURL)
	}

	tok, done, err := c.DeviceFlowToken(state)
	if err != nil || done {
		t.Fatalf("first poll must be pending: done=%v err=%v", done, err)
	}
	tok, done, err = c.DeviceFlowToken(state)
	if err != nil || !done {
		t.Fatalf("second poll must be done: done=%v err=%v", done, err)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" || tok.ExpiresIn != 7200 {
		t.Fatalf("token: %+v", tok)
	}

	uid, ent, nick, err := c.DeviceFlowAccount(state, tok.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "u9" || ent != "e9" || nick != "nick" {
		t.Fatalf("account: %q %q %q", uid, ent, nick)
	}
}
