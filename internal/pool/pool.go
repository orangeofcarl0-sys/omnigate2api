// Package pool 管理 CodeArts 账号池：token 校验/自动刷新、冷却、禁用与轮转。
package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/upstream"
)

// CoolKind 冷却原因。
type CoolKind int

const (
	CoolSoft CoolKind = iota
	CoolErr
	CoolHard // 硬额度（腾讯 402/积分不足）：冷却至次日 04:00（SPEC §28.4 决策 B）
	CoolNone // 复位用哨兵：无冷却
)

// Account 一个上游账号。
type Account struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	UserName     string `json:"user_name"`
	DefaultModel string `json:"default_model"`

	Auth   *auth.Auth       `json:"-"`
	Client upstream.ChatAPI `json:"-"`
	// ProfileID 账号归属的上游（SPEC §24.1）：决定客户端/刷新/调度分支
	ProfileID string

	mu            sync.Mutex
	quota         AccountQuota // 额度快照（华为活动 token 余额 / 腾讯积分）
	lastValidated time.Time
	errCount      int
	coolUntil     time.Time
	coolKind      CoolKind
	// modelCool 按 (账号, 模型) 的冷却截止时刻（SPEC §28.4 决策 B 补充）：
	// 上游的 429+code 6004 是**模型级**限流（文案自证"可切换其他模型继续使用"），
	// 把整账号冷却会平白丢掉该账号对其余模型的容量，故按模型维度记账。
	modelCool         map[string]time.Time
	disabled          bool
	disabledReason    string
	lastErr           string
	activeConcurrent  int       // 当前活跃并发请求数
	maxConcurrent     int       // 最大允许并发数
	keepaliveLastPing time.Time // 最后保活心跳时间
}

// AccountQuota 账号额度快照（面板展示用，非持久化状态）。
type AccountQuota struct {
	Total     int64 `json:"total,omitempty"` // 总额（华为活动额度；腾讯无）
	Used      int64 `json:"used,omitempty"`  // 已用（华为）
	Remain    int64 `json:"remain"`          // 剩余
	UpdatedAt int64 `json:"updated_at"`      // 查询时间（Unix）
}

// SetQuota 写回额度快照（华为 benefit / 腾讯积分查询后落账）。
func (a *Account) SetQuota(q AccountQuota) {
	a.mu.Lock()
	a.quota = q
	a.mu.Unlock()
}

// Config 池配置。
type Config struct {
	ErrThreshold    int
	ErrCooldown     time.Duration
	SoftCooldown    time.Duration
	RefreshSkew     time.Duration // token 到期前多久自动刷新
	MaxConcurrent   int           // 单账号最大并发数（默认 5）
	KeepaliveWindow time.Duration // 保活心跳窗口，超过此时间无活动则发送心跳
}

// Pool 账号池。
type Pool struct {
	cfg      Config
	state    string
	mu       sync.Mutex
	accounts []*Account
}

// New 构建池。
func New(auths []*auth.Auth, cfg Config, stateFile string) (*Pool, error) {
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 30 * time.Minute
	}
	if cfg.MaxConcurrent <= 0 {
		// 上游单账号最多 3 个并发会话（TM.00001041），但会话槽位释放慢，
		// 串行（1）最稳，避免撞限。可按多账号情况调高。
		cfg.MaxConcurrent = 1
	}
	if cfg.KeepaliveWindow <= 0 {
		cfg.KeepaliveWindow = 10 * time.Minute // 10 分钟无活动则保活
	}
	p := &Pool{cfg: cfg, state: stateFile}
	for _, a := range auths {
		p.accounts = append(p.accounts, newAccount(a, cfg.MaxConcurrent))
	}
	p.loadState()
	return p, nil
}

