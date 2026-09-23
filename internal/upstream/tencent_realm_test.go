// 全球域首条 system 前置（区域契约差异，SPEC §28.4 决策 C 补充）。
package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 全球域（.workbuddy.ai）：首条非 system 时自动前置一条，上游 11128 不再触发。
func TestTencentChatStreamGlobalRealmInjectsSystem(t *testing.T) {
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
	rc, err := c.ChatStream(context.Background(), "",
		[]ChatMessage{{Role: "user", Content: "hi"}}, "",
		SignCredential{SecurityToken: "t", Domain: "www.workbuddy.ai"}, "n", "m", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if !strings.Contains(gotBody, `"role":"system"`) {
		t.Fatalf("global realm must lead with a system message: %s", gotBody)
	}
	if !strings.HasPrefix(gotBody, `{"messages":[{"role":"system"`) {
		t.Fatalf("system message must be first: %s", gotBody)
	}
}

// 已带 system 首条：不重复注入。
func TestTencentChatStreamGlobalRealmKeepsExistingSystem(t *testing.T) {
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
	rc, err := c.ChatStream(context.Background(), "",
		[]ChatMessage{{Role: "system", Content: "custom"}, {Role: "user", Content: "hi"}}, "",
		SignCredential{SecurityToken: "t", Domain: "www.workbuddy.ai"}, "n", "m", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if n := strings.Count(gotBody, `"role":"system"`); n != 1 {
		t.Fatalf("existing system must not be duplicated (n=%d): %s", n, gotBody)
	}
	if !strings.Contains(gotBody, `"content":"custom"`) {
		t.Fatalf("client system prompt must be preserved verbatim: %s", gotBody)
	}
}

// 国内域：不注入（保持"模型所见 = 客户端所发"）。
func TestTencentChatStreamCNRealmNoInjection(t *testing.T) {
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
	rc, err := c.ChatStream(context.Background(), "",
		[]ChatMessage{{Role: "user", Content: "hi"}}, "",
		SignCredential{SecurityToken: "t", Domain: "www.codebuddy.cn"}, "n", "m", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if strings.Contains(gotBody, `"role":"system"`) {
		t.Fatalf("CN realm must not be modified: %s", gotBody)
	}
}

// ensureLeadingSystem 不改动调用方切片（避免上层复用 msgs 时被污染）。
func TestEnsureLeadingSystemDoesNotMutate(t *testing.T) {
	in := []ChatMessage{{Role: "user", Content: "hi"}}
	out := ensureLeadingSystem(in)
	if len(in) != 1 || in[0].Role != "user" {
		t.Fatalf("input slice mutated: %+v", in)
	}
	if len(out) != 2 || out[0].Role != "system" || out[1].Role != "user" {
		t.Fatalf("unexpected output: %+v", out)
	}
}
