// 多模态媒体测试（SPEC §30 阶段 2 验收）：SSRF 黑名单表、抓取转换、
// passthrough/placeholder 渲染分叉、roles 分片线格式、指纹参与、env 覆盖。
package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omnigate2api/internal/adapt"
	"omnigate2api/internal/upstream"
)

// TestForbiddenIPTable SPEC §30.4：SSRF 地址黑名单全表。
func TestForbiddenIPTable(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":     true,  // 环回
		"10.1.2.3":      true,  // RFC1918
		"172.16.0.9":    true,  // RFC1918
		"192.168.1.1":   true,  // RFC1918
		"169.254.1.1":   true,  // 链路本地
		"0.0.0.0":       true,  // 未指定
		"224.0.0.1":     true,  // 组播
		"::1":           true,  // 环回 v6
		"fc00::1":       true,  // ULA
		"fe80::1":       true,  // 链路本地 v6
		"8.8.8.8":       false, // 公网
		"114.114.114.5": false, // 公网
		"2606:4700::1":  false, // 公网 v6
	}
	for ip, want := range cases {
		if got := forbiddenIP(net.ParseIP(ip)); got != want {
			t.Fatalf("forbiddenIP(%s)=%v want %v", ip, got, want)
		}
	}
}

// TestGuardedFetchRejectsLoopback 守卫抓取对环回地址（httptest 监听点）直接拒绝。
func TestGuardedFetchRejectsLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("guard must reject before any request reaches the loopback server")
	}))
	defer srv.Close()
	if _, merr := guardedImageFetch(srv.URL + "/x.png"); merr != mErrPrivate {
		t.Fatalf("err=%v want %v", merr, mErrPrivate)
	}
}

// TestImageFetchConvert 抓取转换全分支（guard 关闭以触及环回 httptest）。
func TestImageFetchConvert(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfakepayload")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(png)
		case "/redirect.png":
			http.Redirect(w, r, srv.URL+"/ok.png", http.StatusFound)
		case "/text":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html/>"))
		case "/big.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(make([]byte, fetchBodyMax+1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	fetch := func(u string) (string, mediaErr) { return httpImageFetch(u, false) }

	// 成功 → data URI
	uri, merr := fetch(srv.URL + "/ok.png")
	if merr != "" || !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("convert: uri=%q err=%v", uri, merr)
	}
	// 重定向拒绝 / 非图片 / 超限
	for tc, want := range map[string]mediaErr{
		"/redirect.png": mErrRedirect,
		"/text":         mErrNonImage,
		"/big.png":      mErrTooLarge,
		"/missing.png":  mErrUnreachable,
	} {
		if _, merr := fetch(srv.URL + tc); merr != want {
			t.Fatalf("%s: err=%v want %v", tc, merr, want)
		}
	}
	// 非法 scheme
	if _, merr := fetch("ftp://example.com/x"); merr != mErrBadURL {
		t.Fatalf("scheme: err=%v", merr)
	}
}

// TestRenderMediaPassthrough SPEC §30.5：转换成功保留结构化、失败降级占位、
// Deferred 占位；data URI 直通不抓取。
func TestRenderMediaPassthrough(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "user", Text: "look", Images: []imagePart{
			{URL: "http://x.example/a.png"},
			{URL: "data:image/png;base64,aGk="},
		}, Deferred: []deferredMedia{{Type: "file"}, {Fixed: deferredOversize}}},
	}
	fetch := func(u string) (string, mediaErr) {
		if strings.HasSuffix(u, "a.png") {
			return "data:image/png;base64,YQ==", ""
		}
		return "", mErrPrivate
	}
	ph := mediaTemplate(nil)
	msgs[0].Images = append([]imagePart{{URL: "http://x.example/broken.png"}}, msgs[0].Images...)
	st := renderMedia(msgs, "passthrough", ph, fetch)
	if st.Converted != 1 || st.Failed != 1 || st.Deferred != 2 || st.Images != 3 {
		t.Fatalf("stats=%+v", st)
	}
	m := msgs[0]
	if len(m.Images) != 2 || m.Images[0].URL != "data:image/png;base64,YQ==" || m.Images[1].URL != "data:image/png;base64,aGk=" {
		t.Fatalf("kept=%+v", m.Images)
	}
	for _, want := range []string{"look", "[图片抓取失败：私网拒绝]", "[用户发送了一个附件：file]", deferredOversize} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("text missing %q: %q", want, m.Text)
		}
	}
	if len(m.Deferred) != 0 {
		t.Fatal("deferred must clear after render")
	}
}

