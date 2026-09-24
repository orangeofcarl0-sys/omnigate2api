// 上游客户端家族（SPEC §23.1）：ChatAPI 统一约定，华为 Client 与
// TencentClient 各自实现，内核对接口编程（Account.Client 类型为 ChatAPI）。
package upstream

import (
	"context"
	"io"
)

// ChatAPI 上游聊天能力的统一约定。签名与既有 Client 一致，调用方写法不变；
// 各实现按上游语义取用参数（华为用 AK/SK 签名与 STS，腾讯用 Bearer +
// X-User-Id/X-Domain 系列头并忽略签名）。
//
// tools：roles 上游原样透传进 body；toolChoice：腾讯上游 string 语义的
// 归一化结果（§28.4 决策 D，"none" 对应删 tools 已在上层完成，此处非空即拼入）；
// domain：refresh 端点区域解析用（华为忽略）。
//
// gen：客户端生成参数（temperature/top_p/stop/seed/max_tokens/… 的白名单透传结果，
// 见 server 侧 parseGenParams）。**这是修"客户端设置被静默丢弃"的关键参数**：
// 早前 body 只拼 model/stream/messages/tools，客户端设的 temperature/max_tokens
// 全部无效。实现约定：gen 先铺底，**model/stream/messages 等权威字段随后覆盖**，
// 客户端无法通过这些键篡改路由或流形态。
type ChatAPI interface {
	ChatStream(ctx context.Context, chatID string, messages []ChatMessage,
		traceID string, cred SignCredential, userName, model string,
		tools []map[string]any, toolChoice string, gen map[string]any) (io.ReadCloser, error)
	RefreshToken(ctx context.Context, cfg LoginConfig, refreshToken, codeVerifier, domain string) (*TokenResponse, error)
}

// applyGen 把生成参数铺进 body（nil 值不落键；调用方随后覆盖权威字段）。
func applyGen(body map[string]any, gen map[string]any) {
	for k, v := range gen {
		if v == nil {
			continue
		}
		body[k] = v
	}
}
