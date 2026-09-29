// 上游客户端家族（SPEC §23.1）：ChatAPI 统一约定，华为 Client 与
// TencentClient 各自实现，内核对接口编程（Account.Client 类型为 ChatAPI）。
package upstream

import (
	"context"
	"encoding/json"
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
//
// dpopKeyJSON：华为 DPoP 私钥（JWK JSON，SPEC §24.5）。授权码登录时生成并随凭证落盘，
// refresh 必须复用同一把（换新密钥 → 上游 `InvalidDPoPHeader`）；腾讯忽略该参数。
type ChatAPI interface {
	ChatStream(ctx context.Context, chatID string, messages []ChatMessage,
		traceID string, cred SignCredential, userName, model string,
		tools []map[string]any, toolChoice string, gen map[string]any) (io.ReadCloser, error)
	RefreshToken(ctx context.Context, cfg LoginConfig, refreshToken, codeVerifier, domain, dpopKeyJSON string) (*TokenResponse, error)
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

// CodeartsMaxOutputTokens 华为（CodeArts / InferHub）**单次请求**接受的
// `max_tokens` 硬上限。
//
// 实测（2026-09-26，两个模型边界一致，每步清冷却避免连锁）：
// glm-5.3-flash `max_tokens=65536` → 200，`65537` → 400
// `InferHub.001001005 The request param is invalid`；
// deepseek-v4-flash-0731 同为 `65536` ✓ / `65537` ✗。
//
// 而上游自己的模型目录（agent detail `model_parameters.max_tokens`）广告的是
// 131072 / 393216——**上游广告值高于其 API 实际接受值**。客户端（ZCode 等）照广告值
// 下发 max_tokens 就必得 400，故发送侧按硬上限钳制（钳制时记日志，便于与上游对账）。
const CodeartsMaxOutputTokens = 65536

// clampCodeartsMaxTokens 把 body 里的 `max_tokens` 钳到华为硬上限（无该键或未超限不动）。
// 返回 (原值, 钳后值, 是否钳制)，供调用方记日志。
func clampCodeartsMaxTokens(body map[string]any) (from, to int64, clamped bool) {
	v, ok := body["max_tokens"]
	if !ok {
		return 0, 0, false
	}
	n, ok := toInt64(v)
	if !ok || n <= CodeartsMaxOutputTokens {
		return 0, 0, false
	}
	body["max_tokens"] = int64(CodeartsMaxOutputTokens)
	return n, CodeartsMaxOutputTokens, true
}

// toInt64 宽松取整（入站 JSON 解出的是 float64；也可能是 int/int64/json.Number）。
func toInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
	}
	return 0, false
}
