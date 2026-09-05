// 多模态媒体渲染与转换（SPEC §30）：
//   - renderMedia：按 Profile.media 分叉——placeholder 把 Images/Deferred 折叠为
//     占位文本（§13.3）；passthrough 把 http(s) 图片经转换层转 data URI 后保留
//     结构化形态（roles 分片透传），失败降级占位。
//   - 渲染位于 toolchain 之后、指纹路由之前（§30.3 管线位置：占位文本参与指纹，
//     passthrough 以 canonical URL 参与指纹）。
//   - 像素纪律：图片数据不进日志、不落 DEBUG_PROMPTS（§30.7）。
package server

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"omnigate2api/internal/adapt"
)

// 限制常量（SPEC §30.7）。
const (
	maxImageB64     = 10 << 20 // 单图 base64 上限（≈7.5MB 二进制）
	maxImagesPerMsg = 8        // 每消息图片数上限
	fetchTimeout    = 15 * time.Second
	fetchBodyMax    = 10 << 20 // 抓取响应体上限
)

// defaultMediaPlaceholder 非文本块缺省占位模板（§13.3；Profile 模板可覆盖）。
func defaultMediaPlaceholder(typ string) string {
	return "[用户发送了一个附件：" + typ + "]"
}

// mediaTemplate Profile 模板 → 占位函数；空模板用内置缺省，无 {type} 时整串生效。
func mediaTemplate(p *adapt.UpstreamProfile) func(string) string {
	tmpl := ""
	if p != nil && p.Message.Folding != nil {
		tmpl = p.Message.Folding.MediaPlaceholder
	}
	if tmpl == "" {
		return defaultMediaPlaceholder
	}
	if !strings.Contains(tmpl, "{type}") {
		return func(string) string { return tmpl }
	}
	return func(t string) string { return strings.ReplaceAll(tmpl, "{type}", t) }
}

// mediaMode 有效 media 模式：env 覆盖优先，缺省 placeholder（§30.2）。
func mediaMode(p *adapt.UpstreamProfile, override string) string {
	if override == "passthrough" || override == "placeholder" {
		return override
	}
	if p != nil && p.Message.MediaMode() == "passthrough" {
		return "passthrough"
	}
	return "placeholder"
}

// mediaRenderStats 渲染统计（观测日志）。
type mediaRenderStats struct {
	Images    int // 入参图片数
	Converted int // passthrough 抓取转换成功数
	Failed    int // 抓取失败降级数
	Deferred  int // 不可透传块/超限降级数
}

// renderMedia 就地渲染消息媒体（SPEC §30.3 管线位置：toolchain 后、指纹前）。
// placeholder 模式不发起网络请求；passthrough 模式经 fetch 转换 http(s) 图片。
func renderMedia(msgs []openAIMessage, mode string, ph func(string) string, fetch imageFetchFunc) mediaRenderStats {
	var st mediaRenderStats
	for i := range msgs {
		m := &msgs[i]
		st.Images += len(m.Images)
		st.Deferred += len(m.Deferred)
		var extra strings.Builder
		for _, d := range m.Deferred {
			if d.Fixed != "" {
				extra.WriteString(d.Fixed)
			} else {
				extra.WriteString(ph(d.Type))
			}
		}
		switch mode {
		case "passthrough":
			kept := m.Images[:0]
			for _, im := range m.Images {
				if strings.HasPrefix(im.URL, "data:") {
					kept = append(kept, im)
					continue
				}
				dataURI, merr := fetch(im.URL)
				if merr != "" {
					st.Failed++
					extra.WriteString("[图片抓取失败：" + string(merr) + "]")
					continue
				}
				im.URL = dataURI
				kept = append(kept, im)
				st.Converted++
			}
			m.Images = kept
		default: // placeholder（缺省）：不发网络请求
			for range m.Images {
				extra.WriteString(ph("image"))
			}
			m.Images = nil
		}
		m.Deferred = nil
		if extra.Len() > 0 {
			m.Text += extra.String()
		}
	}
	return st
}

