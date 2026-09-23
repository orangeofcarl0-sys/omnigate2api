// 腾讯区域契约差异（SPEC §28.4 决策 C 补充）：全球域与国内域对请求形态要求不同。
//
// 全球域（凭证 domain 后缀 .workbuddy.ai）要求 **首条消息必须是 system prompt**：
// 否则 400 + `code=11128 first message is not system prompt`
// （displayMsg：请求被安全策略拦截）。2026-09-24 实测同一请求：
//
//	国内域 copilot.tencent.com  仅 user → 200；system+user → 200
//	全球域 www.workbuddy.ai     仅 user → 400/11128；system+user → 200
//
// 后果（修复前）：全球账号在池里"看着健康"，实际每次被轮询到都 400——请求靠
// 换号重试兜住，但白付一次往返、并把该账号连续错误推入冷却（面板显示
// "consecutive errors"）；若家族内只剩全球账号则请求直接失败。
//
// 该前置由官方全球客户端必然满足（否则它自己也无法工作），故网关侧补齐；国内域
// **不注入**，保持"模型所见 = 客户端所发"的默认承诺。
package upstream

// globalRealmSystemPrompt 全球域补齐用的系统提示。内容中性最小——上游只校验
// 首条消息的 role，不校验文案（实测）；不臆造官方客户端的具体提示词。
const globalRealmSystemPrompt = "You are a helpful AI assistant."

// ensureLeadingSystem 首条非 system 时前置一条 system 消息（不改动原切片）。
func ensureLeadingSystem(messages []ChatMessage) []ChatMessage {
	if len(messages) > 0 && messages[0].Role == "system" {
		return messages
	}
	out := make([]ChatMessage, 0, len(messages)+1)
	out = append(out, ChatMessage{Role: "system", Content: globalRealmSystemPrompt})
	return append(out, messages...)
}
