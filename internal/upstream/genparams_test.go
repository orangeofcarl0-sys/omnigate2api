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

// 华为 max_tokens 硬上限钳制（2026-09-26 实测）：上游广告 131072/393216，但其 API
// 只接受 ≤65536（65536 ✓ / 65537 ✗ → InferHub.001001005），照广告值下发必得 400。
func TestClampCodeartsMaxTokens(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		hasKey  bool
		clamped bool
	}{
		{"超限 float64（入站 JSON 形态）", float64(131072), true, true},
		{"超限 int64", int64(393216), true, true},
		{"超限 int", int(70000), true, true},
		{"恰好等于上限", float64(65536), true, false},
		{"未超限", float64(4096), true, false},
		{"无该键", nil, false, false},
		{"非数值（客户端传字符串，钳制不了）", "131072", true, false},
	}
	for _, c := range cases {
		body := map[string]any{"model": "glm-5.3-flash"}
		if c.hasKey {
			body["max_tokens"] = c.in
		}
		from, to, clamped := clampCodeartsMaxTokens(body)
		if clamped != c.clamped {
			t.Fatalf("%s: clamped=%v want %v", c.name, clamped, c.clamped)
		}
		if !c.hasKey {
			if _, exists := body["max_tokens"]; exists {
				t.Fatalf("%s: must not add the key", c.name)
			}
			continue
		}
		if !c.clamped {
			if body["max_tokens"] != c.in {
				t.Fatalf("%s: untouched value must stay %v, got %v", c.name, c.in, body["max_tokens"])
			}
			continue
		}
		got, ok := toInt64(body["max_tokens"])
		orig, okIn := toInt64(c.in)
		if !ok || !okIn {
			t.Fatalf("%s: clamped value not numeric: %v / %v", c.name, body["max_tokens"], c.in)
		}
		if got != CodeartsMaxOutputTokens || to != CodeartsMaxOutputTokens || from != orig {
			t.Fatalf("%s: from=%d to=%d body=%d want %d", c.name, from, to, got, CodeartsMaxOutputTokens)
		}
	}
}
