// 客户端生成参数：发送侧形态纪律（SPEC §33.4）。
//
// 原则：**发什么由官方客户端的形态决定，而不是由"上游恰好接受"决定**——上游多认一个字段，
// 不代表客户端可以发它：请求形态本身就是指纹。
//   - forward：官方客户端会发（或其模型配置驱动）的参数 → 透传：
//     max_tokens（含 max_completion_tokens / max_output_tokens 别名）、temperature、top_p、reasoning_effort；
//   - default-only：官方客户端没有的控件（stop/seed/penalties/logprobs/n/response_format）→
//     **默认值静默放行、非默认值 400**（既不产生形态差异，也不做静默失效）。
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"omnigate2api/internal/auth"
)

func TestParseGenParamsPolicy(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		want         map[string]any
		wantRejected []string
	}{
		{"对齐参数透传", `{"temperature":0.2,"top_p":0.9,"max_tokens":64,"reasoning_effort":"low"}`,
			map[string]any{"temperature": 0.2, "top_p": 0.9, "max_tokens": float64(64), "reasoning_effort": "low"}, nil},
		{"别名归一到 max_tokens", `{"max_completion_tokens":32}`, map[string]any{"max_tokens": float64(32)}, nil},
		{"responses 的 max_output_tokens 同归一", `{"max_output_tokens":48}`, map[string]any{"max_tokens": float64(48)}, nil},
		{"别名并存时 max_tokens 优先", `{"max_completion_tokens":32,"max_tokens":64}`, map[string]any{"max_tokens": float64(64)}, nil},
		{"空串与 null 视为未指定", `{"temperature":"","top_p":null}`, nil, nil},

		{"非对齐参数的非默认值 → 拒绝", `{"stop":["END"]}`, nil, []string{"stop"}},
		{"anthropic 的 stop_sequences 同样拒绝", `{"stop_sequences":["END"]}`, nil, []string{"stop_sequences"}},
		{"seed 非 0 → 拒绝", `{"seed":42}`, nil, []string{"seed"}},
		{"logprobs=true → 拒绝", `{"logprobs":true}`, nil, []string{"logprobs"}},
		{"n>1 → 拒绝", `{"n":3}`, nil, []string{"n"}},
		{"response_format 非 text → 拒绝", `{"response_format":{"type":"json_object"}}`, nil, []string{"response_format"}},

		{"非对齐参数的默认值放行且不透传", `{"stop":[],"seed":0,"logprobs":false,"n":1,"response_format":{"type":"text"},"presence_penalty":0}`,
			nil, nil},
		{"对齐与非默认非对齐并存 → 只报违规键", `{"temperature":0.5,"stop":["x"],"seed":9}`,
			map[string]any{"temperature": 0.5}, []string{"stop", "seed"}},
		{"未涉及生成参数", `{"messages":[]}`, nil, nil},
	}
	for _, c := range cases {
		got, rejected := parseGenParams([]byte(c.body))
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
		if len(rejected) != len(c.wantRejected) {
			t.Fatalf("%s: rejected=%v want %v", c.name, rejected, c.wantRejected)
		}
		for i := range rejected {
			if rejected[i] != c.wantRejected[i] {
				t.Fatalf("%s: rejected=%v want %v", c.name, rejected, c.wantRejected)
			}
		}
	}
}

// 端到端：对齐参数必须真的到达上游；非对齐的非默认值必须 400（而非静默失效）。
func TestGenParamsSendSideDiscipline(t *testing.T) {
	var bodies []string
	fake := fakeUpstreamSized(t, map[string]func(w http.ResponseWriter){"*": okStream(false)}, &bodies)
	srv, _, _, _ := buildTestServer(t, fake.URL, []*auth.Auth{fakeAuth("u1", "tok1")})

	ok := `{"model":"glm-5.2","stream":true,"temperature":0.2,"top_p":0.9,"max_tokens":64,` +
		`"reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`
	if _, code := postChat(t, srv, ok); code != 200 {
		t.Fatalf("对齐参数应放行，status=%d", code)
	}
	if len(bodies) == 0 {
		t.Fatal("上游未收到请求")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"temperature": 0.2, "top_p": 0.9, "max_tokens": float64(64), "reasoning_effort": "low",
	} {
		if got, has := sent[k]; !has || got != want {
			t.Fatalf("上游 body 缺/错 %s = %v（want %v）\n%s", k, got, want, truncateText(bodies[0], 300))
		}
	}
	// 官方客户端没有的字段绝不能出现在 body 里（发送形态即指纹）
	for _, k := range []string{"stop", "seed", "logprobs", "top_logprobs", "n", "response_format", "frequency_penalty"} {
		if _, has := sent[k]; has {
			t.Fatalf("body 不得含官方客户端没有的字段 %q：%s", k, truncateText(bodies[0], 300))
		}
	}

	bad := `{"model":"glm-5.2","stream":true,"stop":["END"],"messages":[{"role":"user","content":"hi"}]}`
	raw, code := postChat(t, srv, bad)
	if code != 400 {
		t.Fatalf("非对齐参数的非默认值应 400，got %d", code)
	}
	if !strings.Contains(string(raw), "stop") || !strings.Contains(string(raw), "unsupported parameter") {
		t.Fatalf("400 文案需点名违规键：%s", truncateText(string(raw), 200))
	}
}
