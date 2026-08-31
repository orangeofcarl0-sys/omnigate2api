package server

import (
	"errors"
	"net/http"
	"testing"

	"omnigate2api/internal/auth"
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