// newAccount 按凭证 Profile 构造账号：客户端按上游家族分发
// （华为签名版 / 腾讯 Bearer 版，SPEC §24.1）。
func newAccount(a *auth.Auth, maxConcurrent int) *Account {
	acct := &Account{
		Name:              a.UserID,
		UID:               a.UserID,
		UserName:          a.UserName,
		Auth:              a,
		ProfileID:         "codearts",
		maxConcurrent:     maxConcurrent,
		keepaliveLastPing: time.Now(),
	}
	if a.Profile == "workbuddy" {
		acct.ProfileID = "workbuddy"
		acct.Client = upstream.NewTencent(120 * time.Second)
	} else {
		acct.Client = upstream.New(120 * time.Second)
	}
	return acct
}

// modelCoolingList 生效中的模型冷却（面板展示用，按到期时间排序，附剩余分钟）。
func modelCoolingList(m map[string]time.Time, now time.Time) []map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(m))
	for model, until := range m {
		if !now.Before(until) {
			continue
		}
		out = append(out, map[string]any{
			"model": model, "until": until.Format(time.RFC3339),
			"in_min": int(until.Sub(now).Minutes()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["until"].(string) < out[j]["until"].(string) })
	return out
}

// truncateReason 日志用短文案（避免把整段上游 body 写进日志）。
func truncateReason(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// Accounts 返回全部账号。
func (p *Pool) Accounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accounts
}

// Get 按名字取账号。
func (p *Pool) Get(name string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// AddAccount 动态添加账号（WebUI 登录成功后调用）。
func (p *Pool) AddAccount(a *auth.Auth) *Account {
	acct := newAccount(a, p.cfg.MaxConcurrent)
	p.mu.Lock()
	for i, existing := range p.accounts {
		if existing.Name == acct.Name {
			existing.mu.Lock()
			existing.Auth = a
			existing.disabled = false
			existing.disabledReason = ""
			existing.lastErr = ""
			existing.mu.Unlock()
			p.mu.Unlock()
			p.saveState()
			return existing
		}
		p.accounts[i] = existing
	}
	p.accounts = append(p.accounts, acct)
	p.mu.Unlock()
	p.saveState()
	log.Printf("pool account added name=%s uid=%s", acct.Name, a.UserID)
	return acct
}

// List 状态快照（脱敏；字段对齐 WorkBuddy 面板）。
func (p *Pool) List() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.accounts))
	now := time.Now()
	for _, a := range p.accounts {
		a.mu.Lock()
		cooling := !a.disabled && now.Before(a.coolUntil)
		until := ""
		if cooling {
			until = a.coolUntil.Format(time.RFC3339)
		}
		reason := a.disabledReason
		if reason == "" {
			reason = a.lastErr
		}
		out = append(out, map[string]any{
			"name":              a.Name,
			"uid":               a.UID,
			"nickname":          a.UserName,
			"user_name":         a.UserName,
			"family":            a.ProfileID, // 渠道标识（codearts/workbuddy；账号不跨渠道互通）
			"default_model":     a.DefaultModel,
			"credits":           a.quota.Remain,
			"quota_total":       a.quota.Total,
			"quota_used":        a.quota.Used,
			"quota_updated_at":  a.quota.UpdatedAt,
			"disabled":          a.disabled,
			"cooling":           cooling,
			"until":             until,
			"err_count":         a.errCount,
			"reason":            reason,
			"last_error":        a.lastErr,
			"model_cooling":     modelCoolingList(a.modelCool, now),
			"active_concurrent": a.activeConcurrent,
			"max_concurrent":    a.maxConcurrent,
			"token_remaining":   a.Auth.Remaining().Round(time.Minute).String(),
			"expires_at":        a.Auth.ExpiresAt().Format(time.RFC3339),
		})
		a.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["uid"].(string) < out[j]["uid"].(string) })
	return out
}

// Stats 汇总。
// Stats 汇总账号健康分布（total/healthy/disabled/cooling）。
// 注：历史第 5 返回值名义为 credits、实为健康数（面板曾复用该位），已删除——
// 额度真值走 List()（credits/quota_total/quota_used）。
func (p *Pool) Stats() (total, healthy, disabled, cooling int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accounts {
		total++
		a.mu.Lock()
		if a.disabled {
			disabled++
		} else if now.Before(a.coolUntil) {
			cooling++
		} else {
			healthy++
		}
		a.mu.Unlock()
	}
	return
}

