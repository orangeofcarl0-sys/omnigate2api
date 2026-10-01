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
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
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
//
// Proxy：**走标准环境变量**（HTTPS_PROXY / HTTP_PROXY / NO_PROXY）。默认不设时行为与以前
// 完全一致（直连）；一旦设了就按环境变量走代理。为什么必须补上这一条（2026-09-29 实测）：
// 本机是**国内直连国际域名**，到 `www.workbuddy.ai` 的连接极不稳定——6 小时内 243 次
// 传输层抖动（168 × EOF + 75 × TLS handshake timeout，最密一分钟 72 次），4 个可用号
// 接连失败后客户端拿到 503「all accounts unavailable」。而宿主机上就开着代理（v2rayN），
// 容器先前既不读 proxy 变量、Transport 也没有 Proxy 字段 ⇒ 永远直连。
//
// 用法（容器内）：`HTTPS_PROXY=http://host.docker.internal:10808`。本机 v2rayN 的 xray
// 入站 10808 是**混合口**（GUI 里记 `Protocol: socks`，实测同时吃 SOCKS5 与 HTTP CONNECT），
// 所以不必再开 10809 HTTP 入站；`socks5://` 亦可——标准库原生支持（`net/http` 的 proxy
// scheme 分派里 socks5/socks5h 是一等公民，零新依赖）。
//
// 并把国内域放进 `NO_PROXY`——国内 API 绕道出海只会更慢更不稳：
//
//	NO_PROXY=copilot.tencent.com,codebuddy.cn,workbuddy.cn,workbuddy.ai,
//	         myhuaweicloud.com,huaweicloud.com,localhost,127.0.0.1
//
// `workbuddy.ai` 也在此列（8/9 个账号的 domain 就是它）。但别误会它的作用：容器出网实际由
// Docker 的 mirrored 网络统一交给 v2rayN 按白名单分流（实测容器直连国内站出口=杭州电信、
// 直连国外站出口=节点 IP），`NO_PROXY` 只决定 Go 要不要多发一次 CONNECT，**并不能让包绕开
// v2rayN**。要让它真直连，得在 v2rayN 侧加 `domain:workbuddy.ai → direct` 路由
// （2026-10-01 实测：宿主直连 6/6=200，同期宿主经代理与容器两条路全部 0/6）。
// `huaweicloud.com` 与 `myhuaweicloud.com` 是两个父域，Go 的 NO_PROXY **不做跨父域覆盖**，都要列。
//
// 注意：`host.docker.internal` 在本机同时解析出 IPv4（192.168.65.254）与 IPv6
// （fdc4:f303:9324::254），而 xray 只监听 127.0.0.1 ⇒ **IPv6 那条连不上**。Go 按解析
// 顺序逐个拨号并回退，实测最终落在 IPv4；若某天代理连不上，先怀疑这里——把 `HTTPS_PROXY`
// 的主机名直接写成 `192.168.65.254` 可钉死 IPv4。
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
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

// doWithRetry 发请求；遇**传输层抖动**（EOF / 连接被上游断开 / TLS 握手超时 / 连接重置）
// 时**换新连接重试一次**，返回最后一次的错误。
//
// 为什么必须重试（2026-09-29 实测）：
//
//	① 上游对空闲连接关得很快，而我们复用连接池——复用一条已被对端关掉的连接做 POST 必然
//	   `EOF`（本容器 6 小时内 168 次）；
//	② 本机到 www.workbuddy.ai 的握手偶发超时（同期 75 次）。
//
// 这两种都是**网络抖动而非账号问题**：`net/http: TLS handshake timeout` / `EOF`。
// 上游那侧"换账号"完全没用（走的是同一条网络），结果 4 个可用号接连失败 → 客户端拿到
// 503「all accounts unavailable」，而面板一片干净（传输错误按设计不罚账号、不改状态）。
// 重试一次几乎总能成功（实测紧随其后的请求就 200）。
//
// 重试的那次把 `Close` 置真，强制新建连接——否则连接池很可能又把那条死连接递回来。
//
// `accountKey` 只用于**观测**（池子记账的键，取 `Auth.UserID`，与 `pool.Account.UID` 同值）：
// 重试**成功**也要留痕。2026-09-30 复盘发现，此前重试成功时既不写日志也不计数，于是
// 「瞬时错误 0」被误读成「链路干净」——实际含义只是「没有连续两次都失败」。现在抖动
// 一发生就记 jitter 日志，被重试消化掉还会额外记一条 recovered 并回调池子计数。
func doWithRetry(ctx context.Context, c *http.Client, build func(fresh bool) (*http.Request, error), attempts int, accountKey string) (*http.Response, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		req, err := build(i > 0)
		if err != nil {
			return nil, err
		}
		resp, err := c.Do(req)
		if err == nil {
			if i > 0 {
				log.Printf("upstream transport retry recovered account=%s attempt=%d/%d err=%v",
					accountKey, i+1, attempts, lastErr)
				noteRetry(accountKey)
			}
			return resp, nil
		}
		if !isTransientTransportErr(err) {
			return nil, err
		}
		lastErr = err
		// 请求上下文已结束（客户端取消/整体超时）时重试毫无意义，直接返回。
		if ctx != nil && ctx.Err() != nil {
			return nil, err
		}
		// 只在**确实还有下一次**时才记，避免最后一次失败也报 "retrying" 误导排障。
		if i < attempts-1 {
			log.Printf("upstream transport jitter (retrying) account=%s attempt=%d/%d err=%v",
				accountKey, i+1, attempts, err)
		}
	}
	return nil, lastErr
}

// retryObserver 重试观测回调（由 server 启动时注入一次；nil = 只记日志、不落池子）。
//
// 为什么用回调而不是直接调池子：`internal/upstream` 不依赖 `internal/pool`（单向依赖，
// 见包注释），传输层也不该认识账号池。但它手里正好有账号键（`Auth.UserID`），
// 于是一根函数指针就能把「抖动被消化」这件事送到面板上。
var retryObserver func(accountKey string)

// SetRetryObserver 注入重试观测回调（进程启动时调用一次）。
func SetRetryObserver(fn func(accountKey string)) { retryObserver = fn }

func noteRetry(accountKey string) {
	if retryObserver != nil && accountKey != "" {
		retryObserver(accountKey)
	}
}

// isTransientTransportErr 传输层抖动判定：值得原地重试一次、且**不该**罚账号或冷却。
// 与 ApiError（上游真的回了错误码）严格区分：后者是业务语义，不能重试掩盖。
func isTransientTransportErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, m := range []string{
		"eof", "connection reset", "broken pipe", "tls handshake timeout",
		"server closed idle connection", "use of closed network connection",
		"http2: server sent goaway", "connection refused", "no such host",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
