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
	"errors"
	"fmt"
	"log"
	"strings"
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
	cfg Config
	// lastDaily 每日动作（label → 最近执行日期，北京时间）：华为福利领取 /
	// 腾讯签到共用同日前去重（claimDaily 模板）。
	lastDaily map[string]string
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
	s := &Scheduler{cfg: cfg, lastDaily: map[string]string{}}
	return s
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
	s.growthTasks(ctx) // SPEC §32 阶段 3：任务接单/领奖（积分+能量主来源）
	s.petTravel(ctx)   // SPEC §32.2：宠物探险状态机随 Tick 执行（小时级周期需多次检查）
}

// growthTasks 成长中心任务自动化（SPEC §32 阶段 3）：接单（not_accepted）
// + 领取已完成奖励（completed）——积分与能量主来源，宠物盲盒能量的唯一入口。
// 幂等：accepted/claimed 状态自然跳过；失败只记日志（§32.2 隔离拍板）。
func (s *Scheduler) growthTasks(ctx context.Context) {
	for _, acct := range s.cfg.Pool.Accounts() {
		if acct.ProfileID != "workbuddy" {
			continue
		}
		api, ok := acct.Client.(upstream.BillingAPI)
		if !ok {
			continue
		}
		tasks, err := api.GrowthTasks(acct.Auth)
		if err != nil {
			log.Printf("tencent tasks account=%s failed err=%v", acct.Name, err)
			continue
		}
		var pending []string
		titles := map[string]string{}
		for _, t := range tasks {
			titles[t.TaskCode] = t.Title
			if !t.Locked && t.AcceptStatus == "not_accepted" && t.TaskCode != "" {
				pending = append(pending, t.TaskCode)
			}
		}
		accepted := 0
		for i := 0; i < len(pending); i += 20 {
			batch := pending[i:]
			if len(batch) > 20 {
				batch = batch[:20]
			}
			res, aerr := api.GrowthAcceptTasks(acct.Auth, batch)
			if aerr != nil {
				log.Printf("tencent tasks account=%s action=accept failed err=%v", acct.Name, aerr)
				break
			}
			for code, msg := range res {
				if strings.Contains(msg, "ok") || strings.HasPrefix(msg, "accepted") {
					accepted++
				} else {
					log.Printf("tencent tasks account=%s action=accept code=%s result=%s", acct.Name, code, msg)
				}
			}
			if len(res) == 0 {
				accepted += len(batch)
			}
		}
		var credit, energy int64
		claimed := 0
		for _, t := range tasks {
			if t.Locked || t.AcceptStatus != "completed" || t.TaskCode == "" {
				continue
			}
			cc, ce, already, cerr := api.GrowthClaimTask(acct.Auth, t.TaskCode)
			if cerr != nil {
				log.Printf("tencent tasks account=%s action=claim code=%s failed err=%v", acct.Name, t.TaskCode, cerr)
				continue
			}
			if already {
				continue
			}
			if cc == 0 && ce == 0 {
				cc = t.RewardCredit
				ce = t.RewardEnergy
			}
			credit += cc
			energy += ce
			claimed++
			log.Printf("tencent tasks account=%s action=claim task=%q credit=+%d energy=+%d", acct.Name, t.Title, cc, ce)
		}
		if accepted > 0 || claimed > 0 {
			log.Printf("tencent tasks account=%s summary accepted=%d claimed=%d credit=+%d energy=+%d",
				acct.Name, accepted, claimed, credit, energy)
		}
	}
}

// benefitTZ 福利额度按北京时间 24 点重置。
var benefitTZ = time.FixedZone("CST", 8*3600)

