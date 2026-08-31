// login-tencent 腾讯（WorkBuddy/CodeBuddy）OAuth 设备流登录（SPEC §24.3）。
//
// 两段式：
//
//	login-tencent url   → 发起授权，打印 authUrl（浏览器打开）
//	login-tencent poll  → 轮询 token（state 在 /tmp 落盘），成功后取账号信息
//	                      落盘 auths/workbuddy-{uid}.json（命名空间前缀）
//
// 端点/流程对齐参考实现（Sliverkiss workbuddy2api cmd/login）。base 可用
// OMNIGATE_TENCENT_BASE 覆盖（与 TencentClient 同一测试/实验通道）；头保真与
// 8e G3 对齐（官方 CLI UA）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

const upstreamBaseCN = "https://copilot.tencent.com"

// upstreamBase 返回登录 base：OMNIGATE_TENCENT_BASE 覆盖（测试/实验），缺省 CN。
func upstreamBase() string {
	if v := os.Getenv("OMNIGATE_TENCENT_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return upstreamBaseCN
}

// envelopeOrFlat 解析登录端点响应：腾讯侧为 {code,msg,data:{...}} envelope
// （8f 真链路实证：auth/state 即此形态，与 refresh 同构）；data 缺省且 code==0
// 时回退扁平结构（兼容参考实现派生夹具）。返回 (data 片段, code, msg, 是否 envelope)。
func envelopeOrFlat(raw []byte) (json.RawMessage, int64, string, bool) {
	var env struct {
		Code int64           `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) == nil {
		if len(env.Data) > 0 && string(env.Data) != "null" {
			return env.Data, env.Code, env.Msg, true
		}
		if env.Code != 0 {
			return nil, env.Code, env.Msg, true
		}
	}
	return nil, 0, "", false
}

const defaultStateFile = "/tmp/login-tencent-state.json"

var client = &http.Client{Timeout: 30 * time.Second}

func main() {
	authDir := flag.String("auth-dir", "auths", "auth output dir (default ./auths)")
	stateFile := flag.String("state-file", defaultStateFile, "state file")
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fatal("usage: login-tencent <url|poll> [-auth-dir DIR]")
	}

	// 独立 cookie jar：多账号登录互不串会话
	jar, _ := cookiejar.New(nil)
	client.Jar = jar

	switch args[0] {
	case "url":
		if err := runURL(*stateFile); err != nil {
			fatal("%v", err)
		}
	case "poll":
		if err := runPoll(*authDir, *stateFile); err != nil {
			fatal("%v", err)
		}
	default:
		fatal("unknown subcommand %q (want url|poll)", args[0])
	}
}

// runURL 发起授权：请求 state，落盘 state 文件，打印 authUrl。
func runURL(stateFile string) error {
	resp, err := client.Post(upstreamBase()+"/v2/plugin/auth/state?platform=CLI", "application/json", strings.NewReader("{}"))
	if err != nil {
		return fmt.Errorf("auth state failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("auth state http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	data, code, msg, envOK := envelopeOrFlat(raw)
	if envOK && code != 0 {
		if msg == "" {
			msg = "business error"
		}
		return fmt.Errorf("auth state business error code=%d msg=%s", code, truncate(msg, 200))
	}
	if envOK && data != nil {
		_ = json.Unmarshal(data, &st)
	} else {
		_ = json.Unmarshal(raw, &st)
	}
	if st.State == "" || st.AuthURL == "" {
		return fmt.Errorf("auth state: missing state or authUrl: %s", truncate(string(raw), 200))
	}
	if err := os.WriteFile(stateFile, []byte(`{"state":"`+st.State+`"}`), 0o600); err != nil {
		return fmt.Errorf("write state: %v", err)
	}
	fmt.Println(st.AuthURL)
	fmt.Println("授权后运行: login-tencent poll")
	return nil
}

func runPoll(authDir, stateFile string) error {
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		return fmt.Errorf("read state: %v (先跑 login-tencent url)", err)
	}
	var ls struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil || ls.State == "" {
		return fmt.Errorf("parse state: %v", err)
	}

	// token 端点：pending 时业务 code 非 0 / 无 accessToken
	tokRaw, status, errTok := doJSON(http.MethodGet, upstreamBase()+"/v2/plugin/auth/token?state="+ls.State, nil)
	if errTok != nil || status >= 400 {
		return fmt.Errorf("登录未完成（waiting for login）。请确认已在浏览器完成授权（err=%v status=%d）", errTok, status)
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	tokData, tokCode, tokMsg, tokEnv := envelopeOrFlat(tokRaw)
	if tokEnv && tokCode != 0 {
		// pending：业务 code 非 0（如 10001 "login ing"）
		if tokMsg == "" {
			tokMsg = "business error"
		}
		return fmt.Errorf("登录未完成（waiting for login）。请确认已在浏览器完成授权（code=%d msg=%s）", tokCode, truncate(tokMsg, 200))
	}
	if tokEnv && tokData != nil {
		_ = json.Unmarshal(tokData, &tok)
	} else {
		_ = json.Unmarshal(tokRaw, &tok)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("登录未完成（waiting for login）。请确认已在浏览器完成授权")
	}

	// 账号信息（带 Bearer）
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctRaw, _, errAcct := doJSON(http.MethodGet, upstreamBase()+"/v2/plugin/login/account?state="+ls.State, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	})
	if errAcct == nil {
		if ad, acCode, acMsg, acEnv := envelopeOrFlat(acctRaw); acEnv && acCode != 0 {
			if acMsg == "" {
				acMsg = "business error"
			}
			return fmt.Errorf("login/account business error code=%d msg=%s", acCode, truncate(acMsg, 200))
		} else if acEnv && ad != nil {
			_ = json.Unmarshal(ad, &acct)
		} else {
			_ = json.Unmarshal(acctRaw, &acct)
		}
	}
	if acct.UID == "" {
		return fmt.Errorf("login/account: missing uid")
	}

	a := &auth.Auth{
		UserID:         acct.UID,
		UserName:       acct.Nickname,
		Profile:        "workbuddy",
		CloudDragonTok: tok.AccessToken,
		RefreshToken:   tok.RefreshToken,
		EnterpriseID:   acct.EnterpriseID,
		Domain:         tok.Domain,
		UpdatedAt:      time.Now().Unix(),
	}
	if tok.ExpiresIn > 0 {
		a.Expiration = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	if err := auth.SaveNew(authDir, a); err != nil {
		return fmt.Errorf("save auth: %v", err)
	}
	_ = os.Remove(stateFile)
	fmt.Printf("workbuddy login ok: uid=%s nickname=%s -> %s\n", acct.UID, acct.Nickname,
		filepath.Join(authDir, a.FileName()))
	fmt.Println("重启服务（docker compose restart）后生效")
	return nil
}

// doJSON 简单 GET/POST JSON 请求（无第三方依赖）。
func doJSON(method, url string, decorate func(*http.Request)) ([]byte, int, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", "https://www.codebuddy.cn")
	req.Header.Set("User-Agent", upstream.TencentClientUA)
	if decorate != nil {
		decorate(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return raw, resp.StatusCode, fmt.Errorf("http %d", resp.StatusCode)
	}
	return raw, resp.StatusCode, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[错误] "+format+"\n", args...)
	os.Exit(1)
}
