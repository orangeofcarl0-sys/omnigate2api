// 登录授权频次闸（SPEC §24.6）。
//
// 为什么需要：面板「发起授权」点一次就会开浏览器，而浏览器通常还登着上游站点 → 授权被
// 自动通过，于是一分钟内能轻松完成多次重登。**2026-09-28 实测事故**：13:57:52–13:58:51
// 对同一账号连续完成 4 次授权，随后登录页开始报「与身份提供程序进行身份验证时出现意外
// 错误 / 账号访问受限」，连健康账号也再登不上（网关 API 侧一切正常）——上游风控把我们的
// 重试当成了异常行为。据此加三道闸，并且**失败后主动停手**：
//
//	① 同渠道最小间隔（默认 60s）——连点无效；
//	② 滚动窗口上限（默认 10 次/小时/渠道）——防"连着点几分钟"；
//	③ 连续未完成 → 冷却（默认连续 3 次未完成 → 冷却 20 分钟）——**最关键的一条**：
//	   登录页一旦开始拒绝，继续重试只会加深风控；这道闸让重试风暴自动终止。
//
// 渠道键：`huawei` / `tencent:cn` / `tencent:global`（各自的 state 互不影响，分别计频）。
// 判定是**惰性**的：每次 allow 时先把超过 TTL 仍未完成的会话结算成一次失败，不需要定时器。
package server

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// loginGuardConfig 闸门参数（默认值见 defaultLoginGuardConfig；可用环境变量覆盖）。
type loginGuardConfig struct {
	MinInterval  time.Duration // 同渠道两次发起之间的最小间隔
	Window       time.Duration // 滚动窗口长度
	MaxPerWindow int           // 窗口内最多发起次数
	// FailureThreshold 连续"发起但没完成"多少次后冷却。固定值（不做成开关）：
	// 这是失败自停的耐久设计，调大就等于允许重试风暴。
	FailureThreshold int
	FailureCooldown  time.Duration
}

func defaultLoginGuardConfig() loginGuardConfig {
	c := loginGuardConfig{
		MinInterval:      60 * time.Second,
		Window:           time.Hour,
		MaxPerWindow:     10,
		FailureThreshold: 3,
		FailureCooldown:  20 * time.Minute,
	}
	if v := envSeconds("OMNIGATE_LOGIN_MIN_INTERVAL_SECONDS"); v > 0 {
		c.MinInterval = v
	}
	if v := envInt("OMNIGATE_LOGIN_MAX_PER_HOUR"); v > 0 {
		c.MaxPerWindow = v
	}
	if v := envSeconds("OMNIGATE_LOGIN_FAILURE_COOLDOWN_SECONDS"); v > 0 {
		c.FailureCooldown = v
	}
	return c
}