// claimDaily 每自然日（北京时间）为指定家族账号执行一次每日动作；同日前去重。
// fn 返回（日志消息, error）：错误记 failed，空消息跳过日志。各家族分别调用
// （claimBenefit / claimTencentCheckin），并行互不阻塞。
func (s *Scheduler) claimDaily(ctx context.Context, label, family string, fn func(*pool.Account) (string, error)) {
	today := time.Now().In(benefitTZ).Format("2006-01-02")
	if today == s.lastDaily[label] {
		return
	}
	for _, acct := range s.cfg.Pool.Accounts() {
		if acct.ProfileID != family {
			continue
		}
		if msg, err := fn(acct); err != nil {
			log.Printf("%s account=%s failed err=%v", label, acct.Name, err)
		} else if msg != "" {
			log.Printf("%s account=%s %s", label, acct.Name, msg)
		}
	}
	s.lastDaily[label] = today
}

// claimBenefit 每自然日（北京时间）为各华为账号领取一次活动福利额度。
// benefit/claim 幂等，重复调用返回既有记录；官方客户端登录时也做同样调用。
func (s *Scheduler) claimBenefit(ctx context.Context) {
	if s.cfg.Client == nil {
		return
	}
	s.claimDaily(ctx, "benefit claim", "codearts", func(acct *pool.Account) (string, error) {
		if _, err := s.cfg.Client.ClaimBenefit(ctx, acct.Auth); err != nil {
			return "", err
		}
		bal, err := s.cfg.Client.FetchTokensBalance(ctx, acct.Auth)
		if err != nil {
			return "ok (balance query failed: " + err.Error() + ")", nil
		}
		acct.SetQuota(pool.AccountQuota{Remain: bal.TotalBalance, Total: bal.TotalQuota, Used: bal.UsedAmount, UpdatedAt: time.Now().Unix()})
		return fmt.Sprintf("ok balance=%d/%d used=%d", bal.TotalBalance, bal.TotalQuota, bal.UsedAmount), nil
	})
}

// claimTencentCheckin 每自然日（北京时间）为腾讯账号签到一次（幂等）并查询
// 余额日志（SPEC §24.2 落地）。签到响应含本次所得积分/连续天数（§32.2，即
// 「Buddy 加油站」积分），失败只记日志、不影响账号健康。
func (s *Scheduler) claimTencentCheckin(ctx context.Context) {
	s.claimDaily(ctx, "tencent checkin", "workbuddy", func(acct *pool.Account) (string, error) {
		api, ok := acct.Client.(upstream.BillingAPI)
		if !ok {
			return "", nil // 无计费能力（非腾讯客户端）：跳过
		}
		res, err := api.DailyCheckin(acct.Auth)
		if err != nil {
			return "", err
		}
		gain := ""
		if res != nil {
			if res.Already {
				gain = " already"
			} else {
				gain = fmt.Sprintf(" credit=+%d", res.Credit)
			}
			if res.StreakDays > 0 {
				gain += fmt.Sprintf(" streak=%d", res.StreakDays)
			}
		}
		if st, serr := api.CheckinStatus(acct.Auth); serr == nil && st != nil {
			gain += fmt.Sprintf(" total=%d theme=%s", st.TotalCredits, st.ThemeName)
		}
		remain, rerr := api.UserResource(acct.Auth)
		if rerr != nil {
			return "ok" + gain + " (balance query failed: " + rerr.Error() + ")", nil
		}
		acct.SetQuota(pool.AccountQuota{Remain: remain, UpdatedAt: time.Now().Unix()})
		return fmt.Sprintf("ok%s balance=%d", gain, remain), nil
	})
}

