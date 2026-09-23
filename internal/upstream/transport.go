// 上游 HTTP 传输层（华为/腾讯共用一份配置，SPEC §28.4 决策 C 补充）。
//
// 为什么必须显式写超时：手工构造 `&http.Transport{}` **不会**继承 DefaultTransport
// 的默认值——`DialContext` 为 nil 时用无超时的拨号、`TLSHandshakeTimeout` 为 0 时
// 握手也不设限。实测后果：当某个区域（如本机到 www.workbuddy.ai）不可达时，
// 流式请求会一直挂在连接/握手阶段，既不报错也不换号，直到客户端自己超时——
// 「号池自动切换」在这种情形下完全失效（这是 2026-09-24 实测到的现象）。
//
// 分工：连接与握手必须有界且短（10s），**首字节（ResponseHeaderTimeout）保持宽松
// 300s**——活动模型/大会话冷启动确实可达数分钟，收紧它会误杀正常请求并连累账号冷却。
package upstream

import (
	"net"
	"net/http"
	"time"
)

const (
	// dialTimeout 建连上限：跨区域真实可达的 RTT 远小于此，超时即视为该账号不可达。
	dialTimeout = 10 * time.Second
	// tlsHandshakeTimeout TLS 握手上限（本机到某些域的握手会长时间无响应）。
	tlsHandshakeTimeout = 10 * time.Second
	// responseHeaderTimeout 首字节上限：宽松，容忍冷启动（见文件头说明）。
	responseHeaderTimeout = 300 * time.Second
)

// newTransport 上游统一传输层（华为/腾讯共用）。
func newTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: tlsHandshakeTimeout,
	}
}
