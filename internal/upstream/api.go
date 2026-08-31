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
type ChatAPI interface {
	ChatStream(ctx context.Context, chatID string, messages []ChatMessage,
		traceID string, cred SignCredential, userName, model string,
		tools []map[string]any, toolChoice string) (io.ReadCloser, error)
	RefreshToken(ctx context.Context, cfg LoginConfig, refreshToken, codeVerifier, domain string) (*TokenResponse, error)
}
