// scheduler 腾讯签到（SPEC §24.2 落地）：每日北京时间一次、仅 workbuddy 家族、
// 与华为福利领取路径并行；幂等日期去重。
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// fakeClient 实现 ChatAPI + BillingAPI：计数签到/余额/宠物调用（SPEC §32）。
type fakeClient struct {
	checkins      int
	balances      int
	statuses      int
	petStates     int
	departs       int
	claims        int
	petState      string // 状态机测试注入：idle|traveling|arrived
	petLimit      bool
	petErr        error // 活动面故障注入（隔离性测试）
	opens         int
	departErr     error // depart 失败注入（no active buddy 激活路径）
	tasks         []upstream.GrowthTask
	taskQueries   int
	acceptBatches [][]string
	claimCodes    []string
}

func (f *fakeClient) ChatStream(ctx context.Context, chatID string, messages []upstream.ChatMessage, traceID string, cred upstream.SignCredential, userName, model string, tools []map[string]any, toolChoice string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeClient) RefreshToken(ctx context.Context, cfg upstream.LoginConfig, refreshToken, codeVerifier, domain string) (*upstream.TokenResponse, error) {
	return nil, nil
}
func (f *fakeClient) DailyCheckin(a *auth.Auth) (*upstream.CheckinResult, error) {
	f.checkins++
	return &upstream.CheckinResult{Credit: 100, StreakDays: 7}, nil
}
func (f *fakeClient) CheckinStatus(a *auth.Auth) (*upstream.CheckinStatus, error) {
	f.statuses++
	return &upstream.CheckinStatus{ThemeName: "t", TodayCheckedIn: true, StreakDays: 7, TotalCredits: 700}, nil
}
func (f *fakeClient) UserResource(a *auth.Auth) (int64, error) {
	f.balances++
	return 42, nil
}
func (f *fakeClient) PetTravelStatus(a *auth.Auth) (*upstream.PetTravel, error) {
	f.petStates++
	if f.petErr != nil {
		return nil, f.petErr
	}
	st := &upstream.PetTravel{State: f.petState, DailyLimitReached: f.petLimit, ArriveAt: 200, ServerNow: 100}
	return st, nil
}
func (f *fakeClient) PetDepart(a *auth.Auth, locationID json.Number) error {
	f.departs++
	return f.departErr
}
func (f *fakeClient) PetClaim(a *auth.Auth, recordID string) (int64, error) {
	f.claims++
	return 88, nil
}
func (f *fakeClient) PetQuota(a *auth.Auth) (*upstream.PetQuota, error) {
	return &upstream.PetQuota{Affordable: 3, MaxOpenCount: 1, CostPerOpen: 50}, nil
}
func (f *fakeClient) PetOpenBox(a *auth.Auth, count int) error { f.opens++; return nil }
func (f *fakeClient) GrowthTasks(a *auth.Auth) ([]upstream.GrowthTask, error) {
	f.taskQueries++
	return f.tasks, nil
}
func (f *fakeClient) GrowthAcceptTasks(a *auth.Auth, codes []string) (map[string]string, error) {
	f.acceptBatches = append(f.acceptBatches, codes)
	return map[string]string{}, nil
}
func (f *fakeClient) GrowthClaimTask(a *auth.Auth, code string) (int64, int64, bool, error) {
	f.claimCodes = append(f.claimCodes, code)
	return 10, 5, false, nil
}
func (f *fakeClient) PetTravelConfig(a *auth.Auth) ([]upstream.PetLocation, error) {
	return []upstream.PetLocation{{ID: "1", Name: "森林", DurationHoursMin: 2, DurationHoursMax: 4}}, nil
}

