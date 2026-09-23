// 账号池可靠性语义测试（SPEC §24.1/§5.3）：轮换与族隔离、冷却生命周期、
// 错误阈值熔断、禁用/启用、状态持久化、并发锁、额度统计。
//
// 补测背景：pool 是可靠性核心（历史上「传输错误累计成硬冷却 → 假 503」即出在此），
// 但此前零测试。
package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"omnigate2api/internal/auth"
)

func testAuth(id, profile string) *auth.Auth {
	return &auth.Auth{
		UserID: id, UserName: "n-" + id, Profile: profile,
		CloudDragonTok: "tok", RefreshToken: "ref",
		Expiration: "2099-01-01T00:00:00Z", EnterpriseID: "e", Domain: "www.codebuddy.cn",
	}
}

func newTestPool(t *testing.T, stateFile string, auths ...*auth.Auth) *Pool {
	t.Helper()
	p, err := New(auths, Config{
		ErrThreshold: 3, ErrCooldown: 10 * time.Minute, SoftCooldown: time.Minute,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 轮换必须覆盖全池且不跨家族（SPEC §24.1：家族隔离）。
func TestPickForRotationAndFamilyIsolation(t *testing.T) {
	p := newTestPool(t, "", testAuth("h1", ""), testAuth("t1", "workbuddy"), testAuth("t2", "workbuddy"))
	tried := map[string]bool{}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		a := p.PickFor("workbuddy", tried)
		if a == nil {
			t.Fatal("expected an account")
		}
		if a.ProfileID != "workbuddy" {
			t.Fatalf("family leak: picked %s (%s)", a.Name, a.ProfileID)
		}
		tried[a.Name] = true
		seen[a.Name] = true
	}
	if !seen["t1"] || !seen["t2"] {
		t.Fatalf("rotation must cover both workbuddy accounts: %v", seen)
	}
	if a := p.PickFor("workbuddy", tried); a != nil {
		t.Fatalf("all tried → nil expected, got %s", a.Name)
	}
	// 华为账号绝不出现在 workbuddy 轮换中
	if a := p.PickFor("codearts", map[string]bool{}); a == nil || a.ProfileID != "codearts" {
		t.Fatalf("codearts pick broken: %+v", a)
	}
}

// 冷却阻止挑选，清除后恢复（软冷却 = 429 类）。
func TestCooldownBlocksPickThenClears(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"))
	p.Cooldown("t1", CoolSoft, time.Minute, "429 rate limit")
	if p.Healthy("t1") {
		t.Fatal("cooling account must be unhealthy")
	}
	if a := p.PickFor("workbuddy", map[string]bool{}); a != nil {
		t.Fatalf("cooling account must not be picked, got %s", a.Name)
	}
	_, _, _, cooling, _ := p.Stats()
	if cooling != 1 {
		t.Fatalf("stats cooling=%d want 1", cooling)
	}
	if !p.ClearCooldown("t1") {
		t.Fatal("clear must report a change")
	}
	if !p.Healthy("t1") {
		t.Fatal("must be healthy after clear")
	}
}

// 硬额度冷却至次日 04:00（北京时间口径，SPEC §28.4 决策 B）。
func TestCooldownUntilTomorrow4AM(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"))
	before := time.Now()
	p.CooldownUntilTomorrow4AM("t1", "insufficient credit")
	a := p.Get("t1")
	if a == nil {
		t.Fatal("account missing")
	}
	until := a.coolUntil
	if !until.After(before) {
		t.Fatalf("deadline must be in the future: %v", until)
	}
	if h := until.Hour(); h != 4 {
		t.Fatalf("deadline hour must be 04:00 CST, got %d (%v)", h, until)
	}
	if d := until.Sub(before); d > 29*time.Hour {
		t.Fatalf("deadline too far: %v", d)
	}
}

// 错误阈值熔断：达阈值才冷却；成功后计数清零。
func TestNoteErrorThresholdAndReset(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"))
	p.NoteError("t1", 3, time.Minute)
	p.NoteError("t1", 3, time.Minute)
	if !p.Healthy("t1") {
		t.Fatal("below threshold must stay healthy")
	}
	p.NoteError("t1", 3, time.Minute) // 第 3 次 → 熔断
	if p.Healthy("t1") {
		t.Fatal("threshold reached must cool down")
	}
	// 既有语义：NoteSuccess 只清计数，不清已生效的冷却（防抖动）。
	// 解除冷却需等冷却到期或 ClearCooldown（面板「清冷却」）。
	p.NoteSuccess("t1")
	if a := p.Get("t1"); a == nil || a.errCount != 0 {
		t.Fatalf("NoteSuccess must reset error count: %+v", a)
	}
	if p.Healthy("t1") {
		t.Fatal("active cooldown must persist until expiry/clear (documented semantics)")
	}
	if !p.ClearCooldown("t1") || !p.Healthy("t1") {
		t.Fatal("ClearCooldown must lift the cooldown")
	}
}

// 禁用/启用及其在 Stats/List 中的可见性。
func TestDisableEnableVisibility(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"))
	p.Disable("t1", "manual")
	if p.Healthy("t1") {
		t.Fatal("disabled must be unhealthy")
	}
	total, healthy, disabled, _, _ := p.Stats()
	if total != 1 || healthy != 0 || disabled != 1 {
		t.Fatalf("stats total=%d healthy=%d disabled=%d", total, healthy, disabled)
	}
	if a := p.PickFor("workbuddy", map[string]bool{}); a != nil {
		t.Fatal("disabled account must not be picked")
	}
	if !p.Enable("t1") {
		t.Fatal("enable must report a change")
	}
	if !p.Healthy("t1") {
		t.Fatal("must be healthy after enable")
	}
}

// 状态持久化往返：冷却/禁用跨实例保留（state.json 语义）。
func TestStatePersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	p := newTestPool(t, stateFile, testAuth("t1", "workbuddy"), testAuth("t2", "workbuddy"))
	p.Cooldown("t1", CoolSoft, time.Hour, "429")
	p.Disable("t2", "manual")
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("state file must be written: %v", err)
	}

	p2 := newTestPool(t, stateFile, testAuth("t1", "workbuddy"), testAuth("t2", "workbuddy"))
	if p2.Healthy("t1") {
		t.Fatal("cooldown must survive reload")
	}
	if a := p2.Get("t2"); a == nil || p2.Healthy("t2") {
		t.Fatal("disable must survive reload")
	}
}

