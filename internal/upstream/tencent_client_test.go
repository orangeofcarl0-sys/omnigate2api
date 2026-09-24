// TencentClient 集成测试（SPEC §23.2）：假 copilot 服务器断言请求头/body、
// 429 分类与 refresh 通道。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeTencent 假 copilot 服务器：按路径分派 chat / refresh。
func fakeTencent(t *testing.T, chatHandler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/chat/completions":
			if chatHandler != nil {
				chatHandler(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		case "/v2/plugin/auth/token/refresh":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"accessToken":"new-token","refreshToken":"new-refresh","expiresIn":3600,"domain":"www.codebuddy.cn"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestTencentChatStreamHeadersAndBody(t *testing.T) {
	var gotBody string
	var gotAuthz, gotUID, gotDomain, gotProduct string
	srv := fakeTencent(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuthz = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotDomain = r.Header.Get("X-Domain")
		gotProduct = r.Header.Get("X-Product")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	})
	defer srv.Close()

	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	msgs := []ChatMessage{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "", ToolCalls: []ChatToolCall{{ID: "c1", Name: "Read", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "r"},
	}
	cred := SignCredential{SecurityToken: "tok", UserID: "u1", EnterpriseID: "e1", Domain: "www.codebuddy.cn"}
	rc, err := c.ChatStream(context.Background(), "", msgs, "", cred, "n", "glm-4.7", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc)

	if gotAuthz != "Bearer tok" || gotUID != "u1" || gotProduct != "SaaS" {
		t.Fatalf("headers authz=%q uid=%q product=%q", gotAuthz, gotUID, gotProduct)
	}
	if gotDomain != "www.codebuddy.cn" {
		t.Fatalf("domain=%q", gotDomain)
	}
	var body struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []any  `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body=%s err=%v", gotBody, err)
	}
	if body.Model != "glm-4.7" || !body.Stream {
		t.Fatalf("model/stream: %+v", body)
	}
	if len(body.Messages) != 3 || body.Messages[2].Role != "tool" || body.Messages[2].ToolCallID != "c1" {
		t.Fatalf("messages=%+v", body.Messages)
	}
	// 模型名不做映射（roles 上游按真实 ID 匹配，SPEC §23.2）
	if body.Model != "glm-4.7" {
		t.Fatalf("model must pass through unchanged: %s", body.Model)
	}
}

func TestTencentChatStreamToolsPassthrough(t *testing.T) {
	var gotBody string
	srv := fakeTencent(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "exec_command"}}}
	rc, err := c.ChatStream(context.Background(), "", []ChatMessage{{Role: "user", Content: "q"}}, "", SignCredential{SecurityToken: "t"}, "n", "m", tools, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if !strings.Contains(gotBody, `"exec_command"`) {
		t.Fatalf("tools must pass through: %s", gotBody)
	}
}

func TestTencentChatStreamErrorClassified(t *testing.T) {
	srv := fakeTencent(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	})
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	_, err := c.ChatStream(context.Background(), "", []ChatMessage{{Role: "user", Content: "q"}}, "", SignCredential{SecurityToken: "t"}, "n", "m", nil, "", nil)
	if err == nil {
		t.Fatal("429 must error")
	}
	var ae *ApiError
	if !errors.As(err, &ae) || ae.Status != 429 {
		t.Fatalf("must classify as ApiError 429: %v", err)
	}
}

func TestTencentRefreshToken(t *testing.T) {
	var gotRefresh, gotSource string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		gotRefresh = r.Header.Get("X-Refresh-Token")
		gotSource = r.Header.Get("X-Auth-Refresh-Source")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"accessToken":"new-token","refreshToken":"new-refresh","expiresIn":3600,"domain":"d"}`)
	}))
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	tok, err := c.RefreshToken(context.Background(), LoginConfig{}, "rt", "vv", "www.codebuddy.cn")
	if err != nil {
		t.Fatal(err)
	}
	if gotRefresh != "rt" || gotSource != "workbuddy" {
		t.Fatalf("headers refresh=%q source=%q", gotRefresh, gotSource)
	}
	if tok.Credentials.SecurityToken != "new-token" || tok.RefreshToken != "new-refresh" {
		t.Fatalf("tok=%+v", tok)
	}
	if tok.Credentials.Expiration == "" {
		t.Fatal("expiration must be set from expiresIn")
	}
}

func TestTencentRefreshNoToken(t *testing.T) {
	c := NewTencent(5 * time.Second)
	if _, err := c.RefreshToken(context.Background(), LoginConfig{}, "", "v", ""); err == nil {
		t.Fatal("empty refresh token must error")
	}
}
