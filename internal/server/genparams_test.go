// 客户端生成参数透传（SPEC §23.1/§13.2）：早前上游 body 只拼 model/stream/messages/tools，
// 客户端设的 temperature/max_tokens/stop 等**全部静默失效**——本组用例把这个行为钉住。
package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"omnigate2api/internal/auth"
)

// parseGenParams：白名单过滤 + 别名归一 + 空值剔除 + 别名优先级。
func TestParseGenParams(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[string]any
	}{
		{"标准参数透传", `{"temperature":0.2,"top_p":0.9,"max_tokens":64,"stop":["x"],"seed":7}`,
			map[string]any{"temperature": 0.2, "top_p": 0.9, "max_tokens": float64(64), "stop": []any{"x"}, "seed": float64(7)}},
		{"max_completion_tokens 归一到 max_tokens", `{"max_completion_tokens":32}`,
			map[string]any{"max_tokens": float64(32)}},
		{"responses 的 max_output_tokens 同归一", `{"max_output_tokens":48}`,
			map[string]any{"max_tokens": float64(48)}},
		{"anthropic 的 stop_sequences 归一到 stop", `{"stop_sequences":["end"]}`,
			map[string]any{"stop": []any{"end"}}},
		{"别名并存时 max_tokens 优先（不随 map 顺序）", `{"max_completion_tokens":32,"max_tokens":64}`,
			map[string]any{"max_tokens": float64(64)}},
		{"空串与 null 视为未指定", `{"temperature":"","top_p":null}`, nil},
		{"白名单外的键一律丢弃", `{"n":3,"user":"u","logit_bias":{"1":2},"temperature":0.5}`,
			map[string]any{"temperature": 0.5}},
		{"全是非参数键则返回 nil", `{"messages":[]}`, nil},
	}
	for _, c := range cases {
		got := parseGenParams([]byte(c.body))
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
		for k, v := range c.want {
			gv, ok := got[k]
			if !ok {
				t.Fatalf("%s: 缺键 %s（got %v）", c.name, k, got)
			}
			gb, _ := json.Marshal(gv)
			wb, _ := json.Marshal(v)
			if string(gb) != string(wb) {
				t.Fatalf("%s: %s = %s want %s", c.name, k, gb, wb)
			}
		}
	}
}

// 端到端：客户端参数必须真的出现在发给上游的 body 里（不是只解析了不用）。
func TestGenParamsReachUpstream(t *testing.T) {
	var bodies []string
	fake := fakeUpstreamSized(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}, &bodies)
	srv, _, _, _ := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})

	body := `{"model":"glm-5.2","stream":true,"temperature":0.2,"top_p":0.9,"max_tokens":64,` +
		`"stop":["END"],"seed":11,"messages":[{"role":"user","content":"hi"}]}`
	if _, code := postChat(t, srv, body); code != 200 {
		t.Fatalf("status=%d", code)
	}
	if len(bodies) == 0 {
		t.Fatal("上游未收到请求")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"temperature": 0.2, "top_p": 0.9, "max_tokens": float64(64), "seed": float64(11)} {
		if got, ok := sent[k]; !ok || got != want {
			t.Fatalf("上游 body 缺/错 %s = %v（want %v）\n%s", k, got, want, truncateText(bodies[0], 300))
		}
	}
	if _, ok := sent["stop"]; !ok {
		t.Fatalf("上游 body 缺 stop：%s", truncateText(bodies[0], 300))
	}
}
