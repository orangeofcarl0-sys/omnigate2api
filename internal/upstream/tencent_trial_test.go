// 国际版账号试用自动激活（SPEC §28.6）。
// 形状来自官方登录前端的 billing chunk：register → 等 1.5s → trial（两段都幂等）。
package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omnigate2api/internal/auth"
)

func trialUpstream(t *testing.T, registerCode int, registerMsg string, trialBody string, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = append(*seen, r.Method+" "+r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/auth/realms/copilot/overseas/user/register"):
			if r.URL.Query().Get("userId") == "" {
				t.Error("register 必须带 userId")
			}
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
				t.Errorf("register 必须带 Bearer: %q", got)
			}
			_, _ = io.WriteString(w, `{"code":`+itoa(registerCode)+`,"msg":"`+registerMsg+`"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/billing/ide/trial":
			_, _ = io.WriteString(w, trialBody)
		case r.Method == http.MethodPost && r.URL.Path == "/console/login/account":
			b, _ := io.ReadAll(r.Body)
			var payload struct {
				Attributes map[string][]string `json:"attributes"`
			}
			if err := json.Unmarshal(b, &payload); err != nil || len(payload.Attributes["countryCode"]) == 0 {
				t.Errorf("地区补写形状不对: %s", string(b))
			}
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func trialTestAccount() *auth.Auth {
	return &auth.Auth{UserID: "uid-test", UserName: "wb", Profile: "workbuddy", Domain: "www.workbuddy.ai", CloudDragonTok: "tok"}
}

// 主路径：register → trial 两步都被打到，结果判定为"已开通/已领"。
func TestEnsureGlobalTrialHappyPath(t *testing.T) {
	trialGrace = 0
	defer func() { trialGrace = trialRegisterGrace }()
	var seen []string
	srv := trialUpstream(t, 200, "register success", `{"code":14051,"msg":"has applied trial"}`, &seen)
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)

	c := NewTencent(5e9)
	act, err := c.EnsureGlobalTrial(trialTestAccount())
	if err != nil {
		t.Fatalf("已开通的账号不该报错: %v", err)
	}
	if !act.OK() || !act.AlreadyHad {
		t.Fatalf("14051 应判为「早就领过」：%+v", act)
	}
	if len(seen) != 2 || !strings.Contains(seen[0], "user/register") || !strings.Contains(seen[1], "ide/trial") {
		t.Fatalf("必须先 register 再 trial: %v", seen)
	}
}

// 上游报"要补地区"：应写默认国家后重试（写地区 + 再 register + trial）。
func TestEnsureGlobalTrialSetsRegionWhenAsked(t *testing.T) {
	trialGrace = 0
	defer func() { trialGrace = trialRegisterGrace }()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "user/register"):
			// 第一次要求补地区，补完即成功
			n := 0
			for _, s := range seen {
				if strings.Contains(s, "user/register") {
					n++
				}
			}
			if n == 1 {
				_, _ = io.WriteString(w, `{"code":500,"msg":"region required"}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":200,"msg":"register success"}`)
		case r.URL.Path == "/billing/ide/trial":
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok"}`)
		case r.URL.Path == "/console/login/account":
			_, _ = io.WriteString(w, `{"code":0,"msg":"ok"}`)
		}
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)

	c := NewTencent(5e9)
	act, err := c.EnsureGlobalTrial(trialTestAccount())
	if err != nil {
		t.Fatalf("补地区后应成功: %v", err)
	}
	if !act.RegionSet || !act.OK() {
		t.Fatalf("应记录补过地区且开通成功: %+v", act)
	}
	joined := strings.Join(seen, ",")
	if !strings.Contains(joined, "/console/login/account") {
		t.Fatalf("应调用地区补写端点: %v", seen)
	}
	if strings.Count(joined, "user/register") != 2 {
		t.Fatalf("补地区后应重跑 register: %v", seen)
	}
}

// 华为账号没有这条通道：直接跳过，不发任何请求。
func TestEnsureGlobalTrialSkipsNonGlobal(t *testing.T) {
	trialGrace = 0
	defer func() { trialGrace = trialRegisterGrace }()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)

	c := NewTencent(5e9)
	act, err := c.EnsureGlobalTrial(&auth.Auth{UserID: "hid", Profile: "codearts", Domain: "codearts.huaweicloud.com", CloudDragonTok: "tok"})
	if err != nil || act == nil || !act.Skipped {
		t.Fatalf("非国际版应跳过: %+v %v", act, err)
	}
	if called {
		t.Fatal("跳过的账号不该发任何请求")
	}
}

// register 直接失败：错误要带出端点与原始响应（排障）。
func TestEnsureGlobalTrialRegisterFailure(t *testing.T) {
	trialGrace = 0
	defer func() { trialGrace = trialRegisterGrace }()
	srv := trialUpstream(t, 403, "forbidden", `{"code":14051}`, nil)
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)

	c := NewTencent(5e9)
	act, err := c.EnsureGlobalTrial(trialTestAccount())
	if err == nil {
		t.Fatal("register 失败必须报错")
	}
	if act == nil || act.RegisterCode != 403 {
		t.Fatalf("应带回业务码: %+v", act)
	}
	if !strings.Contains(err.Error(), "user/register") {
		t.Fatalf("错误里要点出端点: %v", err)
	}
}

// 默认国家（可用 OMNIGATE_TRIAL_COUNTRY 覆盖）：与我方全球账号实证一致 = 新加坡。
func TestTrialCountryDefault(t *testing.T) {
	t.Setenv("OMNIGATE_TRIAL_COUNTRY", "")
	cc, full, local := trialCountry()
	if cc != "SG" || full != "Singapore" || local != "新加坡" {
		t.Fatalf("默认国家应为新加坡: %s/%s/%s", cc, full, local)
	}
	t.Setenv("OMNIGATE_TRIAL_COUNTRY", "US:United States:美国")
	cc, full, local = trialCountry()
	if cc != "US" || full != "United States" || local != "美国" {
		t.Fatalf("应支持覆盖: %s/%s/%s", cc, full, local)
	}
}