func TestSchedulerTencentCheckinOncePerDay(t *testing.T) {
	u1 := auth.New("u1", "huawei", "dom", "tok1", "ak", "sk", "2099-01-01T00:00:00Z", "r", "v")
	u2 := &auth.Auth{UserID: "u2", UserName: "tc", Profile: "workbuddy", CloudDragonTok: "t2",
		RefreshToken: "r2", Expiration: "2099-01-01T00:00:00Z", EnterpriseID: "e2", Domain: "www.codebuddy.cn"}
	p, err := pool.New([]*auth.Auth{u1, u2}, pool.Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 替换 Client：华为账号挂普通 ChatAPI stub（无 BillingAPI），腾讯账号挂计数 stub
	huaweiStub := &fakeClient{}
	for _, a := range p.Accounts() {
		if a.ProfileID == "workbuddy" {
			a.Client = &fakeClient{}
		} else {
			a.Client = huaweiStub
		}
	}
	tcStub := p.Get("u2").Client.(*fakeClient)

	s := New(Config{Pool: p, Enabled: true})
	s.Tick(context.Background())
	s.Tick(context.Background()) // 同一自然日第二次：不重复
	if tcStub.checkins != 1 || tcStub.balances != 1 {
		t.Fatalf("tencent checkin must run once per day: checkins=%d balances=%d", tcStub.checkins, tcStub.balances)
	}
	if huaweiStub.checkins != 0 || huaweiStub.balances != 0 {
		t.Fatalf("huawei account must not trigger tencent billing: %d/%d", huaweiStub.checkins, huaweiStub.balances)
	}
}

// TestSchedulerPetTravelStateMachine SPEC §32.2：idle→depart / arrived→claim /
// traveling→等待 / daily_limit_reached→跳过；幂等分支不误派发。
func TestSchedulerPetTravelStateMachine(t *testing.T) {
	cases := []struct {
		state      string
		limit      bool
		wantDepart int
		wantClaim  int
	}{
		{"idle", false, 1, 0},
		{"idle", true, 0, 0},       // 今日次数上限：跳过
		{"traveling", false, 0, 0}, // 在路上：等待
		{"arrived", false, 0, 1},   // 归来：领取
	}
	for _, tc := range cases {
		u2 := &auth.Auth{UserID: "u2", UserName: "tc", Profile: "workbuddy", CloudDragonTok: "t2",
			RefreshToken: "r2", Expiration: "2099-01-01T00:00:00Z", EnterpriseID: "e2", Domain: "www.codebuddy.cn"}
		p, err := pool.New([]*auth.Auth{u2}, pool.Config{
			ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
			MaxConcurrent: 1, KeepaliveWindow: time.Minute,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		stub := &fakeClient{petState: tc.state, petLimit: tc.limit}
		p.Accounts()[0].Client = stub
		s := New(Config{Pool: p, Enabled: true})
		s.Tick(context.Background())
		if stub.departs != tc.wantDepart || stub.claims != tc.wantClaim {
			t.Fatalf("state=%s limit=%v depart=%d want %d claim=%d want %d",
				tc.state, tc.limit, stub.departs, tc.wantDepart, stub.claims, tc.wantClaim)
		}
	}
}

// TestSchedulerActivityFailureIsolated SPEC §32.2 关键拍板：活动面失败只记日志，
// 绝不冷却/禁用账号（不污染聊天账号健康）。
func TestSchedulerActivityFailureIsolated(t *testing.T) {
	u2 := &auth.Auth{UserID: "u2", UserName: "tc", Profile: "workbuddy", CloudDragonTok: "t2",
		RefreshToken: "r2", Expiration: "2099-01-01T00:00:00Z", EnterpriseID: "e2", Domain: "www.codebuddy.cn"}
	p, err := pool.New([]*auth.Auth{u2}, pool.Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	stub := &fakeClient{petErr: errors.New("activity endpoint changed"), petState: "idle"}
	p.Accounts()[0].Client = stub
	s := New(Config{Pool: p, Enabled: true})
	s.Tick(context.Background())
	total, healthy, disabled, cooling, _ := p.Stats()
	if total != 1 || healthy != 1 || disabled != 0 || cooling != 0 {
		t.Fatalf("activity failure must not touch account health: total=%d healthy=%d disabled=%d cooling=%d",
			total, healthy, disabled, cooling)
	}
}

// TestSchedulerBuddyActivation SPEC §32.2 补：depart 报 no active buddy 时
// 走能量开盲盒激活（quota→open），不误报失败。
func TestSchedulerBuddyActivation(t *testing.T) {
	u2 := &auth.Auth{UserID: "u2", UserName: "tc", Profile: "workbuddy", CloudDragonTok: "t2",
		RefreshToken: "r2", Expiration: "2099-01-01T00:00:00Z", EnterpriseID: "e2", Domain: "www.codebuddy.cn"}
	p, err := pool.New([]*auth.Auth{u2}, pool.Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	stub := &fakeClient{petState: "idle", departErr: errors.New("pet depart failed http=400 code=400 msg=no active buddy")}
	p.Accounts()[0].Client = stub
	s := New(Config{Pool: p, Enabled: true})
	s.Tick(context.Background())
	if stub.opens != 1 {
		t.Fatalf("no active buddy must trigger box activation: opens=%d depart=%d", stub.opens, stub.departs)
	}
}

// TestSchedulerGrowthTasks SPEC §32 阶段 3：not_accepted→批量接单；
// completed→逐个领奖并汇总 credit/energy；claimed/locked 跳过。
func TestSchedulerGrowthTasks(t *testing.T) {
	u2 := &auth.Auth{UserID: "u2", UserName: "tc", Profile: "workbuddy", CloudDragonTok: "t2",
		RefreshToken: "r2", Expiration: "2099-01-01T00:00:00Z", EnterpriseID: "e2", Domain: "www.codebuddy.cn"}
	p, err := pool.New([]*auth.Auth{u2}, pool.Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	stub := &fakeClient{
		petState: "traveling",
		tasks: []upstream.GrowthTask{
			{Code: "t1", Title: "新任务", Status: "available"},
			{Code: "t2", Title: "已接单", Status: "accepted"},
			{Code: "t3", Title: "已完成待领", Status: "completed", RewardCredit: 50, RewardEnergy: 10},
			{Code: "t4", Title: "已领过", Status: "claimed"},
			{Code: "t5", Title: "锁定", Locked: true, Status: "available"},
		},
	}
	p.Accounts()[0].Client = stub
	s := New(Config{Pool: p, Enabled: true})
	s.Tick(context.Background())
	if len(stub.acceptBatches) != 1 || len(stub.acceptBatches[0]) != 1 || stub.acceptBatches[0][0] != "t1" {
		t.Fatalf("accept batches=%v", stub.acceptBatches)
	}
	if len(stub.claimCodes) != 1 || stub.claimCodes[0] != "t3" {
		t.Fatalf("claim codes=%v", stub.claimCodes)
	}
}
