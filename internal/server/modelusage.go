// (账号, 模型) 维度的**窗口用量观测**（SPEC §28.5）。
//
// 为什么需要：上游的「6004 模型限流」只给一句人话 + 一个 reset 墙钟时刻，**报文里没有任何数字**
// （没有 limit/used/remaining）；而我们的用量账本 `data/usage.json` 是按 `family|realm|model`
// 聚合的——国际版 6 个号共用一个桶，无法归因到具体账号。于是"到底多少次请求/多少 token 触发"
// 一直只能猜。
//
// 这里按**账号 × 模型**记当前窗口的请求数与 token 数；撞限（6004）时把**当时的计数快照**存下来，
// 那个快照就是阈值本身的观测值（首次撞限即答案，多窗口重复即可确认）。窗口边界按上游声明的
// reset 时刻推进：过了 reset 就开新窗口（与 §24.7 观察到的"锚定约 24h、重击不后移"一致）。
//
// 只读观测、不参与任何惩罚。
//
// 落盘（`data/model_usage.json`）：窗口是 **24h 锚定**的，而进程一天可能重启多次——不落盘的话
// 每次重启都把窗口清零，"用了几成"就永远只能看到重启后那一小段（实测：重启 1 分钟就撞限时
// 快照读到 window_tokens=189044，与真实 2 亿差了三个数量级）。快照与窗口一并持久化。
//
// 阈值对照口径（面板「模型用量」）：**能实证就用实证**——某 (账号, 模型) 撞过限，
// 那次的 `hit_tokens` 就是该 pair 的上限观测值（`cap_source=observed`）；没撞过才回落到
// 配置里的假设值 `Config.ModelTokenCap`（`cap_source=assumed`），面板会标出这个区别。
// 这条沿用本仓"能力只在实证后宣称"的规矩：假设值不打实证的旗号。
package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// modelUsageFlushInterval 落盘去抖：两次落盘的最小间隔（撞限时不受此限，那是要留痕的证据）。
const modelUsageFlushInterval = 10 * time.Second

// modelWindow 一个 (账号, 模型) 的当前窗口与最近一次撞限快照。
type modelWindow struct {
	start    time.Time // 本窗口首次成功请求（窗口锚点）
	requests int       // 本窗口成功请求数
	tokens   int64     // 本窗口 token 数（输入+输出，能拿到才算）
	lastAt   time.Time

	// 最近一次撞限（6004）时的快照 —— 这就是阈值观测值
	hits        int       // 累计撞限次数
	hitRequests int       // 撞限时本窗口请求数
	hitTokens   int64     // 撞限时本窗口 token 数
	hitStart    time.Time // 撞限时本窗口的起点（用于核对"锚定"语义）
	hitAt       time.Time
	hitResetAt  time.Time // 上游声明的重置时刻

	resetSeen time.Time // 已处理过的 reset 时刻（避免重复开窗）
}

// modelUsageStats 观测器（并发安全）。
type modelUsageStats struct {
	mu  sync.Mutex
	win map[string]*modelWindow

	file      string // 落盘路径；空 = 不持久化（测试用）
	capTokens int64  // 假设的 (账号,模型) 窗口 token 上限；<=0 = 不知道
	lastFlush time.Time
}

// newModelUsageStats file 为空则不落盘；capTokens <=0 表示不设假设上限
// （面板只展示用量与实证撞限值，不显示"用了几成"）。
func newModelUsageStats(file string, capTokens int64) *modelUsageStats {
	s := &modelUsageStats{win: map[string]*modelWindow{}, file: file, capTokens: capTokens}
	s.load()
	return s
}

func modelUsageKey(account, model string) string { return account + "\x00" + model }

// NoteRequest 记一次成功请求及其用量（token 拿不到时传 0）。
// 若上一次撞限声明的 reset 时刻已过，则开启新窗口（锚点 = 本次请求）。
func (s *modelUsageStats) NoteRequest(account, model string, in, out int64, now time.Time) {
	if s == nil || account == "" || model == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := modelUsageKey(account, model)
	w := s.win[k]
	if w == nil {
		w = &modelWindow{start: now}
		s.win[k] = w
	}
	switch {
	case !w.hitResetAt.IsZero() && !now.Before(w.hitResetAt):
		// 上游声明的窗口已过：开新窗口（撞限快照保留，用于面板展示"上次撞限时的计数"）。
		w.start, w.requests, w.tokens = now, 0, 0
		w.resetSeen, w.hitResetAt = w.hitResetAt, time.Time{}
	case w.hitResetAt.IsZero() && !w.start.IsZero() && now.Sub(w.start) >= modelRateLimitHorizon:
		// 从没撞过限、也就没拿到上游的 reset 时刻：按 horizon 兜底滚动，
		// 否则这个窗口会一直累加，"窗口用量"变成"历史总量"。
		w.start, w.requests, w.tokens = now, 0, 0
	}
	w.requests++
	w.tokens += in + out
	w.lastAt = now
	s.flushLocked(false)
}