// Enable 解禁账号。
func (p *Pool) Enable(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.disabled = false
			a.disabledReason = ""
			a.lastErr = ""
			a.clearModelCools()
			a.mu.Unlock()
			p.saveState()
			return true
		}
	}
	return false
}

// CoolModel 冷却某账号的某个模型对（到 until 为止）；其余模型不受影响。
// reason 记入账号 lastErr 便于面板/日志观察（不改账号级冷却与禁用状态）。
func (p *Pool) CoolModel(name, model string, until time.Time, reason string) {
	if model == "" {
		return
	}
	key := strings.ToLower(strings.TrimSpace(model))
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name && a.UID != name {
			continue
		}
		a.mu.Lock()
		if a.modelCool == nil {
			a.modelCool = map[string]time.Time{}
		}
		a.modelCool[key] = until
		a.lastErr = reason
		a.mu.Unlock()
		log.Printf("pool model cooldown account=%s model=%s until=%s reason=%s",
			name, key, until.Format(time.RFC3339), truncateReason(reason))
		p.saveState()
		return
	}
}

// ModelCooled 该账号的该模型当前是否在冷却中。
func (p *Pool) ModelCooled(name, model string) bool {
	key := strings.ToLower(strings.TrimSpace(model))
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name && a.UID != name {
			continue
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		until, ok := a.modelCool[key]
		return ok && time.Now().Before(until)
	}
	return false
}

// ModelCools 该账号全部生效中的模型冷却（模型 → 截止时刻；已过期的不返回）。
func (p *Pool) ModelCools(name string) map[string]time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := map[string]time.Time{}
	for _, a := range p.accounts {
		if a.Name != name && a.UID != name {
			continue
		}
		a.mu.Lock()
		for m, until := range a.modelCool {
			if now.Before(until) {
				out[m] = until
			}
		}
		a.mu.Unlock()
		return out
	}
	return out
}

// clearModelCools 清空某账号的按模型冷却（账号级"清冷却/启用"时一并清）。
func (a *Account) clearModelCools() { a.modelCool = nil }

// ClearCooldown 清冷却（账号级 + 按模型）。
func (p *Pool) ClearCooldown(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.coolUntil = time.Time{}
			a.coolKind = CoolSoft
			a.clearModelCools()
			a.mu.Unlock()
			p.saveState()
			return true
		}
	}
	return false
}

// SyncToDir 用磁盘 auths 全量对齐。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	byUID := map[string]*Account{}
	for _, a := range p.accounts {
		byUID[a.UID] = a
	}
	next := make([]*Account, 0, len(auths))
	for _, a := range auths {
		if existing, ok := byUID[a.UserID]; ok {
			existing.mu.Lock()
			existing.Auth = a
			existing.UserName = a.UserName
			existing.mu.Unlock()
			next = append(next, existing)
			delete(byUID, a.UserID)
		} else {
			next = append(next, newAccount(a, p.cfg.MaxConcurrent))
		}
	}
	p.accounts = next
}

// PickFor 按客户端家族挑一个健康账号（SPEC §24.1）；family 为空 = 全池。
func (p *Pool) PickFor(family string, tried map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accounts {
		if family != "" && a.ProfileID != family {
			continue
		}
		if tried != nil && tried[a.Name] {
			continue
		}
		a.mu.Lock()
		healthy := !a.disabled && now.After(a.coolUntil)
		a.mu.Unlock()
		if healthy {
			return a
		}
	}
	return nil
}

