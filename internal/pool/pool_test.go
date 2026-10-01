// 账号池可靠性语义测试（SPEC §24.1/§5.3）：轮换与族隔离、冷却生命周期、
// 错误阈值熔断、禁用/启用、状态持久化、并发锁、额度统计。
//
// 补测背景：pool 是可靠性核心（历史上「传输错误累计成硬冷却 → 假 503」即出在此），
// 但此前零测试。
package pool

import (
	"os"
	"path/filepath"
	"strings"
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
	_, _, _, cooling := p.Stats()
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

// NoteSuccess 必须清掉历史错误文案：健康账号在面板「原因」列显示
// "consecutive errors" 会把已恢复的账号误报成有问题（误导排障）。
func TestNoteSuccessClearsLastError(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"))
	p.Cooldown("t1", CoolErr, time.Millisecond, "boom")
	if a := p.Get("t1"); a == nil || a.lastErr != "boom" {
		t.Fatalf("Cooldown must record lastErr: %+v", a)
	}
	p.NoteSuccess("t1")
	if a := p.Get("t1"); a == nil || a.lastErr != "" || a.disabledReason != "" {
		t.Fatalf("NoteSuccess must clear stale reason: %+v", a)
	}
	// 禁用原因不得被成功回调抹掉（禁用态需要保留原因供排障）。
	p.Disable("t1", "manual disable")
	p.NoteSuccess("t1")
	if a := p.Get("t1"); a == nil || a.disabledReason != "manual disable" {
		t.Fatalf("disable reason must survive NoteSuccess: %+v", a)
	}
}

// 禁用/启用及其在 Stats/List 中的可见性。
func TestDisableEnableVisibility(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"))
	p.Disable("t1", "manual")
	if p.Healthy("t1") {
		t.Fatal("disabled must be unhealthy")
	}
	total, healthy, disabled, _ := p.Stats()
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
	// Stats 只报健康分布；额度真值走 List()（历史「credits 实为健康数」的假字段已删除）。
	if _, healthy, _, _ := p.Stats(); healthy != 2 {
		t.Fatalf("stats healthy=%d want 2", healthy)
	}
}

// Validate 对"已过期且无续期路径"的账号判死（2026-09-27 改口径）：旧行为是返回可用、
// 等真实请求 401 再禁用——实测后果是账号过期 12 小时面板仍显示"健康"、期间每个请求
// 白打一次上游 401。现在已过期即禁用并写明 re-login required；仅"临近过期（未过期）"
// 才保持可用（见 TestValidateNoRenewalPathSplitsByExpiry）。
func TestValidateExpiredWithoutRenewalPathDisables(t *testing.T) {
	a := testAuth("t1", "workbuddy")
	a.Expiration = "2020-01-01T00:00:00Z"
	a.RefreshToken = ""
	p := newTestPool(t, "", a)
	ok, err := p.Validate(p.Get("t1"))
	if err != nil || ok {
		t.Fatalf("expired token with no renewal path must not validate ok: ok=%v err=%v", ok, err)
	}
	if p.Healthy("t1") {
		t.Fatal("account must be disabled (needs re-login)")
	}
	row := p.List()[0]
	if reason, _ := row["reason"].(string); !strings.Contains(reason, "re-login required") {
		t.Fatalf("reason must point at re-login: %v", row["reason"])
	}
}

// 模型级限流：只冷却 (账号, 模型) 对，其余模型照常可选；账号级"清冷却"一并清掉。
func TestModelScopedCooldown(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"), testAuth("t2", "workbuddy"))
	p.CoolModel("t1", "glm-5.2", time.Now().Add(30*time.Minute), "model rate limit")

	if !p.ModelCooled("t1", "glm-5.2") {
		t.Fatal("pair must be cooling")
	}
	// 大小写不敏感（模型名统一小写归一）
	if !p.ModelCooled("t1", "GLM-5.2") {
		t.Fatal("model key must be case-insensitive")
	}
	// 账号本身仍健康：模型级限流不是账号故障
	if !p.Healthy("t1") {
		t.Fatal("model-scoped limit must not cool the whole account")
	}
	// 同一账号的其它模型照常可选（这是本机制的全部意义）
	if a := p.PickForModel("workbuddy", "kimi-k2.7", nil); a == nil || a.Name != "t1" {
		t.Fatalf("other models on the same account must remain usable: %+v", a)
	}
	// 被限的模型在该账号上被跳过 → 落到另一个账号
	if a := p.PickForModel("workbuddy", "glm-5.2", nil); a == nil || a.Name != "t2" {
		t.Fatalf("limited pair must rotate to another account: %+v", a)
	}
	// 该模型的冷却明细可见（面板展示）
	if l := p.ModelCools("t1"); l["glm-5.2"].IsZero() {
		t.Fatalf("model cool detail must be exposed: %v", l)
	}
	// 账号级清冷却一并清模型冷却
	if !p.ClearCooldown("t1") || p.ModelCooled("t1", "glm-5.2") {
		t.Fatal("ClearCooldown must also drop model-scoped cooldowns")
	}
}

// 所有账号的该模型都在冷却时：不应整体拒绝（退化返回兜底账号，让上游再判一次），
// 否则单模型限流会把整个家族打成不可用。
func TestModelScopedCooldownFallback(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"))
	p.CoolModel("t1", "glm-5.2", time.Now().Add(time.Minute), "limit")
	if a := p.PickForModel("workbuddy", "glm-5.2", nil); a == nil || a.Name != "t1" {
		t.Fatalf("sole account must still be returned as fallback: %+v", a)
	}
}

// "原因"只表达当前状态（2026-09-27 用户报"冷却报因黏在板子上"）：模型级限流会写
// 账号级 lastErr，到期后没人清 → 面板一直挂着长错误文案。现在 List 的 reason 只在
// 禁用/账号级冷却时非空；历史错误留在 last_error（面板放进 tooltip），且到期后由
// Validate 清掉。
func TestListReasonReflectsCurrentStateOnly(t *testing.T) {
	a := testAuth("t1", "workbuddy")
	p := newTestPool(t, "", a)
	acct := p.Get("t1")

	// 模型级限流（账号本身健康）：reason 必须为空，模型限流另走 model_cooling
	p.CoolModel("t1", "deepseek-v4.1-flash", time.Now().Add(30*time.Minute), `{"code":6004,"msg":"usage exceeds frequency limit"}`)
	row := p.List()[0]
	if row["reason"] != "" {
		t.Fatalf("model-level limit must not stick in reason: %v", row["reason"])
	}
	if row["cooling"] != false || row["disabled"] != false {
		t.Fatalf("account itself stays healthy: %+v", row)
	}
	if mc, _ := row["model_cooling"].([]map[string]any); len(mc) != 1 {
		t.Fatalf("model cooling must be reported separately: %+v", row["model_cooling"])
	}
	if last, _ := row["last_error"].(string); !strings.Contains(last, "6004") {
		t.Fatalf("history must stay available in last_error: %v", row["last_error"])
	}

	// 账号级冷却：reason 就是冷却原因（当前状态）
	p.Cooldown("t1", CoolSoft, time.Minute, "429 too many requests")
	if row := p.List()[0]; row["cooling"] != true || row["reason"] != "429 too many requests" {
		t.Fatalf("active cooldown must explain itself: %+v", row)
	}

	// 冷却/限流都过期后：Validate 把痕迹清掉，reason 与 last_error 都归位
	acct.mu.Lock()
	acct.coolUntil = time.Time{}
	acct.coolKind = CoolNone
	acct.modelCool = map[string]time.Time{"deepseek-v4.1-flash": time.Now().Add(-time.Minute)}
	acct.lastErr = "stale"
	acct.lastValidated = time.Time{}
	acct.mu.Unlock()
	if ok, err := p.Validate(acct); err != nil || !ok {
		t.Fatalf("validate: ok=%v err=%v", ok, err)
	}
	row = p.List()[0]
	if row["reason"] != "" || row["last_error"] != "" {
		t.Fatalf("expired cooling traces must be pruned: reason=%v last_error=%v", row["reason"], row["last_error"])
	}
	if mc, _ := row["model_cooling"].([]map[string]any); len(mc) != 0 {
		t.Fatalf("expired model cooling must be pruned: %+v", row["model_cooling"])
	}
}

// 上游封禁的硬禁用必须**粘住**（2026-09-27）：Validate 的「凭证有效即复位」规则本意是
// "重登后旧禁用不该拦请求"，但对上游封禁不成立——社区实测 11140 request illegal 到期
// 也不自愈。若用普通 Disable，下一次 Tick（≤30 分钟）就会清掉，号池又去撞同一面墙
// （本机实测：重启后三个被封号又变"健康"）。
func TestStickyDisableSurvivesValidate(t *testing.T) {
	p := newTestPool(t, "", testAuth("t1", "workbuddy"))
	const reason = "上游封禁（11140 request illegal）——重登无效（实测），可点「启用」重试或换号"
	p.DisableSticky("t1", reason)

	if ok, err := p.Validate(p.Get("t1")); err != nil || !ok {
		t.Fatalf("凭证本身有效，Validate 应返回 ok：ok=%v err=%v", ok, err)
	}
	row := p.List()[0]
	if row["disabled"] != true {
		t.Fatalf("硬禁用必须扛过 Validate（否则号池每 30 分钟又去撞墙）: %+v", row)
	}
	if got, _ := row["reason"].(string); !strings.Contains(got, "11140") {
		t.Fatalf("原因必须保留: %v", row["reason"])
	}

	// 出口一：面板「启用」（用户判断风控已过时可试）
	if !p.Enable("t1") {
		t.Fatal("enable must succeed")
	}
	if p.List()[0]["disabled"] == true {
		t.Fatal("「启用」必须解除硬禁用")
	}

	// 出口二：重新登录（AddAccount 换新凭证）
	p.DisableSticky("t1", reason)
	p.AddAccount(testAuth("t1", "workbuddy"))
	if p.List()[0]["disabled"] == true {
		t.Fatal("重新登录必须解除硬禁用")
	}
}

// 瞬时传输错误只观测、不惩罚，但必须在 List() 里可见（面板据此显示"瞬时错误 N 次"）。
// 2026-09-29 实测反馈：客户端报 503 而面板一片干净，用户误判成"模型限流但 dashboard 无反应"。
func TestNoteTransientObservableWithoutPenalty(t *testing.T) {
	a := auth.New("t1", "n1", "d1", "tok", "ak", "sk", "2099-01-01T00:00:00Z", "", "v")
	p, err := New([]*auth.Auth{a}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.NoteTransient("t1", `Post "https://x/v2/chat/completions": net/http: TLS handshake timeout`)
	p.NoteTransient("t1", `Post "https://x/v2/chat/completions": EOF`)
	row := p.List()[0]
	if row["transient_err"] != 2 {
		t.Fatalf("transient_err=%v，应累计 2", row["transient_err"])
	}
	if msg, _ := row["transient_msg"].(string); !strings.Contains(msg, "EOF") {
		t.Fatalf("应保留最近一次错误信息: %v", row["transient_msg"])
	}
	if at, _ := row["transient_at"].(string); at == "" {
		t.Fatal("应记录最近一次时间（面板据此算「x 分钟前」）")
	}
	// 关键：只观测，不惩罚——不计错误数、不冷却、不禁用
	if row["err_count"] != 0 || row["cooling"] != false || row["disabled"] != false {
		t.Fatalf("瞬时抖动不该罚账号: %+v", row)
	}
	p.ClearTransient("t1")
	if got := p.List()[0]["transient_err"]; got != 0 {
		t.Fatalf("清除后应为 0，实际 %v", got)
	}
}

// 重试**自愈**的抖动也要可见且不惩罚：与 transientErr 互补（那个记"两次都失败"，
// 这个记"抖了但救回来"）。只看前者会低估真实抖动率（2026-09-30 复盘）。
func TestNoteRetryObservableWithoutPenalty(t *testing.T) {
	a := auth.New("t1", "n1", "d1", "tok", "ak", "sk", "2099-01-01T00:00:00Z", "", "v")
	p, err := New([]*auth.Auth{a}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 用 UID 也能命中（上游侧只拿得到 Auth.UserID）
	p.NoteRetry("t1")
	p.NoteRetry("t1")
	row := p.List()[0]
	if row["retry_err"] != 2 {
		t.Fatalf("retry_err=%v，应累计 2", row["retry_err"])
	}
	if at, _ := row["retry_at"].(string); at == "" {
		t.Fatal("应记录最近一次时间（面板据此算「x 分钟前」）")
	}
	if row["err_count"] != 0 || row["cooling"] != false || row["disabled"] != false {
		t.Fatalf("重试自愈不该罚账号: %+v", row)
	}
	// 关键：ClearTransient（成功路径）**不得**清掉它——否则重试刚救回来的那次
	// 会被紧随其后的成功立刻抹掉，计数永远是 0（第一版就踩了这个坑）。
	p.ClearTransient("t1")
	if got := p.List()[0]["retry_err"]; got != 2 {
		t.Fatalf("成功清理不该影响滚动窗口计数，实际 %v", got)
	}
	// 但瞬时错误是"自上次成功以来"口径，必须归零
	if got := p.List()[0]["transient_err"]; got != 0 {
		t.Fatalf("transient_err 清除后应为 0，实际 %v", got)
	}
}

// 滚动窗口到期后从零重计：面板读到的应是"最近一小时抖了几次"这种可决策的量，
// 而不是一个只增不减、越跑越没意义的历史总数。
func TestNoteRetryRollsWindow(t *testing.T) {
	a := auth.New("t1", "n1", "d1", "tok", "ak", "sk", "2099-01-01T00:00:00Z", "", "v")
	p, err := New([]*auth.Auth{a}, Config{
		ErrThreshold: 3, ErrCooldown: time.Minute, SoftCooldown: time.Second,
		MaxConcurrent: 1, KeepaliveWindow: time.Minute,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.NoteRetry("t1")
	p.NoteRetry("t1")
	// 把窗口起点拨到 retryWindow 之前，等价于"这两次已经出窗口了"
	p.mu.Lock()
	p.accounts[0].mu.Lock()
	p.accounts[0].retryWin = time.Now().Add(-2 * retryWindow)
	p.accounts[0].mu.Unlock()
	p.mu.Unlock()
	p.NoteRetry("t1")
	if got := p.List()[0]["retry_err"]; got != 1 {
		t.Fatalf("窗口过期后应从零重计（期望 1），实际 %v", got)
	}
}