// NoteLimit 记一次撞限（6004），返回**撞限时本窗口的计数**（= 阈值观测值）供日志留痕。
func (s *modelUsageStats) NoteLimit(account, model string, resetAt, now time.Time) (requests int, tokens int64, since time.Time) {
	if s == nil || account == "" || model == "" {
		return 0, 0, time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.win[modelUsageKey(account, model)]
	if w == nil {
		w = &modelWindow{start: now}
		s.win[modelUsageKey(account, model)] = w
	}
	w.hits++
	w.hitRequests, w.hitTokens, w.hitStart = w.requests, w.tokens, w.start
	w.hitAt, w.hitResetAt = now, resetAt
	s.flushLocked(true) // 阈值证据：立即落盘，不等去抖
	return w.hitRequests, w.hitTokens, w.hitStart
}

// modelWindowRow 面板/接口用的行。
type modelWindowRow struct {
	Account     string `json:"account"`
	Model       string `json:"model"`
	WindowStart string `json:"window_start"`
	Requests    int    `json:"requests"`
	Tokens      int64  `json:"tokens"`
	LastAt      string `json:"last_at,omitempty"`
	Hits        int    `json:"hits,omitempty"`
	HitRequests int    `json:"hit_requests,omitempty"`
	HitTokens   int64  `json:"hit_tokens,omitempty"`
	HitStart    string `json:"hit_start,omitempty"`
	HitAt       string `json:"hit_at,omitempty"`
	HitResetAt  string `json:"hit_reset_at,omitempty"`

	// 阈值对照（见文件头）：cap_tokens 优先取该 pair 的实证撞限值，否则用配置的假设值。
	CapTokens       int64   `json:"cap_tokens,omitempty"`
	CapSource       string  `json:"cap_source,omitempty"` // observed | assumed
	UsedPct         float64 `json:"used_pct,omitempty"`   // 本窗口已用占比（0–100+）
	RemainingTokens int64   `json:"remaining_tokens,omitempty"`
}

// row 由内部窗口构造输出行（含阈值对照口径）。
func (s *modelUsageStats) row(account, model string, w *modelWindow) modelWindowRow {
	out := modelWindowRow{
		Account: account, Model: model,
		WindowStart: w.start.Format(time.RFC3339), Requests: w.requests, Tokens: w.tokens,
		LastAt: tsOrEmpty(w.lastAt),
		Hits:   w.hits, HitRequests: w.hitRequests, HitTokens: w.hitTokens,
		HitStart: tsOrEmpty(w.hitStart), HitAt: tsOrEmpty(w.hitAt),
		HitResetAt: tsOrEmpty(w.hitResetAt),
	}
	// 实证优先：撞过限的 pair，那次撞限时的窗口 token 数就是它的上限观测值。
	if w.hitTokens > 0 {
		out.CapTokens, out.CapSource = w.hitTokens, "observed"
	} else if s.capTokens > 0 {
		out.CapTokens, out.CapSource = s.capTokens, "assumed"
	}
	if out.CapTokens > 0 {
		out.UsedPct = float64(out.Tokens) / float64(out.CapTokens) * 100
		out.RemainingTokens = out.CapTokens - out.Tokens
	}
	return out
}

// Snapshot 当前观测全量（按账号、模型排序）。
func (s *modelUsageStats) Snapshot() []modelWindowRow {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]modelWindowRow, 0, len(s.win))
	for k, w := range s.win {
		acct, model := splitModelUsageKey(k)
		out = append(out, s.row(acct, model, w))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// ForAccount 该账号在各模型上的窗口观测（面板账号行内联展示用：model → 行）。
func (s *modelUsageStats) ForAccount(account string) map[string]modelWindowRow {
	if s == nil || account == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out map[string]modelWindowRow
	for k, w := range s.win {
		acct, model := splitModelUsageKey(k)
		if acct != account {
			continue
		}
		if out == nil {
			out = map[string]modelWindowRow{}
		}
		out[model] = s.row(acct, model, w)
	}
	return out
}

// CapTokens 当前假设的上限（面板提示用；0 = 未设）。
func (s *modelUsageStats) CapTokens() int64 {
	if s == nil {
		return 0
	}
	return s.capTokens
}

// ---------------------------------------------------------------------------
// 落盘
// ---------------------------------------------------------------------------

// modelUsageFile 落盘形状：账号 → 模型 → 窗口状态（嵌套 map 比扁平的 \x00 键可读得多）。
type modelUsageFile struct {
	Accounts map[string]map[string]modelWindowState `json:"accounts"`
}

// modelWindowState 落盘的窗口状态。时间一律用 RFC3339 字符串：空值自然省略
// （time.Time 的 omitempty 对零值结构体不生效，会写出 4 个 0001-01-01 噪音）。
type modelWindowState struct {
	Start       string `json:"start"`
	Requests    int    `json:"requests"`
	Tokens      int64  `json:"tokens"`
	LastAt      string `json:"last_at,omitempty"`
	Hits        int    `json:"hits,omitempty"`
	HitRequests int    `json:"hit_requests,omitempty"`
	HitTokens   int64  `json:"hit_tokens,omitempty"`
	HitStart    string `json:"hit_start,omitempty"`
	HitAt       string `json:"hit_at,omitempty"`
	HitResetAt  string `json:"hit_reset_at,omitempty"`
	ResetSeen   string `json:"reset_seen,omitempty"`
}

// parseTS 解析落盘时间戳；空串/坏值 → 零值（宁可少一条时间，也别因一处坏值丢整个文件）。
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (s *modelUsageStats) load() {
	if s.file == "" {
		return
	}
	raw, err := os.ReadFile(s.file)
	if err != nil {
		return // 首次运行：无文件即空表
	}
	var f modelUsageFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("model usage: 解析 %s 失败（忽略，从空开始）: %v", s.file, err)
		return
	}
	for acct, models := range f.Accounts {
		for model, st := range models {
			s.win[modelUsageKey(acct, model)] = &modelWindow{
				start: parseTS(st.Start), requests: st.Requests, tokens: st.Tokens, lastAt: parseTS(st.LastAt),
				hits: st.Hits, hitRequests: st.HitRequests, hitTokens: st.HitTokens,
				hitStart: parseTS(st.HitStart), hitAt: parseTS(st.HitAt), hitResetAt: parseTS(st.HitResetAt),
				resetSeen: parseTS(st.ResetSeen),
			}
		}
	}
}

