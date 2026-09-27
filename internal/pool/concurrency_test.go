// 全球域单账号并发上限独立分档（SPEC §28.4 / HANDOFF §11.11）：上游对国际版有更严的
// 风控档位——社区实证「官方默认压到 2，并发过高被判为异常流量」（ithtelab 1.0.40 /
// linguo pool.max_in_flight_global=2）。国内域沿用通用档。
package pool

import (
	"testing"
	"time"

	"omnigate2api/internal/auth"
)

// 全球域单账号并发上限独立分档（默认 2）：上游对国际版有更严的风控档位——社区实证
// 「官方默认压到 2，并发过高被判为异常流量」（ithtelab 1.0.40 / linguo
// pool.max_in_flight_global=2，HANDOFF §11.11）。国内域沿用通用档。
func TestMaxConcurrentGlobalRealm(t *testing.T) {
	cn := testAuth("cn1", "workbuddy") // Domain www.codebuddy.cn
	global := &auth.Auth{UserID: "g1", UserName: "g", Profile: "workbuddy", CloudDragonTok: "t",
		RefreshToken: "r", Expiration: "2099-01-01T00:00:00Z", EnterpriseID: "e", Domain: "www.workbuddy.ai"}

	caps := func(p *Pool) map[string]int {
		out := map[string]int{}
		for _, row := range p.List() {
			out[row["uid"].(string)] = row["max_concurrent"].(int)
		}
		return out
	}

	p, err := New([]*auth.Auth{cn, global}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 5, MaxConcurrentGlobal: 2, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := caps(p); got["cn1"] != 5 || got["g1"] != 2 {
		t.Fatalf("全球域必须走独立档位：%+v", got)
	}

	// 缺省（未配 max_concurrent_global）→ 2
	p2, err := New([]*auth.Auth{global}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 5, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := caps(p2); got["g1"] != 2 {
		t.Fatalf("缺省应为 2：%+v", got)
	}

	// 显式覆盖生效
	p3, err := New([]*auth.Auth{global}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 5, MaxConcurrentGlobal: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := caps(p3); got["g1"] != 1 {
		t.Fatalf("显式覆盖应生效：%+v", got)
	}
}