// 并发槽位：达上限拒绝，释放后可取，等待超时返回 false。
func TestConcurrencyLock(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"))
	if !p.AcquireLock("t1") {
		t.Fatal("first acquire must succeed")
	}
	if p.AcquireLock("t1") {
		t.Fatal("second acquire must fail at capacity")
	}
	if p.AcquireLockWait("t1", 100*time.Millisecond) {
		t.Fatal("wait must time out at capacity")
	}
	p.ReleaseLock("t1")
	if !p.AcquireLock("t1") {
		t.Fatal("acquire must succeed after release")
	}
	p.ReleaseLock("t1")
}

// 额度快照进入 List/Stats（面板与签到链路共用）。
func TestQuotaReflectedInListAndStats(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"), testAuth("t2", "workbuddy"))
	p.Get("t1").SetQuota(AccountQuota{Remain: 400, Total: 2000, Used: 1600, UpdatedAt: time.Now().Unix()})
	p.Get("t2").SetQuota(AccountQuota{Remain: 43, UpdatedAt: time.Now().Unix()})

	var found bool
	for _, row := range p.List() {
		if row["name"] == "t1" {
			found = true
			if row["credits"] != int64(400) || row["quota_total"] != int64(2000) || row["quota_used"] != int64(1600) {
				t.Fatalf("t1 row=%v", row)
			}
		}
	}
	if !found {
		t.Fatal("t1 must appear in List")
	}
	// 注意（结构气味，待改名）：Stats 第 5 个返回值名义为 credits，实为**健康账号数**
	// （见 pool.go 注释「面板『积分合计』位复用为健康数」）。额度真值走 List()。
	if _, healthy, _, _, credits := p.Stats(); credits != int64(healthy) || healthy != 2 {
		t.Fatalf("stats credits(%d) must equal healthy(%d)=2 (documented quirk)", credits, healthy)
	}
}

// Validate 对过期且无 refresh_token 的账号返回「可用」（警告日志，不误禁——
// SPEC §7：令牌过期由真实请求 401 自然禁用）。
func TestValidateExpiredWithoutRefreshTokenStaysUsable(t *testing.T) {
	a := testAuth("t1", "workbuddy")
	a.Expiration = "2020-01-01T00:00:00Z"
	a.RefreshToken = ""
	p := newTestPool(t, "", a)
	ok, err := p.Validate(p.Get("t1"))
	if err != nil || !ok {
		t.Fatalf("validate must not disable on expiry alone: ok=%v err=%v", ok, err)
	}
	if !p.Healthy("t1") {
		t.Fatal("account must remain healthy")
	}
}