// PickForModel 按家族 + 模型选号：账号健康 **且该模型未在该账号上冷却**。
// 这是模型级限流（6004）下的正确轮换口径——别再因为一个模型被限就把整个账号跳过。
func (p *Pool) PickForModel(family, model string, tried map[string]bool) *Account {
	key := strings.ToLower(strings.TrimSpace(model))
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var fallback *Account // 所有账号该模型都在冷却时的兜底（可能已解封但表未清）
	for _, a := range p.accounts {
		if family != "" && a.ProfileID != family {
			continue
		}
		if tried != nil && tried[a.Name] {
			continue
		}
		a.mu.Lock()
		healthy := !a.disabled && now.After(a.coolUntil)
		var pairCooling bool
		if key != "" {
			if until, ok := a.modelCool[key]; ok {
				pairCooling = now.Before(until)
			}
		}
		a.mu.Unlock()
		if !healthy {
			continue
		}
		if pairCooling {
			if fallback == nil {
				fallback = a
			}
			continue
		}
		return a
	}
	return fallback
}

// PickExcluding 挑一个健康账号（全池，兼容既有调用）。
func (p *Pool) PickExcluding(tried map[string]bool) *Account {
	return p.PickFor("", tried)
}

// Validate 校验并（必要时）自动刷新 token。带 5 分钟缓存。
func (p *Pool) Validate(a *Account) (bool, error) {
	a.mu.Lock()
	if time.Since(a.lastValidated) < 5*time.Minute && !a.disabled {
		ok := true
		a.mu.Unlock()
		return ok, nil
	}
	a.mu.Unlock()

	authz := a.Auth
	if authz.ExpiringSoon(p.cfg.RefreshSkew) || authz.Expired() {
		if authz.Refresh() == "" {
			// 没有 refresh_token（华为云 ticket 通道不返回），不立即 disable，
			// 只打告警日志。token 过期后上游 401 会自然 disable。
			log.Printf("pool token account=%s expiring soon (remaining=%s) but no refresh_token, re-login required",
				a.Name, authz.Remaining().Round(time.Minute))
			a.mu.Lock()
			a.lastValidated = time.Now()
			a.mu.Unlock()
			return true, nil
		}
		cfg := upstream.DefaultLoginConfig()
		resp, err := a.Client.RefreshToken(context.Background(), cfg, authz.Refresh(), authz.Verifier(), authz.Domain)
		if err != nil {
			p.Disable(a.Name, "refresh failed: "+err.Error())
			return false, nil
		}
		if err := applyAuthCreds(authz, resp); err != nil {
			log.Printf("pool token refresh save failed account=%s err=%v", a.Name, err)
		}
		log.Printf("pool token refreshed account=%s", a.Name)
	}
	a.mu.Lock()
	a.lastValidated = time.Now()
	// 凭证有效即视为可用：复位历史的 disabled/冷却持久化状态。
	// 用户重新登录换新凭证后，旧 disabled 状态不应继续拦截请求。
	if a.disabled || time.Now().Before(a.coolUntil) {
		a.disabled = false
		a.coolUntil = time.Time{}
		a.coolKind = CoolNone
		a.lastErr = ""
	}
	a.mu.Unlock()
	return true, nil
}

// Cooldown 冷却账号。
func (p *Pool) Cooldown(name string, kind CoolKind, dur time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		a.coolUntil = time.Now().Add(dur)
		a.coolKind = kind
		a.lastErr = reason
		a.mu.Unlock()
		log.Printf("pool cooldown account=%s kind=%d dur=%s reason=%s", name, kind, dur, reason)
	}
	p.saveState()
}

// CooldownUntilTomorrow4AM 硬额度冷却：冷却到本地次日 04:00（对齐
// workbuddy2api 实证；与每日积分刷新节奏解耦，次日即解禁重试）。
func (p *Pool) CooldownUntilTomorrow4AM(name, reason string) {
	now := time.Now()
	next4AM := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, time.Local).AddDate(0, 0, 1)
	p.Cooldown(name, CoolHard, next4AM.Sub(now), reason)
}

// NoteError 累计错误。
func (p *Pool) NoteError(name string, threshold int, cooldown time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		a.errCount++
		if a.errCount >= threshold {
			a.coolUntil = time.Now().Add(cooldown)
			a.lastErr = "consecutive errors"
		}
		a.mu.Unlock()
	}
	p.saveState()
}

