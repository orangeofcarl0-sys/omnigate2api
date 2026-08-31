// scheduler 腾讯签到（SPEC §24.2 落地）：每日北京时间一次、仅 workbuddy 家族、
// 与华为福利领取路径并行；幂等日期去重。
package scheduler

import (
	"context"
	"io"
	"testing"
	"time"

	"omnigate2api/internal/auth"
	"omnigate2api/internal/pool"
	"omnigate2api/internal/upstream"
)

// fakeClient 实现 ChatAPI + BillingAPI：计数签到/余额调用。
type fakeClient struct {
	checkins int
	balances int
}

func (f *fakeClient) ChatStream(ctx context.Context, chatID string, messages []upstream.ChatMessage, traceID string, cred upstream.SignCredential, userName, model string, tools []map[string]any, toolChoice string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeClient) RefreshToken(ctx context.Context, cfg upstream.LoginConfig, refreshToken, codeVerifier, domain string) (*upstream.TokenResponse, error) {
	return nil, nil
}
func (f *fakeClient) DailyCheckin(a *auth.Auth) error { f.checkins++; return nil }
func (f *fakeClient) UserResource(a *auth.Auth) (int64, error) {
	f.balances++
	return 42, nil
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
