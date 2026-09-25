// 面板腾讯 OAuth 集成（SPEC §24.3）：start 拿授权链接、poll pending→done、
// 成功落盘凭证并热加入账号池。
package server

import (
	"context"
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
	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
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
	// 登录成功必须触发账号初始化（额度快照/签到）——否则新账号要等下一个 Tick
	// 才有额度，当日已跑过的每日动作（按动作去重）更是要等次日。
	initCh := make(chan string, 4)
	h.cfg.AccountInit = func(ctx context.Context, acct *pool.Account) { initCh <- acct.UID }

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
	select {
	case got := <-initCh:
		if got != "u9" {
			t.Fatalf("init hook must receive the new account, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("login must trigger AccountInit (新账号初始化)")
	}
	// 契约：成功必须带 status=done + added（面板原来只认 status==="done"，腾讯响应
	// 却没有这个字段 → 成功被判成失败、state 已消费 → 之后每次重试都 "state expired"）
	if done["status"] != "done" || done["added"] != true {
		t.Fatalf("done poll contract: %v", done)
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
	// state 已消费：终端态（expired=true）而不是 4xx——面板据此停轮询并提示重开，
	// 否则只能靠 HTTP 报错文案猜，会一直空转在这个假错误上。
	st2 := post("/admin/api/oauth/tencent/poll", `{"state":"`+state+`"}`)
	if st2["ok"] != false || st2["expired"] != true {
		t.Fatalf("consumed state must be terminal: %v", st2)
	}
	if msg, _ := st2["message"].(string); !strings.Contains(msg, "发起授权") {
		t.Fatalf("expired message must tell the user what to do: %v", st2)
	}
	_ = time.Now()
}

// 同一 uid 再登录一次 = 更新凭证而不是新增账号（浏览器多半仍登录着同一账号，
// 授权被自动通过）。响应必须自证这一点，否则就是"显示成功但账号数没变"。
func TestOAuthTencentPanelFlowSameUIDUpdates(t *testing.T) {
	tencent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/auth/state"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"state":"S9","authUrl":"https://auth.example/flow"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/auth/token"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"accessToken":"at9","refreshToken":"rt9","expiresIn":7200,"domain":"www.codebuddy.cn"}}`)
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
	login := func() map[string]any {
		st := post("/admin/api/oauth/tencent/start", `{}`)
		state, _ := st["state"].(string)
		return post("/admin/api/oauth/tencent/poll", `{"state":"`+state+`"}`)
	}

	first := login()
	if first["ok"] != true || first["added"] != true {
		t.Fatalf("first login must be a new account: %v", first)
	}
	second := login()
	if second["ok"] != true || second["added"] != false {
		t.Fatalf("same uid must report update-in-place: %v", second)
	}
	if msg, _ := second["message"].(string); !strings.Contains(msg, "已在账号池中") {
		t.Fatalf("update message must explain the uid collision: %v", second)
	}
	// 仍是同一个账号，没有多出一个
	if n := len(p.List()); n != 1 {
		t.Fatalf("pool must hold exactly one account, got %d", n)
	}
}

// 面板腾讯登录的区域选择（HANDOFF §6.6）：realm=global 时 Origin 走国际域，
// 且凭证 domain 按所选区域落账（上游没回 domain 也不能落成相反区域——否则之后
// 所有请求都会被路由到另一个 base）。
func TestOAuthTencentPanelFlowGlobalRealm(t *testing.T) {
	var origins []string
	tencent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		origins = append(origins, r.Header.Get("Origin"))
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/auth/state"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"state":"SG","authUrl":"https://www.workbuddy.ai/login?x=1"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/auth/token"):
			// 故意不带 domain：区域只能由所选 realm 决定
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"accessToken":"at-g","refreshToken":"rt-g","expiresIn":3600}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/login/account"):
			_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{"uid":"ug9","enterpriseId":"eg9","nickname":"nick-g"}}`)
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

	st := post("/admin/api/oauth/tencent/start", `{"realm":"global"}`)
	if st["ok"] != true || st["realm"] != "global" || !strings.HasPrefix(st["auth_url"].(string), "https://www.workbuddy.ai/") {
		t.Fatalf("start(global): %v", st)
	}
	state := st["state"].(string)
	done := post("/admin/api/oauth/tencent/poll", `{"state":"`+state+`"}`)
	if done["ok"] != true || done["realm"] != "global" || done["uid"] != "ug9" {
		t.Fatalf("poll(global): %v", done)
	}
	// 三段都带国际域 Origin
	for i, o := range origins {
		if o != "https://www.workbuddy.ai" {
			t.Fatalf("request %d origin=%q want global", i, o)
		}
	}
	// 凭证落盘：domain 按所选区域补齐（TencentRegion 据此分流后续所有请求）
	files, _ := filepath.Glob(filepath.Join(authDir, "workbuddy-ug9.json"))
	if len(files) != 1 {
		t.Fatalf("credential not saved: %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	var saved struct {
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Domain != "www.workbuddy.ai" {
		t.Fatalf("domain must follow realm: %q err=%v", saved.Domain, err)
	}
	acct := p.Get("ug9")
	if acct == nil || !upstream.TencentRegion(acct.Auth.Domain) {
		t.Fatalf("pooled account must be routed global: %+v", acct)
	}
}