// activateBuddy 宠物激活子流程（SPEC §32.2 补）：quota → 能量足够则开盲盒。
// 能量不足只记日志（能量来自任务/旅行奖励，后续 Tick 自然重试）。
func (s *Scheduler) activateBuddy(acct *pool.Account, api upstream.BillingAPI) {
	q, err := api.PetQuota(acct.Auth)
	if err != nil {
		log.Printf("tencent pet account=%s action=activate failed err=%v", acct.Name, err)
		return
	}
	if q == nil || q.Affordable < 1 {
		log.Printf("tencent pet account=%s action=activate skipped reason=no_buddy_energy_insufficient affordable=%d cost=%d",
			acct.Name, affordable(q), costPer(q))
		return
	}
	count := q.Affordable
	if q.MaxOpenCount > 0 && count > q.MaxOpenCount {
		count = q.MaxOpenCount
	}
	if err := api.PetOpenBox(acct.Auth, count); err != nil {
		log.Printf("tencent pet account=%s action=activate failed err=%v", acct.Name, err)
		return
	}
	log.Printf("tencent pet account=%s action=activate opened=%d (buddy box, energy spent=%d)",
		acct.Name, count, costPer(q)*count)
}

func affordable(q *upstream.PetQuota) int {
	if q == nil {
		return 0
	}
	return q.Affordable
}

func costPer(q *upstream.PetQuota) int {
	if q == nil {
		return 0
	}
	return q.CostPerOpen
}

// petTravel 成长中心宠物探险（SPEC §32.2）：每 Tick 状态机——
// arrived → claim；idle 且未达上限 → depart（config 首个地点）；traveling → 等待。
// 幂等（no unclaimed / daily_limit_reached 均按跳过）；失败只记日志，
// 绝不冷却/禁用账号（活动面故障不得污染聊天账号健康，§32.2 关键拍板）。
func (s *Scheduler) petTravel(ctx context.Context) {
	for _, acct := range s.cfg.Pool.Accounts() {
		if acct.ProfileID != "workbuddy" {
			continue
		}
		api, ok := acct.Client.(upstream.BillingAPI)
		if !ok {
			continue
		}
		st, err := api.PetTravelStatus(acct.Auth)
		if err != nil {
			log.Printf("tencent pet account=%s failed err=%v", acct.Name, err)
			continue
		}
		if st == nil {
			continue
		}
		switch st.State {
		case "arrived":
			credit, cerr := api.PetClaim(acct.Auth, st.RecordID.String())
			switch {
			case cerr == nil:
				log.Printf("tencent pet account=%s state=arrived action=claim credit=+%d", acct.Name, credit)
			case errors.Is(cerr, upstream.ErrPetNoUnclaimed):
				log.Printf("tencent pet account=%s state=arrived action=skip reason=no_unclaimed", acct.Name)
			default:
				log.Printf("tencent pet account=%s state=arrived action=claim failed err=%v", acct.Name, cerr)
			}
		case "idle":
			if st.DailyLimitReached {
				log.Printf("tencent pet account=%s state=idle action=skip reason=daily_limit", acct.Name)
				continue
			}
			locs, cerr := api.PetTravelConfig(acct.Auth)
			if cerr != nil || len(locs) == 0 {
				log.Printf("tencent pet account=%s state=idle action=depart failed err=%v", acct.Name, cerr)
				continue
			}
			loc := locs[0]
			if derr := api.PetDepart(acct.Auth, loc.ID); derr != nil {
				// 无活跃宠物（活测实证 msg="no active buddy"）：能量足够则开盲盒激活
				// （宠物唯一获取途径；能量唯一出口），下轮 Tick 自然进入旅行状态机
				if strings.Contains(derr.Error(), "no active buddy") {
					s.activateBuddy(acct, api)
					continue
				}
				log.Printf("tencent pet account=%s state=idle action=depart failed err=%v", acct.Name, derr)
				continue
			}
			log.Printf("tencent pet account=%s state=idle action=depart location=%s duration=%dh~%dh",
				acct.Name, loc.Name, loc.DurationHoursMin, loc.DurationHoursMax)
		case "traveling":
			remain := st.ArriveAt - st.ServerNow
			if remain < 0 {
				remain = 0
			}
			log.Printf("tencent pet account=%s state=traveling action=wait arrive_in=%dm", acct.Name, remain/60)
		default:
			log.Printf("tencent pet account=%s state=%s action=none", acct.Name, st.State)
		}
	}
}
