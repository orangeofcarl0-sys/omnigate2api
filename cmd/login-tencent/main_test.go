// login-tencent 设备流 mock 测试（8f 前置，SPEC §28.6 项 1 的假上游形态）：
// OMNIGATE_TENCENT_BASE 覆盖 base，auth/state → token → login/account 三段式端到端，
// 断言落盘命名空间文件与头保真。
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTencentLogin 假 copilot 登录三端点；记录 token 请求头。
func fakeTencentLogin(t *testing.T, tokenUA *string, acctAuthz *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/plugin/auth/state":
			w.Header().Set("Content-Type", "application/json")
			// 8f 真链路实证形态：{code,msg,data} envelope
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"state":"S1","authUrl":"https://auth.example/flow"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/plugin/auth/token":
			if tokenUA != nil {
				*tokenUA = r.Header.Get("User-Agent")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"accessToken":"acc-tok","refreshToken":"ref-tok","expiresIn":7200,"domain":"www.codebuddy.cn"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/plugin/login/account":
			if acctAuthz != nil {
				*acctAuthz = r.Header.Get("Authorization")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"uid":"u9","enterpriseId":"e9","nickname":"nick"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
}

func TestLoginTencentDeviceFlow(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	authDir := filepath.Join(dir, "auths")

	var tokenUA, acctAuthz string
	srv := fakeTencentLogin(t, &tokenUA, &acctAuthz)
	defer srv.Close()
	t.Setenv("OMNIGATE_TENCENT_BASE", srv.URL)

	// url 段：发起授权 → state 落盘 + 打印 authUrl
	runURL(stateFile)
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var ls struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil || ls.State != "S1" {
		t.Fatalf("state file: %s err=%v", raw, err)
	}

	// poll 段：token + account → 落盘 workbuddy-u9.json，state 文件清除
	runPoll(authDir, stateFile)
	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Fatalf("state file must be removed after success")
	}
	authPath := filepath.Join(authDir, "workbuddy-u9.json")
	out, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("namespace file missing: %v", err)
	}
	var a struct {
		UserID         string `json:"user_id"`
		UserName       string `json:"user_name"`
		Profile        string `json:"profile"`
		CloudDragonTok string `json:"cloud_dragon_token"`
		RefreshToken   string `json:"refresh_token"`
		EnterpriseID   string `json:"enterprise_id"`
		Domain         string `json:"domain"`
		Expiration     string `json:"expiration"`
	}
	if err := json.Unmarshal(out, &a); err != nil {
		t.Fatal(err)
	}
	if a.UserID != "u9" || a.Profile != "workbuddy" || a.UserName != "nick" {
		t.Fatalf("account fields: %+v", a)
	}
	if a.CloudDragonTok != "acc-tok" || a.RefreshToken != "ref-tok" || a.EnterpriseID != "e9" || a.Domain != "www.codebuddy.cn" {
		t.Fatalf("token fields: %+v", a)
	}
	if a.Expiration == "" {
		t.Fatal("expiration must be derived from expiresIn")
	}
	// 头保真对齐（8e G3）：官方 CLI UA；account 请求带 Bearer
	if tokenUA != "CLI/2.63.2 CodeBuddy/2.63.2" {
		t.Fatalf("token UA must be official CLI: %q", tokenUA)
	}
	if acctAuthz != "Bearer acc-tok" {
		t.Fatalf("account request must carry Bearer: %q", acctAuthz)
	}
}

// TestLoginTencentPollPending 未授权完成时 poll 报错（无 accessToken 视为 pending）。
func TestLoginTencentPollPending(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	if err := os.WriteFile(stateFile, []byte(`{"state":"S1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":10001,"msg":"login ing","data":null}`)
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_TENCENT_BASE", srv.URL)

	err := runPoll(filepath.Join(dir, "auths"), stateFile)
	if err == nil || !strings.Contains(err.Error(), "waiting for login") {
		t.Fatalf("pending poll must explain waiting: %v", err)
	}
}

// TestEnvelopeOrFlatFlatFallback 扁平结构（参考实现派生夹具）仍可解析。
func TestEnvelopeOrFlatFlatFallback(t *testing.T) {
	data, code, msg, ok := envelopeOrFlat([]byte(`{"state":"S1","authUrl":"u"}`))
	if ok || code != 0 || msg != "" {
		t.Fatalf("flat must not claim envelope: ok=%v code=%d msg=%q", ok, code, msg)
	}
	if data != nil {
		t.Fatalf("flat data=%s", data)
	}
	envData, envCode, envMsg, envOK := envelopeOrFlat([]byte(`{"code":0,"msg":"OK","data":{"state":"S1"}}`))
	if !envOK || envCode != 0 || envMsg != "OK" || len(envData) == 0 {
		t.Fatalf("envelope parse: ok=%v code=%d msg=%q data=%s", envOK, envCode, envMsg, envData)
	}
	errData, errCode, errMsg, errOK := envelopeOrFlat([]byte(`{"code":10001,"msg":"login ing","data":null}`))
	if !errOK || errCode != 10001 || errMsg != "login ing" || errData != nil {
		t.Fatalf("envelope business error: ok=%v code=%d msg=%q data=%v", errOK, errCode, errMsg, errData)
	}
}
