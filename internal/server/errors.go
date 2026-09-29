// 错误分类与回合结果判定：客户端断开判定、上游错误分类（冷却/禁用/瞬时重试）。
package server

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// isClientCancel 判断错误是否由客户端断开/超时引起。客户端取消不代表上游
// 或账号异常，不应计入错误计数或触发冷却。
func isClientCancel(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// quotaMarkers 华为/上游额度耗尽关键词（InferHub.4291 insufficient quota 等）。
var quotaMarkers = []string{
	"insufficient quota", "quota exhausted", "quota exceeded", "no quota",
	"out of quota", "额度不足", "配额不足", "额度用尽", "inferhub.4291", "4291",
}

// isQuotaError 判定额度耗尽类错误（华为常规引擎月度配额/福利额度用尽）。
func isQuotaError(msg string) bool {
	low := strings.ToLower(msg)
	for _, m := range quotaMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// paramErrMarkers 上游"请求参数非法"标记（华为 InferHub.001001005）。
// 这是**请求侧**问题：客户端传了上游不接受的参数（实测 max_tokens>65536 必得此错），
// 换账号、等冷却都不会好——所有同家族账号必然同样拒绝。
var paramErrMarkers = []string{"inferhub.001001005", "the request param is invalid"}

// isRequestParamError 判定请求参数非法。这类错误**不得计入账号错误、不得冷却账号**：
// 否则一个客户端传错参数，三次就能把整个华为渠道打成 10 分钟 no_healthy_account
// （2026-09-26 实测复现：ZCode 按 /v1/models 广告的 max_output_tokens=131072 下发
// max_tokens，华为只接受 ≤65536）。处置 = 只记日志 + 把上游原话回给客户端。
func isRequestParamError(msg string) bool {
	low := strings.ToLower(msg)
	for _, m := range paramErrMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// modelRateLimitHorizon 模型级限流冷却上限：上游给出的解封时刻可能很远（甚至误报），
// 钳住以免一对 (账号,模型) 被长期废掉——到期自然重试，真限流会再次触发。
const modelRateLimitHorizon = 24 * time.Hour

// settleModelRateLimit 模型级限流结算（429 + code 6004）：**只冷却 (账号, 模型)**，
// 不动账号级健康——上游文案自证"可切换其他模型继续使用"，整账号冷却会平白丢掉该账号
// 对其余模型的容量；解封时刻优先取上游声明值。
func (h *Handler) settleModelRateLimit(acct *pool.Account, model, msg string) {
	now := time.Now()
	until := upstream.ModelRateLimitUntil(msg, now, h.cfg.SoftCooldown, modelRateLimitHorizon)
	h.cfg.Pool.CoolModel(acct.Name, model, until, msg)
	// 阈值观测（SPEC §28.5）：把"撞限时本窗口的请求数/token 数"连同窗口起点一并打进日志 ——
	// 上游报文不含任何数字，这个计数快照就是阈值本身的观测值（首次撞限即答案）。
	reqs, toks, since := h.modelUsage.NoteLimit(acct.Name, model, until, now)
	log.Printf("upstream model rate limit account=%s model=%s until=%s window_requests=%d window_tokens=%d window_start=%s msg=%s",
		acct.Name, model, until.Format(time.RFC3339), reqs, toks, since.Format(time.RFC3339), truncateText(msg, 140))
}

// settleTencentKind 腾讯专属错误结算（chat 与流式错误帧两条路径共用）。
// 返回 true = 已处理，调用方不要再走通用分支。
//
// 只认三类新语义（其余仍由 handleUpstreamError 既有分支处理）：
//   - `TencentErrAccountBanned`（11140 request illegal）：上游授权封禁——**换号无用且
//     不自愈**（社区两个同目标项目一致实测），故禁用该账号并把原因写清，让面板显式提示
//     "需重新登录"。2026-09-27 事故前我们把它当通用错误累计，结果每 10 分钟一轮假冷却、
//     客户端 12s 一轮重试，三个全球号被打成"全池不可用"。
//   - `TencentErrSoftRate`（11140 rate-limiting 变体等限流文案）：带「将在…重置」就
//     冷却到该墙钟，否则按配置软冷却。
//   - `TencentErrClientSide`（内容策略/参数类）：请求侧问题，不罚账号、原样透传。
func (h *Handler) settleTencentKind(acct *pool.Account, model, msg string) bool {
	switch upstream.ClassifyTencent(0, msg) {
	case upstream.TencentErrAccountBanned:
		// 提示语按 2026-09-27 实测校正：**重登无效**（三个全球号换全新凭证后，同内容同模型
		// 仍 11140）→ 该封禁不是凭证/授权状态，而是账号级（或账号组/区域）风控。社区
		// "需重登"的口径在此不适用，故给用户的是可执行的出口：换号 / 点「启用」试探。
		h.cfg.Pool.DisableSticky(acct.Name, "上游封禁（11140 request illegal）——重登无效（实测），可点「启用」重试或换号")
		log.Printf("upstream account banned account=%s model=%s msg=%s",
			acct.Name, model, truncateText(msg, 140))
		return true
	case upstream.TencentErrSoftRate:
		until := upstream.ModelRateLimitUntil(msg, time.Now(), h.cfg.SoftCooldown, modelRateLimitHorizon)
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, time.Until(until), msg)
		log.Printf("upstream soft rate account=%s model=%s until=%s msg=%s",
			acct.Name, model, until.Format(time.RFC3339), truncateText(msg, 140))
		return true
	case upstream.TencentErrTrialInactive:
		// 账号能登录、凭证有效，但站点侧试用未激活 → 任何模型都 429/14017，且不自愈。
		// 不禁用就会让号池反复挑中一个永远失败的号（客户端表现为随机报错）。
		// 非 sticky 禁用：站点激活后重新登录（面板「授权登录」）或点「启用」即可恢复。
		// 真正的修法（2026-09-29 实测）：账号**没走完站点入驻资料**（「完善你的资料」确认国家/地区，
		// 仅需填一次），所以站点侧从未发放试用额度。上游那句"退出登录再登录"并不够——
		// 实测重登后仍 14017；必须去站点补完资料页，试用立刻到账（350 积分）。
		h.cfg.Pool.Disable(acct.Name,
			"未激活免费试用（上游 14017）——该账号还没走完站点入驻：请用浏览器打开 workbuddy.ai 完成「完善你的资料」（确认国家/地区，仅需一次），完成后回来重登此账号即可")
		log.Printf("upstream trial not activated account=%s model=%s msg=%s",
			acct.Name, model, truncateText(msg, 140))
		return true
	case upstream.TencentErrClientSide:
		log.Printf("upstream client-side rejection (no penalty) account=%s model=%s msg=%s",
			acct.Name, model, truncateText(msg, 140))
		return true
	}
	return false
}

// handleUpstreamError 按上游错误分类结算账号：401 禁用、429 软冷却、
// 并发会话上限仅记日志（瞬时）、5xx 硬冷却、其余累计错误计数。
// 腾讯家族先按实证语义分类（SPEC §28.4 决策 B）：硬额度/会话死亡专属动作。
// 传输层错误（非 ApiError，如 tls bad record MAC / 连接重置）视为瞬时：
// 只记日志并轮换账号，不计错误数、不冷却（网络抖动不该惩罚账号）。
func (h *Handler) handleUpstreamError(acct *pool.Account, model string, err error) {
	var ae *upstream.ApiError
	if !errors.As(err, &ae) {
		// 传输层错误（EOF / TLS 握手超时 / 连接重置）是瞬时网络抖动：只记日志并轮换账号，
		// 不计错误数、不冷却（换账号走的是同一条网络，罚它没有意义）。
		// 但要在池子上留观测痕迹——否则客户端报错而面板一片干净，用户无从判断
		// （2026-09-29 用户报"似乎限流但 dashboard 无反应"就是这个盲区）。
		log.Printf("upstream transport error (transient) account=%s err=%v", acct.Name, err)
		h.cfg.Pool.NoteTransient(acct.Name, err.Error())
		return
	}
	if acct.ProfileID == "workbuddy" {
		// 先过腾讯专属三态（封禁/限流文案/请求侧）：它们必须先于"模型级 6004"与通用分支
		if h.settleTencentKind(acct, model, ae.Message) {
			return
		}
		switch upstream.ClassifyTencent(ae.Status, ae.Message) {
		case upstream.TencentErrHardCredit:
			h.cfg.Pool.CooldownUntilTomorrow4AM(acct.Name, ae.Error())
			return
		case upstream.TencentErrSessionDead:
			h.cfg.Pool.Disable(acct.Name, ae.Error()+" (re-login via login-tencent)")
			return
		case upstream.TencentErrModelRateLimit:
			h.settleModelRateLimit(acct, model, ae.Message)
			return
		}
	}
	// 请求参数非法（华为 InferHub.001001005）是客户端侧问题：只记日志、不计错误、
	// 不冷却账号（换号也没用——同家族账号必然同样拒绝）。调用方在聊天循环里
	// 已按 isRequestParamError 直接回 400 给客户端，这里是兜底。
	if isRequestParamError(ae.Error()) {
		log.Printf("upstream request param error (client-side, no penalty) account=%s model=%s err=%s",
			acct.Name, model, truncateText(ae.Error(), 200))
		return
	}
	switch {
	case ae.Status == 401 || ae.Code == 401:
		h.cfg.Pool.Disable(acct.Name, "401 "+ae.Message)
	case ae.Status == 429:
		// 无模型级证据的 429：按账号软冷却（可能账号级/渠道级，无法定位到模型）
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, h.cfg.SoftCooldown, ae.Error())
	case ae.Status == 400 && isConcurrentLimitError(ae.Message):
		// 并发会话上限（TM.00001041）是瞬时错误：上游会话槽位会被其他请求释放，
		// 不冷却账号——池的并发锁已防止过载，冷却反而误伤后续请求。
		log.Printf("upstream concurrent limit (transient) account=%s msg=%s", acct.Name, truncateText(ae.Message, 80))
	case ae.Status >= 500 && isUpstreamBackendTimeout(ae.Error()):
		// 上游网关的"后端超时"（华为 APIG.0203 Backend timeout）是**上游侧慢**，不是账号
		// 故障——按瞬时处理：短软冷却，不判账号死。对"单账号家族"尤其重要：一次后端超时
		// 不该让整条渠道停 10 分钟（2026-09-27 实测：一个华为号 + 一次 504 = 10 分钟
		// no_healthy_account；其前因是并发超上游会话上限导致的压力）。
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, h.cfg.SoftCooldown, ae.Error())
		log.Printf("upstream backend timeout (transient, soft cooldown) account=%s model=%s msg=%s",
			acct.Name, model, truncateText(ae.Message, 120))
	case ae.Status >= 500:
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolErr, h.cfg.ErrCooldown, ae.Error())
	default:
		if isQuotaError(ae.Error()) {
			// 额度不足（华为 MaaS 福利 InferHub.4291 多为分钟级限流，可自恢复）：
			// 软冷却 60s 不累计；月度配额耗尽时持续失败由面板/日志暴露，换模型即可
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, time.Minute, ae.Error())
			return
		}
		// 其余（含 403 等无专属分支的状态码）走累计熔断。**必须把上游原话记下来**：
		// 2026-09-27 排查腾讯 403 时发现，这条路径此前只累计不记原因，日志里只剩
		// 状态码，等于把唯一的线索丢了（面板「原因」也只写 consecutive errors）。
		log.Printf("upstream error account=%s model=%s status=%d msg=%s",
			acct.Name, model, ae.Status, truncateText(ae.Message, 200))
		h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	}
}

// isUpstreamBackendTimeout 上游网关的"后端超时"文案（华为 APIG.0203 / Backend timeout）。
// 语义：网关等不到后端（模型）在超时内返回——**上游侧慢**，与账号健康无关。
// 故按瞬时处理（短软冷却），而不是 5xx 默认的硬冷却。
func isUpstreamBackendTimeout(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "apig.0203") || strings.Contains(low, "backend timeout")
}

func isConcurrentLimitError(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "tm.00001041") || strings.Contains(low, "并发会话")
}
