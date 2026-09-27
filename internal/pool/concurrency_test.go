// 单账号并发上限按家族/区域分档（SPEC §28.4 / HANDOFF §7 第 9 条 + §11.11）：
//   - 全球域（workbuddy.ai）：上游对国际版风控更严，社区实证「官方默认压到 2」；
//   - 华为（codearts）：上游**硬上限是 3 个并发会话**（TM.00001041 原文「并发会话数已达
//     上限(3个)」）且槽位释放慢 → 默认 2（留一个余量）。2026-09-27 实测：配 5 时一天
//     18 次 TM.00001041 风暴（我们等 5s 重试、请求被拖住），随后一条 504 又把唯一的
//     华为号冷却 10 分钟；
//   - 国内域腾讯：沿用通用档。
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

	huawei := testAuth("h1", "") // 华为：Profile 为空、Domain 为空

	caps := func(p *Pool) map[string]int {
		out := map[string]int{}
		for _, row := range p.List() {
			out[row["uid"].(string)] = row["max_concurrent"].(int)
		}
		return out
	}

	p, err := New([]*auth.Auth{cn, global, huawei}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 5, MaxConcurrentGlobal: 2, MaxConcurrentCodearts: 2, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := caps(p); got["cn1"] != 5 || got["g1"] != 2 || got["h1"] != 2 {
		t.Fatalf("分档错误（国内腾讯 5 / 全球 2 / 华为 2）：%+v", got)
	}

	// 缺省（未配两档）→ 全球 2、华为 2
	p2, err := New([]*auth.Auth{global, huawei}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 5, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := caps(p2); got["g1"] != 2 || got["h1"] != 2 {
		t.Fatalf("缺省应为 2/2：%+v", got)
	}

	// 显式覆盖生效
	p3, err := New([]*auth.Auth{global, huawei}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 5, MaxConcurrentGlobal: 1, MaxConcurrentCodearts: 3, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := caps(p3); got["g1"] != 1 || got["h1"] != 3 {
		t.Fatalf("显式覆盖应生效：%+v", got)
	}
}
