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

// modelRateLimitHorizon 模型级限流冷却上限：上游给出的解封时刻可能很远（甚至误报），
// 钳住以免一对 (账号,模型) 被长期废掉——到期自然重试，真限流会再次触发。
const modelRateLimitHorizon = 24 * time.Hour

// settleModelRateLimit 模型级限流结算（429 + code 6004）：**只冷却 (账号, 模型)**，
// 不动账号级健康——上游文案自证"可切换其他模型继续使用"，整账号冷却会平白丢掉该账号
// 对其余模型的容量；解封时刻优先取上游声明值。
func (h *Handler) settleModelRateLimit(acct *pool.Account, model, msg string) {
	until := upstream.ModelRateLimitUntil(msg, time.Now(), h.cfg.SoftCooldown, modelRateLimitHorizon)
	h.cfg.Pool.CoolModel(acct.Name, model, until, msg)
	log.Printf("upstream model rate limit account=%s model=%s until=%s msg=%s",
		acct.Name, model, until.Format(time.RFC3339), truncateText(msg, 140))
}

// handleUpstreamError 按上游错误分类结算账号：401 禁用、429 软冷却、
// 并发会话上限仅记日志（瞬时）、5xx 硬冷却、其余累计错误计数。
// 腾讯家族先按实证语义分类（SPEC §28.4 决策 B）：硬额度/会话死亡专属动作。
// 传输层错误（非 ApiError，如 tls bad record MAC / 连接重置）视为瞬时：
// 只记日志并轮换账号，不计错误数、不冷却（网络抖动不该惩罚账号）。
func (h *Handler) handleUpstreamError(acct *pool.Account, model string, err error) {
	var ae *upstream.ApiError
	if !errors.As(err, &ae) {
		// 传输层错误（tls bad record MAC / 连接重置等）是瞬时网络抖动：
		// 只记日志并轮换账号，不计错误数、不冷却。
		log.Printf("upstream transport error (transient) account=%s err=%v", acct.Name, err)
		return
	}
	if acct.ProfileID == "workbuddy" {
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
	case ae.Status >= 500:
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolErr, h.cfg.ErrCooldown, ae.Error())
	default:
		if isQuotaError(ae.Error()) {
			// 额度不足（华为 MaaS 福利 InferHub.4291 多为分钟级限流，可自恢复）：
			// 软冷却 60s 不累计；月度配额耗尽时持续失败由面板/日志暴露，换模型即可
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, time.Minute, ae.Error())
			return
		}
		h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	}
}
func isConcurrentLimitError(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "tm.00001041") || strings.Contains(low, "并发会话")
}
