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
// 只读观测、不参与任何惩罚；进程内存态（重启清空——阈值观测要的是连续窗口内的计数，
// 关键结论同时会写进日志行，便于长期留痕）。
package server

import (
	"sort"
	"sync"
	"time"
)

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
}

func newModelUsageStats() *modelUsageStats {
	return &modelUsageStats{win: map[string]*modelWindow{}}
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
	w := s.win[modelUsageKey(account, model)]
	if w == nil {
		w = &modelWindow{start: now}
		s.win[modelUsageKey(account, model)] = w
	}
	if !w.hitResetAt.IsZero() && !now.Before(w.hitResetAt) {
		// 上游声明的窗口已过：开新窗口（撞限快照保留，用于面板展示"上次撞限时的计数"）。
		w.start, w.requests, w.tokens = now, 0, 0
		w.resetSeen, w.hitResetAt = w.hitResetAt, time.Time{}
	}
	w.requests++
	w.tokens += in + out
	w.lastAt = now
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
		out = append(out, modelWindowRow{
			Account: acct, Model: model,
			WindowStart: w.start.Format(time.RFC3339), Requests: w.requests, Tokens: w.tokens,
			LastAt: tsOrEmpty(w.lastAt),
			Hits:   w.hits, HitRequests: w.hitRequests, HitTokens: w.hitTokens,
			HitStart: tsOrEmpty(w.hitStart), HitAt: tsOrEmpty(w.hitAt),
			HitResetAt: tsOrEmpty(w.hitResetAt),
		})
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
		out[model] = modelWindowRow{
			Account: acct, Model: model,
			WindowStart: w.start.Format(time.RFC3339), Requests: w.requests, Tokens: w.tokens,
			LastAt: tsOrEmpty(w.lastAt),
			Hits:   w.hits, HitRequests: w.hitRequests, HitTokens: w.hitTokens,
			HitStart: tsOrEmpty(w.hitStart), HitAt: tsOrEmpty(w.hitAt),
			HitResetAt: tsOrEmpty(w.hitResetAt),
		}
	}
	return out
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