// NoteSuccess 清零错误计数与上次错误文案。
// 必须一并清 lastErr：否则面板「原因」列会在健康账号上长期显示历史错误
// （如 "consecutive errors"），把已恢复的账号显示成有问题——误导排障。
// disabledReason 仅在未禁用时清（禁用账号的成功回调不该抹掉禁用原因）。
func (p *Pool) NoteSuccess(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.errCount = 0
			a.lastErr = ""
			if !a.disabled {
				a.disabledReason = ""
			}
			a.mu.Unlock()
		}
	}
}

// Disable 禁用账号。
func (p *Pool) Disable(name, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.disabled = true
			a.disabledReason = reason
			a.lastErr = reason
			a.mu.Unlock()
			log.Printf("pool disable account=%s reason=%s", name, reason)
		}
	}
	p.saveState()
}

// Healthy 查询账号可用。
func (p *Pool) Healthy(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			h := !a.disabled && time.Now().After(a.coolUntil)
			a.mu.Unlock()
			return h
		}
	}
	return false
}

// AcquireLock 尝试获取账号的并发请求锁。返回 true 表示成功，false 表示已达上限。
func (p *Pool) AcquireLock(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.activeConcurrent >= a.maxConcurrent {
			log.Printf("pool concurrent limit reached account=%s current=%d max=%d",
				name, a.activeConcurrent, a.maxConcurrent)
			return false
		}
		a.activeConcurrent++
		return true
	}
	return false
}

// ReleaseLock 释放账号的并发请求锁；等待者由 AcquireLockWait 轮询发现，无需广播唤醒。
func (p *Pool) ReleaseLock(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			if a.activeConcurrent > 0 {
				a.activeConcurrent--
			}
			a.mu.Unlock()
			break
		}
	}
}

// AcquireLockWait 阻塞等待账号并发槽位，超时返回 false。
//
// 上游单账号并发会话上限 3（TM.00001041），本池 maxConcurrent 默认 1 串行最稳。
// 高并发请求在此排队等待槽位释放，而非立即失败跳号。
// timeout=0 时等价于非阻塞 AcquireLock。
//
// 有意采用 100ms 轮询而非事件驱动（sync.Cond / 每账号 channel 信号量）：
//   - 轮询对「释放与检查之间的竞态」天然免疫，不存在丢失唤醒导致的死等；
//   - sync.Cond 超时需 timer + 先持锁再 Wait，且 Broadcast 会惊群唤醒
//     全部账号的等待者，复杂度与出错面都更大；
//   - 代价是槽位释放后最多 100ms（平均 50ms）才被抢到，对本服务秒级
//     请求可忽略；CPU 为每等待者 10 次/秒 × 微秒级检查。
//     若将来出现大量并发排队或亚百毫秒延迟要求，再改为 channel 信号量
//     （len(sem) 即并发数），并先补一个并发排队集成测试后再换。
func (p *Pool) AcquireLockWait(name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		p.mu.Lock()
		acquired := false
		for _, a := range p.accounts {
			if a.Name != name {
				continue
			}
			a.mu.Lock()
			if a.activeConcurrent < a.maxConcurrent {
				a.activeConcurrent++
				acquired = true
			}
			a.mu.Unlock()
			break
		}
		p.mu.Unlock()
		if acquired {
			return true
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond) // 轮询间隔（取舍见函数头注释）
	}
}

// NeedKeepalive 检查账号是否需要保活心跳。
func (p *Pool) NeedKeepalive(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		// 超过 KeepaliveWindow 无活动则需保活
		return time.Since(a.keepaliveLastPing) > p.cfg.KeepaliveWindow
	}
	return false
}

// PingKeepalive 更新账号的保活心跳时间。
func (p *Pool) PingKeepalive(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.keepaliveLastPing = time.Now()
			a.mu.Unlock()
			break
		}
	}
}

