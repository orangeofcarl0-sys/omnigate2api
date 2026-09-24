// 阶段 8e（SPEC §28.4）：refresh envelope / 官方 CLI 头保真 / 区域感知 /
// 错误语义分类 / 模型清单 / tool_choice string / 原生 tool_calls 拼装。
package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"omnigate2api/internal/auth"
)

func TestTencentRefreshEnvelope(t *testing.T) {
	var gotSource string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/token/refresh" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		gotSource = r.Header.Get("X-Auth-Refresh-Source")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{"accessToken":"env-token","refreshToken":"env-refresh","expiresIn":1800,"domain":"d"}}`)
	}))
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	tok, err := c.RefreshToken(context.Background(), LoginConfig{}, "rt", "vv", "www.codebuddy.cn")
	if err != nil {
		t.Fatal(err)
	}
	if gotSource != "workbuddy" {
		t.Fatalf("source=%q", gotSource)
	}
	if tok.Credentials.SecurityToken != "env-token" || tok.RefreshToken != "env-refresh" {
		t.Fatalf("envelope parse failed: %+v", tok)
	}
	if tok.Credentials.Expiration == "" {
		t.Fatal("expiration must be set from envelope expiresIn")
	}
}

func TestTencentRefreshEnvelopeBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":10001,"msg":"login expired","data":null}`)
	}))
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	_, err := c.RefreshToken(context.Background(), LoginConfig{}, "rt", "v", "")
	if err == nil || !strings.Contains(err.Error(), "login expired") {
		t.Fatalf("business error must surface: %v", err)
	}
}

