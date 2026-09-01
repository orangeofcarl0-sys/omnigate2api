package server

import (
	"errors"
	"net/http"
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
		h.handleUpstreamError(acct, errors.New(`Post "https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions": remote error: tls: bad record MAC`))
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
		h.handleUpstreamError(acct, ae)
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
	h.handleUpstreamError(acct, &upstream.ApiError{Status: 400, Message: "other error"})
	if list2 := p.List(); list2[0]["err_count"] != 1 {
		t.Fatalf("generic 4xx must accumulate: %+v", list2[0])
	}
}