// CheckAndRefreshToken 检查 token 是否快过期并主动刷新（比 scheduler 更积极）。
func (p *Pool) CheckAndRefreshToken(name string) error {
	p.mu.Lock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.Unlock()
	if acct == nil {
		return fmt.Errorf("account not found: %s", name)
	}

	acct.mu.Lock()
	remaining := acct.Auth.Remaining()
	acct.mu.Unlock()

	// 如果 token 剩余少于 1 小时，主动刷新
	if remaining <= time.Hour {
		log.Printf("pool proactive refresh account=%s remaining=%s", name, remaining)
		return p.RefreshToken(name)
	}
	return nil
}

// RefreshToken 手动刷新指定账号的 token。
func (p *Pool) RefreshToken(name string) error {
	p.mu.Lock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.Unlock()
	if acct == nil {
		return fmt.Errorf("account not found: %s", name)
	}

	authz := acct.Auth
	if authz.Refresh() == "" {
		return fmt.Errorf("no refresh_token available")
	}

	cfg := upstream.DefaultLoginConfig()
	resp, err := acct.Client.RefreshToken(context.Background(), cfg, authz.Refresh(), authz.Verifier(), authz.Domain)
	if err != nil {
		return fmt.Errorf("refresh failed: %w", err)
	}
	if err := applyAuthCreds(authz, resp); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	log.Printf("pool token refreshed manually account=%s", name)
	return nil
}

// applyAuthCreds 刷新结果落回凭证并保存（Validate 内联刷新与 RefreshToken 共用）。
func applyAuthCreds(authz *auth.Auth, resp *upstream.TokenResponse) error {
	authz.CloudDragonTok = resp.Credentials.SecurityToken
	authz.AccessKeyID = resp.Credentials.AccessKeyID
	authz.SecretAccessKey = resp.Credentials.SecretAccessKey
	authz.Expiration = resp.Credentials.Expiration
	if resp.RefreshToken != "" {
		authz.RefreshToken = resp.RefreshToken
	}
	authz.UpdatedAt = time.Now().Unix()
	return authz.Save()
}

func (p *Pool) loadState() {
	if p.state == "" {
		return
	}
	raw, err := os.ReadFile(p.state)
	if err != nil {
		return
	}
	var data []struct {
		Name      string               `json:"name"`
		ErrCount  int                  `json:"err_count"`
		CoolUntil time.Time            `json:"cool_until"`
		Disabled  bool                 `json:"disabled"`
		Reason    string               `json:"reason"`
		LastErr   string               `json:"last_error"`
		ModelCool map[string]time.Time `json:"model_cool,omitempty"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	for _, a := range p.accounts {
		for _, s := range data {
			if s.Name == a.Name {
				a.mu.Lock()
				a.errCount = s.ErrCount
				a.coolUntil = s.CoolUntil
				a.disabled = s.Disabled
				a.disabledReason = s.Reason
				a.lastErr = s.LastErr
				a.modelCool = s.ModelCool
				a.mu.Unlock()
			}
		}
	}
}

func (p *Pool) saveState() {
	if p.state == "" {
		return
	}
	type entry struct {
		Name      string               `json:"name"`
		ErrCount  int                  `json:"err_count"`
		CoolUntil time.Time            `json:"cool_until"`
		Disabled  bool                 `json:"disabled"`
		Reason    string               `json:"reason"`
		LastErr   string               `json:"last_error"`
		ModelCool map[string]time.Time `json:"model_cool,omitempty"`
	}
	out := make([]entry, 0, len(p.accounts))
	for _, a := range p.accounts {
		a.mu.Lock()
		out = append(out, entry{
			Name:      a.Name,
			ErrCount:  a.errCount,
			CoolUntil: a.coolUntil,
			Disabled:  a.disabled,
			Reason:    a.disabledReason,
			LastErr:   a.lastErr,
			ModelCool: a.modelCool,
		})
		a.mu.Unlock()
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	tmp := p.state + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.state)
}