func TestTencentChatHeadersOfficialCLI(t *testing.T) {
	var got map[string]string
	srv := fakeTencent(t, func(w http.ResponseWriter, r *http.Request) {
		got = map[string]string{
			"ua": r.Header.Get("User-Agent"), "accept": r.Header.Get("Accept"),
			"origin": r.Header.Get("Origin"), "referer": r.Header.Get("Referer"),
			"noauth": r.Header.Get("X-No-Authorization"), "nouid": r.Header.Get("X-No-User-Id"),
			"noent": r.Header.Get("X-No-Enterprise-Id"), "nodom": r.Header.Get("X-No-Department-Info"),
			"authz": r.Header.Get("Authorization"),
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	// 空账号字段：按官方 CLI 约定发 X-No-*: 1 占位
	rc, err := c.ChatStream(context.Background(), "", []ChatMessage{{Role: "user", Content: "q"}}, "", SignCredential{}, "n", "m", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if got["ua"] != TencentClientUA || got["accept"] != tencentClientAccept {
		t.Fatalf("UA/Accept not official CLI: %+v", got)
	}
	if got["origin"] != tencentOriginCN || got["referer"] != tencentOriginCN+"/" {
		t.Fatalf("origin/referer: %+v", got)
	}
	if got["noauth"] != "1" || got["nouid"] != "1" || got["noent"] != "1" || got["nodom"] != "1" {
		t.Fatalf("X-No-* placeholders missing for empty fields: %+v", got)
	}
	if got["authz"] != "" {
		t.Fatalf("empty token must not send Authorization: %q", got["authz"])
	}
}

func TestTencentResolveRegion(t *testing.T) {
	t.Setenv("OMNIGATE_TENCENT_BASE", "")
	c := NewTencent(5 * time.Second)
	if base, origin := c.resolve("www.workbuddy.ai"); base != tencentBaseGlobal || origin != tencentOriginGlobal {
		t.Fatalf("global: base=%s origin=%s", base, origin)
	}
	if base, origin := c.resolve("www.codebuddy.cn"); base != tencentBaseCN || origin != tencentOriginCN {
		t.Fatalf("cn: base=%s origin=%s", base, origin)
	}
	if base, origin := c.resolve(""); base != tencentBaseCN || origin != tencentOriginCN {
		t.Fatalf("empty domain must default to CN: base=%s origin=%s", base, origin)
	}
	// 环境覆盖仅影响 base，Origin 仍按域规则
	t.Setenv("OMNIGATE_TENCENT_BASE", "http://fake")
	c2 := NewTencent(5 * time.Second)
	if base, origin := c2.resolve("x.workbuddy.ai"); base != "http://fake" || origin != tencentOriginGlobal {
		t.Fatalf("env override: base=%s origin=%s", base, origin)
	}
}

func TestTencentClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   TencentErrKind
	}{
		{402, "{}", TencentErrHardCredit},
		{200, `{"msg":"积分不足"}`, TencentErrHardCredit},
		{200, `{"msg":"insufficient credit"}`, TencentErrHardCredit},
		{403, `{"msg":"offline user session not found"}`, TencentErrSessionDead},
		{200, `{"msg":"12153"}`, TencentErrSessionDead},
		{429, `{"error":"rate limited"}`, TencentErrOther}, // 429 走通用软冷却
		{500, `{}`, TencentErrOther},
	}
	for _, c := range cases {
		if got := ClassifyTencent(c.status, c.body); got != c.want {
			t.Fatalf("ClassifyTencent(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestTencentFetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/console/enterprises/personal/models" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Fatalf("authz=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"","data":{
  "models":[
    {"id":"glm-5.2","name":"GLM","maxInputTokens":200000,"maxOutputTokens":4096,"disabled":false},
    {"id":"old-model","name":"OLD","disabled":true},
    {"id":"unlisted","name":"X","disabled":false}
  ],
  "agents":[{"name":"cli","models":["glm-5.2","old-model"]},{"name":"ide","models":["unlisted"]}]
}}`)
	}))
	defer srv.Close()
	c := NewTencent(5 * time.Second)
	c.base = srv.URL
	infos, err := c.FetchModels(&auth.Auth{UserID: "u", CloudDragonTok: "tok", Domain: "www.codebuddy.cn"})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != "glm-5.2" {
		t.Fatalf("must expose only cli agent non-disabled models: %+v", infos)
	}
	if infos[0].ContextWindow != 200000 || infos[0].MaxTokens != 4096 {
		t.Fatalf("mapping maxInput/maxOutput: %+v", infos[0])
	}
}

func TestTencentChatStreamToolChoice(t *testing.T) {
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
	rc, err := c.ChatStream(context.Background(), "", []ChatMessage{{Role: "user", Content: "q"}}, "", SignCredential{SecurityToken: "t"}, "n", "m", nil, "required", nil)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if !strings.Contains(gotBody, `"tool_choice":"required"`) {
		t.Fatalf("tool_choice must be in body: %s", gotBody)
	}
}

// TestStreamDeltasNativeToolCalls 原生 tool_calls 增量拼装（§28.4 决策 D）：
// 多 index 片段按 index 拼接，终止帧前以单帧完整参数回调。
func TestStreamDeltasNativeToolCalls(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"\"}}]}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"/a\\\"\"}}]}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_2\",\"function\":{\"name\":\"Exec\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}]}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]}}]}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n" +
		"data: [DONE]\n"
	var got []ToolCall
	rc, err := StreamDeltasWithTools(strings.NewReader(stream),
		func(content, reason, finish string, upErr error) error { return nil },
		func(t ToolCall) error { got = append(got, t); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("must emit both stitched calls: %+v", got)
	}
	if got[0].ID != "call_1" || got[0].Name != "Read" || got[0].Arguments != `{"path":"/a"}` {
		t.Fatalf("call0 stitch wrong: %+v", got[0])
	}
	if got[1].ID != "call_2" || got[1].Name != "Exec" || got[1].Arguments != `{"cmd":"ls"}` {
		t.Fatalf("call1 stitch wrong: %+v", got[1])
	}
	if len(rc.ToolCalls) != 2 || rc.Finish != "tool_calls" {
		t.Fatalf("aggregation wrong: %+v finish=%q", rc.ToolCalls, rc.Finish)
	}
}

// TestAggregateRawNativeToolCalls [DONE] 终止（无命名事件）时同样聚合原生调用。
func TestAggregateRawNativeToolCalls(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"F\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n" +
		"data: [DONE]\n"
	rc, err := AggregateRaw(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.ToolCalls) != 1 || rc.ToolCalls[0].ID != "c1" || rc.ToolCalls[0].Arguments != `{"a":1}` {
		t.Fatalf("aggregate: %+v", rc.ToolCalls)
	}
}
