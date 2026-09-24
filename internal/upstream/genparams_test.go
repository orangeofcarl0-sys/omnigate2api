// 生成参数并入上游 body 的规矩（SPEC §23.1）：gen 铺底、权威字段覆盖——
// 客户端不能借 gen 篡改 model/stream/messages（路由与流形态必须由网关决定）。
package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestApplyGenCannotOverrideAuthoritativeFields(t *testing.T) {
	var got map[string]any
	srv := fakeTencent(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL

	// 恶意/异常的 gen：试图覆盖权威字段
	gen := map[string]any{
		"model": "attacker-model", "stream": false, "messages": []any{"pwned"},
		"temperature": 0.3, "max_tokens": 64,
	}
	rc, err := c.ChatStream(context.Background(), "",
		[]ChatMessage{{Role: "user", Content: "hi"}}, "",
		SignCredential{SecurityToken: "t"}, "n", "real-model", nil, "", gen)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	if got["model"] != "real-model" {
		t.Fatalf("gen 不得覆盖 model：%v", got["model"])
	}
	if got["stream"] != true {
		t.Fatalf("gen 不得覆盖 stream：%v", got["stream"])
	}
	if _, isArr := got["messages"].([]any); !isArr || len(got["messages"].([]any)) != 1 {
		t.Fatalf("gen 不得覆盖 messages：%v", got["messages"])
	}
	// 真参数照常并入
	if got["temperature"] != 0.3 || got["max_tokens"] != float64(64) {
		t.Fatalf("生成参数应并入 body：temperature=%v max_tokens=%v", got["temperature"], got["max_tokens"])
	}
}

// gen 为空/nil 时 body 形态与既有完全一致（零回归）。
func TestApplyGenEmptyKeepsBodyShape(t *testing.T) {
	var got map[string]any
	srv := fakeTencent(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	rc, err := c.ChatStream(context.Background(), "",
		[]ChatMessage{{Role: "user", Content: "hi"}}, "",
		SignCredential{SecurityToken: "t"}, "n", "m", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	for k := range got {
		switch k {
		case "model", "stream", "messages":
		default:
			t.Fatalf("空 gen 不应引入额外字段，出现 %q", k)
		}
	}
}
