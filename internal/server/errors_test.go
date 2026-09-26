package server

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

// 传输层错误（非 ApiError，如 tls bad record MAC）是瞬时：不计数、不冷却。
func TestTransportErrorIsTransient(t *testing.T) {
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	_, p, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	acct := p.Get("u1")
	for i := 0; i < 5; i++ {
		h.handleUpstreamError(acct, "glm-5.2", errors.New(`Post "https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions": remote error: tls: bad record MAC`))
	}
	list := p.List()
	if len(list) != 1 || list[0]["err_count"] != 0 || list[0]["cooling"] != false || list[0]["disabled"] != false {
		t.Fatalf("transport errors must not penalize account: %+v", list[0])
	}
}

// 额度耗尽（InferHub.4291 insufficient quota）：硬冷却至次日，不累计错误数。
func TestQuotaErrorHardCooldown(t *testing.T) {
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	_, p, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	acct := p.Get("u1")
	ae := &upstream.ApiError{Status: 200, Message: `{"code":"InferHub.4291.200","msg":"insufficient quota"}`}
	for i := 0; i < 5; i++ {
		h.handleUpstreamError(acct, "glm-5.2", ae)
	}
	list := p.List()
	if len(list) != 1 || !list[0]["cooling"].(bool) {
		t.Fatalf("quota must cooldown: %+v", list[0])
	}
	if list[0]["err_count"] != 0 {
		t.Fatalf("quota must not accumulate err_count: %+v", list[0])
	}
	// 软冷却（60s 级）可自恢复：清冷却后立即可用（分钟级限流语义）
	h.cfg.Pool.ClearCooldown("u1")
	if !h.cfg.Pool.Healthy("u1") {
		t.Fatalf("quota soft-cooldown must clear: account unhealthy")
	}
	// 非额度错误（普通 4xx，非并发/非 429）仍累计
	h.cfg.Pool.Enable("u1")
	h.cfg.Pool.ClearCooldown("u1")
	h.handleUpstreamError(acct, "glm-5.2", &upstream.ApiError{Status: 400, Message: "other error"})
	if list2 := p.List(); list2[0]["err_count"] != 1 {
		t.Fatalf("generic 4xx must accumulate: %+v", list2[0])
	}
}

// 模型级限流（429 + code 6004）只冷却 (账号, 模型)：账号保持健康，换模型可继续用，
// 请求自动轮换到其它账号。这是"号池自动切换"在单模型限流下的正确口径。
func TestModelRateLimitSettlesPerModel(t *testing.T) {
	fake := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	srv, p, _, h := buildTestServer(t, fake.URL,
		[]*auth.Auth{fakeAuth("u1", "tok1"), tencentFakeAuth("u2", "tok2")})
	_ = srv
	acct := p.Get("u2")
	body := `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-22 09:46:39 UTC+8 重置，您也可以切换其他模型继续使用。"}`
	h.handleUpstreamError(acct, "glm-5.2", &upstream.ApiError{Status: 429, Message: body})

	if !p.Healthy("u2") {
		t.Fatal("model-scoped limit must not cool the whole account")
	}
	if !p.ModelCooled("u2", "glm-5.2") {
		t.Fatal("the limited (account, model) pair must be cooling")
	}
	if p.ModelCooled("u2", "kimi-k2.7") {
		t.Fatal("other models must stay available")
	}
	if a := p.PickForModel("workbuddy", "glm-5.2", nil); a == nil || a.Name != "u2" {
		// 只有一个腾讯账号时是兜底返回；关键是"不因单模型限流而整体不可用"
		t.Fatalf("sole workbuddy account must remain selectable as fallback: %+v", a)
	}
}

// 请求参数非法（华为 InferHub.001001005）是客户端侧问题：不计数、不冷却账号——
// 否则一个客户端的坏参数（实测 max_tokens=131072 > 华为硬上限 65536）三次就能把
// 整个华为渠道打成 10 分钟 no_healthy_account。
func TestRequestParamErrorNoPenalty(t *testing.T) {
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": okStream(false)})
	_, p, _, h := buildTestServer(t, huawei.URL, []*auth.Auth{fakeAuth("u1", "tok1")})
	acct := p.Get("u1")
	ae := &upstream.ApiError{Status: 400,
		Message: "codearts error code=InferHub.001001005.400 msg=The request param is invalid, Please check it"}
	for i := 0; i < 5; i++ {
		h.handleUpstreamError(acct, "glm-5.3-flash", ae)
	}
	list := p.List()
	if len(list) != 1 || list[0]["err_count"] != 0 || list[0]["cooling"] != false || list[0]["disabled"] != false {
		t.Fatalf("param error must not penalize account: %+v", list[0])
	}
}

// 端到端：上游 400 + 001001005 → 客户端拿到 400 invalid_request_error（上游原话），
// 且**不轮换**（换号无用）、账号保持健康。
func TestIntegrationRequestParamErrorFailsFast(t *testing.T) {
	var calls int
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": func(w http.ResponseWriter) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error_code":"InferHub.001001005","error_msg":"The request param is invalid, Please check it"}`)
	}})
	srv, p, _, _ := buildTestServer(t, huawei.URL, []*auth.Auth{fakeAuth("u1", "tok1")})

	raw, status := postChat(t, srv, `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":131072}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if !strings.Contains(string(raw), "invalid_request_error") || !strings.Contains(string(raw), "001001005") {
		t.Fatalf("client must get the upstream param error: %s", raw)
	}
	if calls != 1 {
		t.Fatalf("param error must not rotate to other accounts: upstream calls=%d", calls)
	}
	if list := p.List(); list[0]["err_count"] != 0 || list[0]["cooling"] != false {
		t.Fatalf("account must stay healthy: %+v", list[0])
	}
}

// 端到端（流内错误帧形态，实测华为就是这么回 001001005 的）：HTTP 200 + 错误帧
// → 客户端拿到 400 invalid_request_error（上游原话）、**不换号**、账号保持健康。
func TestIntegrationRequestParamErrorFrameTerminal(t *testing.T) {
	paramFrame := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseFrame(w, "", `{"error_code":"InferHub.001001005.400","error_msg":"The request param is invalid, Please check it"}`)
		sseFrame(w, "", `[DONE]`)
	}
	var calls int
	huawei := fakeUpstream(t, map[string]func(w http.ResponseWriter){"*": func(w http.ResponseWriter) {
		calls++
		paramFrame(w)
	}})
	srv, p, _, _ := buildTestServer(t, huawei.URL, []*auth.Auth{fakeAuth("u1", "tok1")})

	raw, status := postChat(t, srv, `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if !strings.Contains(string(raw), "invalid_request_error") || !strings.Contains(string(raw), "001001005") {
		t.Fatalf("client must get the upstream param error: %s", raw)
	}
	if calls != 1 {
		t.Fatalf("param error must be terminal (no rotation): upstream calls=%d", calls)
	}
	if list := p.List(); list[0]["err_count"] != 0 || list[0]["cooling"] != false {
		t.Fatalf("account must stay healthy: %+v", list[0])
	}
}
