// 上游传输层必须有界：手工构造 Transport 不会继承 DefaultTransport 的默认超时，
// 缺 DialContext/TLSHandshakeTimeout 会让不可达区域把请求挂死（不报错、不换号，
// 「号池自动切换」因此失效——2026-09-24 实测现象）。
package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUpstreamTransportHasTimeouts(t *testing.T) {
	tr := newTransport()
	if tr.DialContext == nil {
		t.Fatal("DialContext must be set (unbounded dial hangs on unreachable realms)")
	}
	if tr.TLSHandshakeTimeout <= 0 || tr.TLSHandshakeTimeout > 30*time.Second {
		t.Fatalf("TLSHandshakeTimeout must be small and bounded, got %v", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout must stay generous for cold starts, got %v", tr.ResponseHeaderTimeout)
	}
	// 两个家族必须共用同一份配置（否则只修一边）：流式客户端无整体 Timeout，
	// 只能靠传输层兜底，尤其要看住它。
	for label, c := range map[string]*http.Client{
		"tencent": NewTencent(5 * time.Second).streamHTTP,
		"huawei":  New(5 * time.Second).streamHTTP,
	} {
		tr2, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: stream client transport type %T", label, c.Transport)
		}
		if tr2.DialContext == nil || tr2.TLSHandshakeTimeout <= 0 {
			t.Fatalf("%s: stream transport lacks dial/TLS bounds (would hang, not rotate)", label)
		}
	}
}

// 传输层抖动判定：EOF / TLS 握手超时 / 连接重置算抖动（值得原地重试一次）；
// 上游真回的业务错误（ApiError）**不算**——重试只会掩盖语义。

func TestIsTransientTransportErr(t *testing.T) {
	transient := []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		errors.New(`Post "https://www.workbuddy.ai/v2/chat/completions": EOF`),
		errors.New(`Post "https://www.workbuddy.ai/v2/chat/completions": net/http: TLS handshake timeout`),
		errors.New("read tcp 127.0.0.1:1234->1.2.3.4:443: connection reset by peer"),
		errors.New("write: broken pipe"),
		errors.New("http2: server sent GOAWAY and closed the connection"),
		&net.DNSError{Err: "no such host", Name: "x.example"},
	}
	for _, e := range transient {
		if !isTransientTransportErr(e) {
			t.Fatalf("应判为瞬时抖动: %v", e)
		}
	}
	notTransient := []error{
		nil,
		errors.New("json: cannot unmarshal string"),
		&ApiError{Code: 429, Status: 429, Message: `{"code":6004}`},
		errors.New("上游封禁"),
	}
	for _, e := range notTransient {
		if isTransientTransportErr(e) {
			t.Fatalf("不该判为瞬时抖动: %v", e)
		}
	}
}

// flakyRT 头 N 次返回传输层错误，之后交给真实 RoundTripper —— 用来验证"换新连接重试一次"。
type flakyRT struct {
	fails int
	inner http.RoundTripper
	seen  []bool // 每次请求的 req.Close（true = 强制新连接）
}

func (f *flakyRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.seen = append(f.seen, req.Close)
	if f.fails > 0 {
		f.fails--
		return nil, errors.New(`Post "https://x/v2/chat/completions": EOF`)
	}
	return f.inner.RoundTrip(req)
}

func TestDoWithRetryRecoversFromTransient(t *testing.T) {
	inner := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	rt := &flakyRT{fails: 1, inner: inner}
	c := &http.Client{Transport: rt}
	resp, err := doWithRetry(context.Background(), c, func(fresh bool) (*http.Request, error) {
		req, _ := http.NewRequest(http.MethodPost, "https://x/v2/chat/completions", strings.NewReader("{}"))
		req.Close = fresh
		return req, nil
	}, 2)
	if err != nil {
		t.Fatalf("抖动后应重试成功: %v", err)
	}
	defer resp.Body.Close()
	if len(rt.seen) != 2 {
		t.Fatalf("应恰好请求 2 次，实际 %d", len(rt.seen))
	}
	if rt.seen[0] || !rt.seen[1] {
		t.Fatalf("第二次必须强制新连接（Close=true）: %v", rt.seen)
	}
}

func TestDoWithRetryGivesUpAndDoesNotRetryBusinessError(t *testing.T) {
	// 连续抖动：用尽重试后返回最后一次错误
	rt := &flakyRT{fails: 5, inner: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	c := &http.Client{Transport: rt}
	if _, err := doWithRetry(context.Background(), c, func(bool) (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "https://x/y", strings.NewReader("{}"))
	}, 2); err == nil {
		t.Fatal("持续抖动必须把错误抛出去（调用方据此换号/记日志）")
	}
	if len(rt.seen) != 2 {
		t.Fatalf("只应尝试 2 次，实际 %d", len(rt.seen))
	}

	// 业务错误（非抖动）不得重试：上游真的回了错误码，重试等于掩盖语义
	calls := 0
	rt2 := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("some business failure")
	})
	c2 := &http.Client{Transport: rt2}
	if _, err := doWithRetry(context.Background(), c2, func(bool) (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "https://x/y", strings.NewReader("{}"))
	}, 3); err == nil {
		t.Fatal("应返回错误")
	}
	if calls != 1 {
		t.Fatalf("非抖动错误不该重试，实际调用 %d 次", calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 传输层必须读代理环境变量：容器默认直连，而国内直连国际域名极不稳定
// （2026-09-29 实测 6h 243 次 EOF/TLS 超时 → 客户端 503），要靠 HTTPS_PROXY 兜住。
// 注：标准库对代理环境变量是**首次读取后缓存**的，故这里只断言"已接上 ProxyFromEnvironment"
// 这条接线（行为验证放在实机：设 HTTPS_PROXY 后看容器能否经代理出网）。
func TestTransportHonorsProxyEnv(t *testing.T) {
	tr := newTransport()
	if tr.Proxy == nil {
		t.Fatal("newTransport 必须设置 Proxy（http.ProxyFromEnvironment），否则 HTTPS_PROXY 形同虚设")
	}
}
