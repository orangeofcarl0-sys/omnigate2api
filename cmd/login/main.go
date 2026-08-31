// login 工具：华为云 CodeArts OAuth2（PKCE）登录 → 保存 auths/codearts-{user_id}.json。
//
// 流程（对齐 huaweicloud.authentication 扩展）：
//  1. 本地起 127.0.0.1 回调服务
//  2. 生成 ticket_id/secret + PKCE，构造 codearts.huaweicloud.com/authorize 链接
//  3. 浏览器登录 → 本地回调收 authorization code，或轮询 snap-manager /v1/login/ticket
//  4. oauth2/tokens 换 STS 临时 AK/SK + security_token + refresh_token → 落盘
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth output dir")
	printOnly := flag.Bool("print-only", false, "server mode: print login link and poll (no local browser)")
	clientID := flag.String("client-id", upstream.CLIENT_ID, "OAuth client id (uri scheme)")
	flag.Parse()

	cfg := upstream.DefaultLoginConfig()
	if *clientID != "" {
		cfg.ClientID = *clientID
	}
	client := upstream.New(60 * time.Second)
	ctx := context.Background()

	// 1. 本地回调服务
	var ln net.Listener
	var err error
	callbackPort := 0
	codeCh := make(chan string, 1)
	if !*printOnly {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("loopback listen: %v", err)
		}
		callbackPort = ln.Addr().(*net.TCPAddr).Port
		go serveCallback(ln, cfg.RedirectPath, codeCh)
		defer ln.Close()
	}

	// 2. 生成 ticket/secret + PKCE
	ticketID, err := upstream.RandomHex(16)
	if err != nil {
		log.Fatalf("gen ticket: %v", err)
	}
	secret, err := upstream.RandomHex(16)
	if err != nil {
		log.Fatalf("gen secret: %v", err)
	}
	verifier, challenge, err := upstream.PKCE()
	if err != nil {
		log.Fatalf("gen pkce: %v", err)
	}

	loginURL := client.BuildAuthorizeURL(cfg, ticketID, challenge, "S256", callbackPort)
	fmt.Println("============================================================")
	fmt.Println("  CodeArts Agent 登录（华为云账号）")
	fmt.Println("============================================================")
	fmt.Println("步骤：")
	fmt.Println("  1. 打开下面链接，用华为云账号完成登录")
	fmt.Println("  2. 登录成功后浏览器会跳回 127.0.0.1（服务器模式跳转失败可忽略）")
	fmt.Println("  3. 本工具自动换取 STS 临时凭证并落盘 auths/")
	fmt.Println("")
	fmt.Println("登录链接：")
	fmt.Println("  " + loginURL)
	fmt.Println("")

	if !*printOnly {
		_ = openBrowser(loginURL)
	}

	// 3. 双通道：回调 code vs ticket 轮询
	var tok *upstream.TokenResponse
	ticker := time.NewTicker(2 * time.Second)
	timer := time.NewTimer(5 * time.Minute)
	defer ticker.Stop()
	defer timer.Stop()
loginLoop:
	for {
		select {
		case <-timer.C:
			log.Fatalf("登录超时（5 分钟）")
		case code := <-codeCh:
			tok, err = client.ExchangeCode(ctx, cfg, code, verifier, callbackPort)
			if err != nil {
				log.Fatalf("exchange code: %v", err)
			}
			break loginLoop
		case <-ticker.C:
			t, terr := client.PollTicket(ctx, cfg, ticketID, secret)
			if terr == nil && t != nil && t.UserName != "" {
				tok = t
				break loginLoop
			}
		}
	}

	// 4. 落盘
	if os.Getenv("OMNIGATE_LOGIN_DEBUG") != "" {
		// 双通道汇合后的裁决点：ticket 轮询与授权码两条通道的 TokenResponse 都在此。
		// 只打长度不打值——token 前缀片段同样是凭证泄露面（发布安全审查 F2）。
		fmt.Printf("[debug] refresh_token len=%d token_len=%d\n", len(tok.RefreshToken), len(tok.Credentials.SecurityToken))
	}
	cred := tok.Credentials
	a := auth.New(tok.UserID, tok.UserName, tok.DomainID,
		cred.SecurityToken, cred.AccessKeyID, cred.SecretAccessKey,
		cred.Expiration, tok.RefreshToken, verifier)
	if err := auth.SaveNew(*authDir, a); err != nil {
		log.Fatalf("save auth: %v", err)
	}
	fmt.Printf("\n✅ 登录成功：user_id=%s name=%s\n", tok.UserID, tok.UserName)
	fmt.Printf("凭证已保存：%s\n", filepath.Join(*authDir, a.FileName()))
	if cred.Expiration != "" {
		fmt.Printf("STS 有效期至：%s（到期前自动 refresh 续期）\n", cred.Expiration)
	}
}

// serveCallback 处理本地回调（GET query: secret + code）。
func serveCallback(ln net.Listener, redirectPath string, ch chan<- string) {
	mux := http.NewServeMux()
	mux.HandleFunc(redirectPath, func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			// 兼容 POST body
			body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			vals, _ := url.ParseQuery(string(body))
			code = vals.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if code == "" {
			// 二段握手：门户登录完成后先回调 secret+redirect（无 code）。
			// 把浏览器 302 回 redirect 指向的门户页（带用户会话 cookie），
			// 门户据此将 ticket 标记为已授权（authorizationAvailable→true），
			// 随后回调本监听下发 code；ticket 轮询通道亦随之放行。
			if rurl := r.URL.Query().Get("redirect"); rurl != "" {
				if u, err := url.Parse(rurl); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
					http.Redirect(w, r, rurl, http.StatusFound)
					return
				}
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<h3>登录失败：缺少 code</h3>"))
			return
		}
		_, _ = w.Write([]byte("<h3>登录成功，可关闭此页面。</h3>"))
		select {
		case ch <- code:
		default:
		}
	})
	_ = http.Serve(ln, mux)
}

func openBrowser(rawurl string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawurl)
	case "darwin":
		cmd = exec.Command("open", rawurl)
	default:
		cmd = exec.Command("xdg-open", rawurl)
	}
	return cmd.Start()
}
