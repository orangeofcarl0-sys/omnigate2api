// 错误分类与回合结果判定：客户端断开判定、上游错误分类（冷却/禁用/瞬时重试）。
package server

import (
	"context"
	"errors"
	"log"
	"strings"

	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// isClientCancel 判断错误是否由客户端断开/超时引起。客户端取消不代表上游
// 或账号异常，不应计入错误计数或触发冷却。
func isClientCancel(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// handleUpstreamError 按上游错误分类结算账号：401 禁用、429 软冷却、
// 并发会话上限仅记日志（瞬时）、5xx 硬冷却、其余累计错误计数。
// 腾讯家族先按实证语义分类（SPEC §28.4 决策 B）：硬额度/会话死亡专属动作。
// 传输层错误（非 ApiError，如 tls bad record MAC / 连接重置）视为瞬时：
// 只记日志并轮换账号，不计错误数、不冷却（网络抖动不该惩罚账号）。
func (h *Handler) handleUpstreamError(acct *pool.Account, err error) {
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
		}
	}
	switch {
	case ae.Status == 401 || ae.Code == 401:
		h.cfg.Pool.Disable(acct.Name, "401 "+ae.Message)
	case ae.Status == 429:
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, h.cfg.SoftCooldown, ae.Error())
	case ae.Status == 400 && isConcurrentLimitError(ae.Message):
		// 并发会话上限（TM.00001041）是瞬时错误：上游会话槽位会被其他请求释放，
		// 不冷却账号——池的并发锁已防止过载，冷却反而误伤后续请求。
		log.Printf("upstream concurrent limit (transient) account=%s msg=%s", acct.Name, truncateText(ae.Message, 80))
	case ae.Status >= 500:
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolErr, h.cfg.ErrCooldown, ae.Error())
	default:
		h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	}
}
func isConcurrentLimitError(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "tm.00001041") || strings.Contains(low, "并发会话")
}