// flushLocked 落盘（临时文件 + rename 原子替换）。调用方需持锁。
// force=true 时跳过去抖（撞限证据要立刻留痕）。
func (s *modelUsageStats) flushLocked(force bool) {
	if s.file == "" {
		return
	}
	if !force && time.Since(s.lastFlush) < modelUsageFlushInterval {
		return
	}
	s.lastFlush = time.Now()
	out := modelUsageFile{Accounts: map[string]map[string]modelWindowState{}}
	for k, w := range s.win {
		acct, model := splitModelUsageKey(k)
		if acct == "" || model == "" {
			continue
		}
		if out.Accounts[acct] == nil {
			out.Accounts[acct] = map[string]modelWindowState{}
		}
		out.Accounts[acct][model] = modelWindowState{
			Start: tsOrEmpty(w.start), Requests: w.requests, Tokens: w.tokens, LastAt: tsOrEmpty(w.lastAt),
			Hits: w.hits, HitRequests: w.hitRequests, HitTokens: w.hitTokens,
			HitStart: tsOrEmpty(w.hitStart), HitAt: tsOrEmpty(w.hitAt), HitResetAt: tsOrEmpty(w.hitResetAt),
			ResetSeen: tsOrEmpty(w.resetSeen),
		}
	}
	raw, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		log.Printf("model usage: 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.file), 0o700); err != nil {
		log.Printf("model usage: 建目录失败: %v", err)
		return
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("model usage: 写盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, s.file); err != nil {
		log.Printf("model usage: 替换失败: %v", err)
	}
}

// Flush 立即落盘（优雅退出用）。
func (s *modelUsageStats) Flush() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked(true)
}

func splitModelUsageKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// tsOrEmpty 零值返回空串（JSON 里不出现 0001-01-01）。
func tsOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
