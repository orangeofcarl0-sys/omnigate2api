// 面板腾讯 OAuth 集成（SPEC §24.3）：start 拿授权链接、poll pending→done、
// 成功落盘凭证并热加入账号池。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omnigate2api/internal/adapt"
)

func TestOAuthTencentPanelFlow(t *testing.T) {
	var polled int
	tencent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/auth/state"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"state":"S1","authUrl":"https://auth.example/flow"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/auth/token"):
			polled++
			if polled == 1 {
				_, _ = io.WriteString(w, `{"code":11217,"msg":"login ing","data":null}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"accessToken":"at","refreshToken":"rt","expiresIn":7200,"domain":"www.codebuddy.cn"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/login/account"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"uid":"u9","enterpriseId":"e9","nickname":"nick"}}`)
		default:
			t.Fatalf("unexpected: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer tencent.Close()
	t.Setenv("OMNIGATE_TENCENT_BASE", tencent.URL)

	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	authDir := t.TempDir()
	srv, p, _, h := buildTestServer(t, huawei.URL, nil)
	h.cfg.AuthDir = authDir
	h.cfg.Profiles = adapt.NewRegistry(&adapt.Codearts, &adapt.Workbuddy)

	post := func(path, body string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}

	st := post("/admin/api/oauth/tencent/start", `{}`)
	if st["ok"] != true || st["auth_url"] != "https://auth.example/flow" {
		t.Fatalf("start: %v", st)
	}
	state := st["state"].(string)

	pend := post("/admin/api/oauth/tencent/poll", `{"state":"`+state+`"}`)
	if pend["waiting"] != true {
		t.Fatalf("first poll must wait: %v", pend)
	}
	done := post("/admin/api/oauth/tencent/poll", `{"state":"`+state+`"}`)
	if done["ok"] != true || done["uid"] != "u9" || done["nickname"] != "nick" {
		t.Fatalf("done poll: %v", done)
	}

	// 落盘命名空间文件 + 热入池
	files, _ := filepath.Glob(filepath.Join(authDir, "workbuddy-*.json"))
	if len(files) != 1 {
		t.Fatalf("credential file must land: %v", files)
	}
	acct := p.Get("u9")
	if acct == nil || acct.ProfileID != "workbuddy" {
		t.Fatalf("account must join pool hot: %+v", acct)
	}
	raw, _ := os.ReadFile(files[0])
	if !strings.Contains(string(raw), "workbuddy") {
		t.Fatalf("credential namespace wrong: %s", raw[:120])
	}
	// state 已消费（过期/删除后重复 poll → 400）
	if st2 := post("/admin/api/oauth/tencent/poll", `{"state":"`+state+`"}`); st2["ok"] != false {
		t.Fatalf("state must be consumed: %v", st2)
	}
	_ = time.Now()
}
