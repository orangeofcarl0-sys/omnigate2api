// Package scheduler 提供 token 自动续期看门狗。
//
// CodeArts Agent 没有每日签到/申请额度接口（免费额度按月重置，见
// codearts.huaweicloud.com/portal/settings/personal-usage）。本调度器
// 周期性校验全部账号，临近过期的 token 自动走 oauth2 refresh_token 续期。
//
// 增强功能：
// - 主动保活：定期发送轻量级请求保持会话活跃
// - 更积极的刷新策略：token 剩余少于 1 小时即主动刷新
package scheduler

import (
	"context"
	"log"
	"time"

	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// Config 调度器配置。
type Config struct {
	Pool              *pool.Pool
	Enabled           bool
	PollInterval      time.Duration    // 默认 30m
	RefreshSkew       time.Duration    // 到期前多久刷新，默认 30m
	KeepaliveInterval time.Duration    // 保活心跳间隔，默认 15m
	Client            *upstream.Client // 福利网关客户端；为 nil 时跳过自动领取
}

// Scheduler 定时任务。
type Scheduler struct {
	cfg              Config
	lastBenefitClaim string // 最近一次福利领取日期（北京时间）
	lastTencentCheck string // 最近一次腾讯签到日期（北京时间）
}

// New 构造调度器。
func New(cfg Config) *Scheduler {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 30 * time.Minute
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = 15 * time.Minute // 15 分钟保活一次
	}
	return &Scheduler{cfg: cfg}
}

// Run 启动定时循环。
func (s *Scheduler) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		log.Printf("token watchdog disabled")
		return
	}
	log.Printf("token watchdog enabled: poll=%s refresh_skew=%s keepalive=%s",
		s.cfg.PollInterval, s.cfg.RefreshSkew, s.cfg.KeepaliveInterval)

	// 立即执行一次
	s.Tick(ctx)

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick 校验全部账号（自动续期 + 保活 + 福利领取）。
func (s *Scheduler) Tick(ctx context.Context) {
	for _, acct := range s.cfg.Pool.Accounts() {
		ok, _ := s.cfg.Pool.Validate(acct)
		if ok {
			remaining := acct.Auth.Remaining().Round(time.Minute)
			log.Printf("token account=%s ok remaining=%s", acct.Name, remaining)

			// 主动保活：如果 token 快过期或长时间无活动，主动刷新
			if err := s.cfg.Pool.CheckAndRefreshToken(acct.Name); err != nil {
				log.Printf("proactive refresh failed account=%s err=%v", acct.Name, err)
			} else if err == nil && remaining <= time.Hour {
				log.Printf("token refreshed account=%s new_remaining=%s", acct.Name,
					acct.Auth.Remaining().Round(time.Minute))
			}
		} else {
			log.Printf("token account=%s invalid/disabled", acct.Name)
		}
	}
	s.claimBenefit(ctx)
	s.claimTencentCheckin(ctx)
}

// benefitTZ 福利额度按北京时间 24 点重置。
var benefitTZ = time.FixedZone("CST", 8*3600)

// claimBenefit 每自然日（北京时间）为各账号领取一次活动福利额度。
// benefit/claim 幂等，重复调用返回既有记录；官方客户端登录时也做同样调用。
func (s *Scheduler) claimBenefit(ctx context.Context) {
	if s.cfg.Client == nil {
		return
	}
	today := time.Now().In(benefitTZ).Format("2006-01-02")
	if today == s.lastBenefitClaim {
		return
	}
	for _, acct := range s.cfg.Pool.Accounts() {
		if acct.ProfileID != "codearts" {
			continue // 福利领取仅华为（SPEC §24.2：腾讯签到本期不做）
		}
		rec, err := s.cfg.Client.ClaimBenefit(ctx, acct.Auth)
		if err != nil {
			log.Printf("benefit claim account=%s failed err=%v", acct.Name, err)
			continue
		}
		if bal, err := s.cfg.Client.FetchTokensBalance(ctx, acct.Auth); err == nil {
			acct.SetQuota(pool.AccountQuota{Remain: bal.TotalBalance, Total: bal.TotalQuota, Used: bal.UsedAmount, UpdatedAt: time.Now().Unix()})
			log.Printf("benefit claim account=%s ok balance=%d/%d used=%d",
				acct.Name, bal.TotalBalance, bal.TotalQuota, bal.UsedAmount)
		} else {
			log.Printf("benefit claim account=%s ok (balance query failed: %v)", acct.Name, err)
		}
		_ = rec
	}
	s.lastBenefitClaim = today
}

// claimTencentCheckin 每自然日（北京时间）为腾讯账号签到一次（幂等）并查询
// 余额日志；与华为福利领取（claimBenefit）并行、互不阻塞（SPEC §24.2 落地）。
func (s *Scheduler) claimTencentCheckin(ctx context.Context) {
	today := time.Now().In(benefitTZ).Format("2006-01-02")
	if today == s.lastTencentCheck {
		return
	}
	for _, acct := range s.cfg.Pool.Accounts() {
		if acct.ProfileID != "workbuddy" {
			continue // 华为侧走 claimBenefit
		}
		api, ok := acct.Client.(upstream.BillingAPI)
		if !ok {
			continue
		}
		if err := api.DailyCheckin(acct.Auth); err != nil {
			log.Printf("tencent checkin account=%s failed err=%v", acct.Name, err)
			continue
		}
		if remain, err := api.UserResource(acct.Auth); err == nil {
			acct.SetQuota(pool.AccountQuota{Remain: remain, UpdatedAt: time.Now().Unix()})
			log.Printf("tencent checkin account=%s ok remaining=%d", acct.Name, remain)
		} else {
			log.Printf("tencent checkin account=%s ok (balance query failed: %v)", acct.Name, err)
		}
	}
	s.lastTencentCheck = today
}
