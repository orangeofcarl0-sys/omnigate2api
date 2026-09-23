// 上游传输层必须有界：手工构造 Transport 不会继承 DefaultTransport 的默认超时，
// 缺 DialContext/TLSHandshakeTimeout 会让不可达区域把请求挂死（不报错、不换号，
// 「号池自动切换」因此失效——2026-09-24 实测现象）。
package upstream

import (
	"net/http"
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