// ---------------------------------------------------------------------------
// URL→data URI 转换层（SSRF 防护，§30.4）
// ---------------------------------------------------------------------------

// mediaErr 抓取失败类别（渲染为固定占位文本的括号内容）。
type mediaErr string

const (
	mErrPrivate    mediaErr = "私网拒绝"
	mErrRedirect   mediaErr = "重定向拒绝"
	mErrNonImage   mediaErr = "非图片"
	mErrTooLarge   mediaErr = "超限"
	mErrTimeout    mediaErr = "超时"
	mErrUnreachable mediaErr = "网络错误"
	mErrBadURL     mediaErr = "解析失败"
)

// imageFetchFunc 抓取并转换为 data URI；失败返回类别。
type imageFetchFunc func(rawURL string) (dataURI string, err mediaErr)

// guardedImageFetch 生产抓取器（§30.4 全套防护）。
func guardedImageFetch(rawURL string) (string, mediaErr) {
	return httpImageFetch(rawURL, true)
}

// httpImageFetch 抓取 URL → data URI。guard=true 启用 SSRF 防护（DNS/拨号双点
// 私网校验——Control 在拨号前拿到真实对端地址，封死 DNS rebinding 的 TOCTOU）；
// 重定向拒绝、Content-Type 限 image/*、响应体上限、超时对所有调用方生效。
// guard 仅供测试构造回路（httptest 监听在环回地址）。
func httpImageFetch(rawURL string, guard bool) (string, mediaErr) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", mErrBadURL
	}
	dialer := &net.Dialer{Timeout: fetchTimeout}
	if guard {
		// 快速失败路径：DNS 预校验（拨号期 Control 仍强制复检）
		if addrs, lerr := net.LookupIP(u.Hostname()); lerr == nil {
			for _, ip := range addrs {
				if forbiddenIP(ip) {
					return "", mErrPrivate
				}
			}
		}
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, serr := net.SplitHostPort(address)
			if serr != nil {
				return serr
			}
			if forbiddenIP(net.ParseIP(host)) {
				return errors.New(string(mErrPrivate))
			}
			return nil
		}
	}
	client := &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New(string(mErrRedirect))
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if rerr != nil {
		return "", mErrBadURL
	}
	resp, gerr := client.Do(req)
	if gerr != nil {
		if strings.Contains(gerr.Error(), string(mErrRedirect)) {
			return "", mErrRedirect
		}
		if errors.Is(gerr, context.DeadlineExceeded) {
			return "", mErrTimeout
		}
		var nerr net.Error
		if errors.As(gerr, &nerr) && nerr.Timeout() {
			return "", mErrTimeout
		}
		return "", mErrUnreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", mErrUnreachable
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "image/") {
		return "", mErrNonImage
	}
	body, rerr2 := io.ReadAll(io.LimitReader(resp.Body, fetchBodyMax+1))
	if rerr2 != nil {
		return "", mErrUnreachable
	}
	if len(body) > fetchBodyMax {
		return "", mErrTooLarge
	}
	if ct == "" {
		ct = http.DetectContentType(body)
		if !strings.HasPrefix(ct, "image/") {
			return "", mErrNonImage
		}
	}
	if !strings.Contains(ct, ";") {
		ct = strings.TrimSpace(strings.SplitN(ct, ",", 2)[0])
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(body), ""
}

// forbiddenIP SSRF 地址黑名单（§30.4）：环回/私网/链路本地/组播/未指定一律拒绝。
// IsPrivate 覆盖 RFC1918 与 fc00::/7；IsLinkLocalUnicast 覆盖 169.254/16 与 fe80::/10。
func forbiddenIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

// logMediaRender 观测日志（§30.8）：host 不含路径/查询（防签名泄露）。
func logMediaRender(profileID, mode string, st mediaRenderStats) {
	if st.Images == 0 && st.Deferred == 0 {
		return
	}
	log.Printf("media render profile=%s mode=%s images=%d converted=%d failed=%d deferred=%d",
		profileID, mode, st.Images, st.Converted, st.Failed, st.Deferred)
}