func envSeconds(key string) time.Duration {
	if n := envInt(key); n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

func envInt(key string) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// guardDecision allow() 的结论。
type guardDecision struct {
	OK         bool
	RetryAfter time.Duration // 被拒时还要等多久
	Reason     string
}

// openLogin 一次已发起、尚未完成的登录会话。
type openLogin struct {
	channel string
	started time.Time
	ttl     time.Duration
}

// loginGuard 登录授权频次闸（进程内状态；重启即清空——重启不是高频动作，可接受）。
type loginGuard struct {
	cfg loginGuardConfig

	mu       sync.Mutex
	history  map[string][]time.Time // channel → 窗口内的发起时间
	last     map[string]time.Time   // channel → 最近一次发起时间
	open     map[string]*openLogin  // channel → 进行中的会话
	streak   map[string]int         // channel → 连续未完成次数
	coolTill map[string]time.Time   // channel → 冷却截止
	coolWhy  map[string]string
	lastDone map[string]doneLogin // channel → 最近一次成功登录（uid/时间，用于提示）
}

type doneLogin struct {
	uid string
	at  time.Time
}

func newLoginGuard(cfg loginGuardConfig) *loginGuard {
	return &loginGuard{
		cfg:      cfg,
		history:  map[string][]time.Time{},
		last:     map[string]time.Time{},
		open:     map[string]*openLogin{},
		streak:   map[string]int{},
		coolTill: map[string]time.Time{},
		coolWhy:  map[string]string{},
		lastDone: map[string]doneLogin{},
	}
}

// allow 判断现在能否为该渠道发起一次授权。会先把已超时未完成的会话结算成失败（惰性）。
func (g *loginGuard) allow(channel string, now time.Time) guardDecision {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settleLocked(channel, now)

	if until, ok := g.coolTill[channel]; ok {
		if now.Before(until) {
			why := g.coolWhy[channel]
			return guardDecision{OK: false, RetryAfter: until.Sub(now), Reason: fmt.Sprintf(
				"%s；已暂停该渠道的授权请求 %s（防止把账号推向上游风控）", why, humanDur(until.Sub(now)))}
		}
		delete(g.coolTill, channel)
		delete(g.coolWhy, channel)
	}

	// 同渠道同时只允许一个未完成的授权：否则"发起→没完成→再发起"会不断覆盖上一个会话，
	// 失败永远不被计数，连续失败冷却就形同虚设（2026-09-28 重试风暴正是这个形态）。
	if o, ok := g.open[channel]; ok && o != nil {
		left := o.ttl - now.Sub(o.started)
		if left < 0 {
			left = 0
		}
		return guardDecision{OK: false, RetryAfter: left, Reason: fmt.Sprintf(
			"上一次授权还没结束（还剩 %s 超时）——请先在浏览器完成它；若已放弃，等它超时后再试"+
				"（连续重复发起会被上游判为异常登录）", humanDur(left))}
	}

	if last, ok := g.last[channel]; ok {
		if wait := g.cfg.MinInterval - now.Sub(last); wait > 0 {
			return guardDecision{OK: false, RetryAfter: wait, Reason: fmt.Sprintf(
				"距上次发起授权仅 %s，请等 %s 再试（重复授权会被上游判为异常登录）",
				humanDur(now.Sub(last)), humanDur(wait))}
		}
	}

	gprune := g.pruneLocked(channel, now)
	if len(gprune) >= g.cfg.MaxPerWindow {
		oldest := gprune[0]
		wait := g.cfg.Window - now.Sub(oldest)
		return guardDecision{OK: false, RetryAfter: wait, Reason: fmt.Sprintf(
			"%s 内已发起 %d 次授权（上限 %d），请等 %s 再试",
			humanDur(g.cfg.Window), len(gprune), g.cfg.MaxPerWindow, humanDur(wait))}
	}
	return guardDecision{OK: true}
}

// recordStart 记一次发起。调用方必须已通过 allow（那里保证没有未完成会话）；若仍有
// （理论上不该发生），先补记一次失败再覆盖——宁可多算失败，也不能漏计。
func (g *loginGuard) recordStart(channel string, ttl time.Duration, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if o, ok := g.open[channel]; ok && o != nil {
		g.noteFailureLocked(channel, "上一次授权未完成就重新发起", now)
	}
	g.history[channel] = append(g.history[channel], now)
	g.last[channel] = now
	g.open[channel] = &openLogin{channel: channel, started: now, ttl: ttl}
}

// recordDone 记一次成功登录（清连续失败计数；记住 uid 供面板提示"刚登过"）。
func (g *loginGuard) recordDone(channel, uid string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.open, channel)
	g.streak[channel] = 0
	g.lastDone[channel] = doneLogin{uid: uid, at: now}
}

// recordFailed 记一次明确失败（换码失败等）——立即计入连续失败，不等 TTL。
func (g *loginGuard) recordFailed(channel, why string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.open, channel)
	g.noteFailureLocked(channel, why, now)
}

// recentLogin 最近一次成功登录（面板提示用；同渠道）。
func (g *loginGuard) recentLogin(channel string, now time.Time) (string, time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.lastDone[channel]
	if !ok {
		return "", 0, false
	}
	return d.uid, now.Sub(d.at), true
}

// settleLocked 把"发起后超过 TTL 仍未完成"的会话结算成一次失败：
// 这正是"登录页报错 → 用户/面板再点一次"的形态，必须计入而非忽略。
func (g *loginGuard) settleLocked(channel string, now time.Time) {
	o, ok := g.open[channel]
	if !ok || o == nil {
		return
	}
	if now.Sub(o.started) < o.ttl {
		return
	}
	delete(g.open, channel)
	g.noteFailureLocked(channel, "上一次授权一直没有完成（登录页可能已开始拒绝）", now)
}

// noteFailureLocked 累计连续失败，达到阈值就开冷却（并清零计数，冷却期满可再试一次）。
func (g *loginGuard) noteFailureLocked(channel, why string, now time.Time) {
	g.streak[channel]++
	if g.streak[channel] < g.cfg.FailureThreshold {
		return
	}
	g.streak[channel] = 0
	g.coolTill[channel] = now.Add(g.cfg.FailureCooldown)
	g.coolWhy[channel] = fmt.Sprintf("连续 %d 次授权未完成，暂停以避开上游风控", g.cfg.FailureThreshold)
}

// pruneLocked 清掉窗口外的时间戳并返回窗口内的（升序）。
func (g *loginGuard) pruneLocked(channel string, now time.Time) []time.Time {
	cut := now.Add(-g.cfg.Window)
	src := g.history[channel]
	kept := src[:0]
	for _, t := range src {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	g.history[channel] = kept
	return kept
}

// humanDur 人类可读的短时长（1m30s / 45s / 2h5m）。
func humanDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