// TestRolesContentPartsMarshal SPEC §30.5：无分片 string content（零回归）、
// 有分片数组 content（文本合首片）。
func TestRolesContentPartsMarshal(t *testing.T) {
	plain, _ := json.Marshal(upstream.ChatMessage{Role: "user", Content: "hi"})
	if !strings.Contains(string(plain), `"content":"hi"`) {
		t.Fatalf("plain=%s", plain)
	}
	withImg, _ := json.Marshal(upstream.ChatMessage{
		Role:    "user",
		Content: "look",
		ContentParts: []upstream.ChatContentPart{{
			Type:     "image_url",
			ImageURL: &upstream.ChatImageURL{URL: "data:image/png;base64,aGk=", Detail: "low"},
		}},
	})
	var parsed struct {
		Role    string           `json:"role"`
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(withImg, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Content) != 2 {
		t.Fatalf("parts=%s", withImg)
	}
	if parsed.Content[0]["type"] != "text" || parsed.Content[0]["text"] != "look" {
		t.Fatalf("text part=%v", parsed.Content[0])
	}
	iu := parsed.Content[1]["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,aGk=" || iu["detail"] != "low" {
		t.Fatalf("image part=%v", parsed.Content[1])
	}
}

// TestFingerprintImages SPEC §30.6：同图 → 哈希链一致（可续接），改图 → 变化。
func TestFingerprintImages(t *testing.T) {
	base := []openAIMessage{{Role: "user", Text: "look"}}
	a := append(base, openAIMessage{Role: "user", Text: "again", Images: []imagePart{{URL: "data:image/png;base64,aGk="}}})
	b := append(base, openAIMessage{Role: "user", Text: "again", Images: []imagePart{{URL: "data:image/png;base64,aGk="}}})
	c := append(base, openAIMessage{Role: "user", Text: "again", Images: []imagePart{{URL: "data:image/png;base64,Zg=="}}})
	fa, fb, fc := fingerprint(a), fingerprint(b), fingerprint(c)
	if fa[1] != fb[1] {
		t.Fatal("same images must produce identical chain")
	}
	if fa[1] == fc[1] {
		t.Fatal("different images must diverge")
	}
	// 纯文本消息哈希不受新增 images 字段影响（无图时行为零回归）
	if fingerprint(base)[0] != fingerprint([]openAIMessage{{Role: "user", Text: "look"}})[0] {
		t.Fatal("text-only fingerprint changed")
	}
}

// TestMediaModeOverride SPEC §30.2：env 覆盖优先，缺省 placeholder。
func TestMediaModeOverride(t *testing.T) {
	passthroughProfile := &adapt.UpstreamProfile{Message: adapt.MessageProfile{Model: "roles", Media: "passthrough"}}
	placeholderProfile := &adapt.UpstreamProfile{Message: adapt.MessageProfile{Model: "roles"}}
	if got := mediaMode(passthroughProfile, ""); got != "passthrough" {
		t.Fatalf("declare: %s", got)
	}
	if got := mediaMode(passthroughProfile, "placeholder"); got != "placeholder" {
		t.Fatalf("env override: %s", got)
	}
	if got := mediaMode(placeholderProfile, ""); got != "placeholder" {
		t.Fatalf("default: %s", got)
	}
}

// TestMediaPassthroughValidation SPEC §30.2：passthrough 仅限 roles（fail-fast）。
func TestMediaPassthroughValidation(t *testing.T) {
	p := adapt.Workbuddy
	p.Message.Media = "passthrough"
	if err := p.Validate(); err != nil {
		t.Fatalf("roles+passthrough must be valid: %v", err)
	}
	c := *textOnlyTestProfile()
	c.Message.Media = "passthrough"
	if err := c.Validate(); err == nil {
		t.Fatal("text-only+passthrough must fail validation")
	}
	bad := adapt.Workbuddy
	bad.Message.Media = "none"
	if err := bad.Validate(); err == nil {
		t.Fatal("media=none must fail validation (枚举已删除)")
	}
}

// TestCountTokensImages SPEC §30.6：图片按固定近似 +1000 tok/张。
func TestCountTokensImages(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "user", Text: "hello"},
		{Role: "user", Text: "look", Images: []imagePart{{URL: "data:image/png;base64,aGk="}, {URL: "https://x/y.png"}}},
	}
	want := tokensApprox("hello") + tokensApprox("look") + 2*approxTokensPerImage
	if got := anthropicCountTokensEstim(msgs); got != want {
		t.Fatalf("count=%d want %d", got, want)
	}
}

// TestRolesRenderMediaPassthrough 端到端：passthrough 渲染后 roles 渲染产出分片数组。
func TestRolesRenderMediaPassthrough(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "user", Text: "look", Images: []imagePart{{URL: "data:image/png;base64,aGk=", Detail: "auto"}}},
	}
	renderMedia(msgs, "passthrough", mediaTemplate(nil), nil)
	up := renderRolesMessages(msgs)
	if len(up) != 1 || len(up[0].ContentParts) != 1 {
		t.Fatalf("parts=%+v", up)
	}
	raw, _ := json.Marshal(up[0])
	if !strings.Contains(string(raw), `"content":[{"type":"text","text":"look"},{"type":"image_url"`) {
		t.Fatalf("wire=%s", raw)
	}
	// 无图消息零回归：string content
	plain := renderRolesMessages([]openAIMessage{{Role: "user", Text: "hi"}})
	praw, _ := json.Marshal(plain[0])
	if !strings.Contains(string(praw), `"content":"hi"`) {
		t.Fatalf("plain wire=%s", praw)
	}
}

// TestAnthropicToolResultImages SPEC §30.2 拍板：tool_result 图片同机制结构化。
func TestAnthropicToolResultImages(t *testing.T) {
	body := `{"max_tokens":100,"messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"tu_1","content":[
			{"type":"text","text":"screenshot done"},
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"aGk="}}
		]}
	]}]}`
	req, err := parseAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var tool *openAIMessage
	for i := range req.Messages {
		if req.Messages[i].Role == "tool" {
			tool = &req.Messages[i]
		}
	}
	if tool == nil {
		t.Fatal("tool message missing")
	}
	if !strings.Contains(tool.Text, "screenshot done") || len(tool.Images) != 1 ||
		tool.Images[0].URL != "data:image/jpeg;base64,aGk=" {
		t.Fatalf("tool media=%+v text=%q", tool.Images, tool.Text)
	}
}
