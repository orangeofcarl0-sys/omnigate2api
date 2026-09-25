// Token 用量统计与「白嫖金额」换算（SPEC §34 观测面）。
//
// 落账口径：
//   - 只记**上游真实 usage**（无上游用量不估算，避免估算值污染统计）；
//   - 按 (日期, 家族, 区域, 模型) 聚合，日期取北京时间自然日（与调度器同口径）；
//   - 持久化 data/usage.json（10s 去抖落盘，保留 90 天，临时文件 + rename 原子替换）。
//
// 白嫖口径（§34.2 拍板）——「没花真金白银的用量」折算成钱：
//   - 积分抵扣：腾讯请求上游报 credit>0。这些积分来自签到/成长任务/宠物/新号赠送等
//     免费途径，按官方售卖牌价折算：国内 ¥0.014/积分（700 元/5 万积分）、
//     国际 $0.03/credits（$15/500 credits）、汇率 7.15 —— 牌价来源：社区对官方售卖页
//     的整理（maiphucgiang_codebuddy2api app/credits.py），可在 config.json 的
//     pricing 块覆盖；
//   - 免费直用：按该账号区域的促销生效判定（非 paid，如 hy3 Free now）与华为福利
//     模型（maas_type benefit，每日 1000 万 token 额度）——金额 = token × 模型牌价
//     倍率 × 基准（即"正常 API 调用花费"折成积分）× 区域积分牌价。基准
//     credit_per_1k_mult（每千 token × 倍率的积分数）实测 0.105：glm-5.3（x0.79）
//     3874 token 无缓存请求记 0.32 积分。全球 config 常把促销烘进倍率（dsv41f
//     全球 "x0.00"）→ 倍率为 0 时回落国内牌价作参考；pricing.free_cny_per_mtoken
//     可用固定单价覆盖整条折算链。为什么不用 credit==0 判定：微小请求的积分按
//     显示精度取整后为 0，会把付费用量误判成白嫖（实测 glm-5.3 全球 48in/16out
//     记 0 积分）；
//   - 华为非福利模型走真实计费（用户自己的华为账户），不计入白嫖，token 照常统计。
package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"omnigate2api/internal/upstream"
)

// statsCST 自然日按北京时间切（与调度器同口径）。
var statsCST = time.FixedZone("CST", 8*3600)

// usageRetention 保留天数（更早的日期在落盘时裁掉）。
const usageRetention = 90

// usageFlushInterval 落盘去抖：两次落盘的最小间隔。
const usageFlushInterval = 10 * time.Second

// usageDaysDefault /admin/api/usage 逐日视图默认天数（?days= 可覆盖，上限 usageRetention）。
const usageDaysDefault = 14

// Pricing 白嫖金额换算单价（config.json `pricing` 块；<=0 的牌价回落内置默认）。
type Pricing struct {
	CreditCNY        float64 `json:"credit_cny"`          // 国内 1 积分 ≈ ¥（官方 700 元/5 万积分 → 0.014）
	CreditUSD        float64 `json:"credit_usd"`          // 国际 1 credits ≈ $（官方 $15/500 → 0.03）
	USDCNY           float64 `json:"usd_cny"`             // 汇率（¥/$）
	CreditPer1KMult  float64 `json:"credit_per_1k_mult"`  // 牌价基准：每千 token × 倍率 的积分数（实测 0.105，见 §34.2）
	FreeCNYPerMToken float64 `json:"free_cny_per_mtoken"` // 免费直用固定单价（¥/百万 token；>0 时优先于倍率折算）
}

// defaultPricing 社区整理的官方售卖牌价 + 实测的 token→积分基准（见包注释）。
func defaultPricing() Pricing {
	return Pricing{CreditCNY: 0.014, CreditUSD: 0.03, USDCNY: 7.15, CreditPer1KMult: 0.105}
}

// UsageAgg 单键聚合（键 = 日期 × 家族 × 区域 × 模型）。
// Uncertain* = 落账时促销缓存未冷、无法判定免费的用量——快照读取时按当前促销
// 状态一次性折叠进 free 或 paid（冷启动窗口自愈，见 §34.2）。
type UsageAgg struct {
	Requests          int64   `json:"requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CachedTokens      int64   `json:"cached_tokens"`
	Credits           float64 `json:"credits"`
	FreeRequests      int64   `json:"free_requests"`
	FreeTokens        int64   `json:"free_tokens"`
	UncertainRequests int64   `json:"uncertain_requests,omitempty"`
	UncertainTokens   int64   `json:"uncertain_tokens,omitempty"`
}

func (a *UsageAgg) add(b *UsageAgg) {
	if b == nil {
		return
	}
	a.Requests += b.Requests
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.CachedTokens += b.CachedTokens
	a.Credits += b.Credits
	a.FreeRequests += b.FreeRequests
	a.FreeTokens += b.FreeTokens
	a.UncertainRequests += b.UncertainRequests
	a.UncertainTokens += b.UncertainTokens
}

// UsageStats 进程内聚合 + 落盘。并发安全；nil 存储时调用方应跳过落账。
type UsageStats struct {
	mu        sync.Mutex
	file      string
	pricing   Pricing
	days      map[string]map[string]*UsageAgg // date -> "family|realm|model" -> agg
	lastFlush time.Time
}

// NewUsageStats 载入已有统计（文件不存在/损坏则从空开始）。file 为空 = 不持久化（内存态，测试用）。
func NewUsageStats(file string, p *Pricing) *UsageStats {
	pr := defaultPricing()
	if p != nil {
		if p.CreditCNY > 0 {
			pr.CreditCNY = p.CreditCNY
		}
		if p.CreditUSD > 0 {
			pr.CreditUSD = p.CreditUSD
		}
		if p.USDCNY > 0 {
			pr.USDCNY = p.USDCNY
		}
		if p.CreditPer1KMult > 0 {
			pr.CreditPer1KMult = p.CreditPer1KMult
		}
		pr.FreeCNYPerMToken = p.FreeCNYPerMToken // 0 = 未配置，走牌价倍率折算
	}
	u := &UsageStats{file: file, pricing: pr, days: map[string]map[string]*UsageAgg{}}
	u.load()
	return u
}

type usageFile struct {
	Days map[string]map[string]*UsageAgg `json:"days"`
}

func (u *UsageStats) load() {
	if u.file == "" {
		return
	}
	raw, err := os.ReadFile(u.file)
	if err != nil {
		return // 首次运行：无文件即空表
	}
	var f usageFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("usage stats: 解析 %s 失败（忽略，从空开始）: %v", u.file, err)
		return
	}
	if f.Days != nil {
		u.days = f.Days
	}
}

// Record 落账一次真实上游用量。free=已判定免费直用（促销非 paid / 华为福利）；
// uncertain=腾讯请求但促销缓存未冷、暂无法判定（快照时折叠，见 Snapshot）。
func (u *UsageStats) Record(family, realm, model string, in, out, cached int64, credit float64, free, uncertain bool) {
	if u == nil {
		return
	}
	day := time.Now().In(statsCST).Format("2006-01-02")
	key := family + "|" + realm + "|" + model
	u.mu.Lock()
	defer u.mu.Unlock()
	dayMap := u.days[day]
	if dayMap == nil {
		dayMap = map[string]*UsageAgg{}
		u.days[day] = dayMap
	}
	agg := dayMap[key]
	if agg == nil {
		agg = &UsageAgg{}
		dayMap[key] = agg
	}
	agg.Requests++
	agg.InputTokens += in
	agg.OutputTokens += out
	agg.CachedTokens += cached
	agg.Credits += credit
	switch {
	case uncertain:
		agg.UncertainRequests++
		agg.UncertainTokens += in + out
	case free:
		agg.FreeRequests++
		agg.FreeTokens += in + out
	}
	if time.Since(u.lastFlush) >= usageFlushInterval {
		u.flushLocked()
	}
}

// creditCNY 单积分人民币价值（国际 credits 按美元牌价 × 汇率；华为无积分概念 → 0）。
func (u *UsageStats) creditCNY(realm string) float64 {
	if realm == "global" {
		return u.pricing.CreditUSD * u.pricing.USDCNY
	}
	if realm == "cn" {
		return u.pricing.CreditCNY
	}
	return 0
}

// flushLocked 落盘（临时文件 + rename）。调用方需持锁。
func (u *UsageStats) flushLocked() {
	u.lastFlush = time.Now()
	if u.file == "" {
		return
	}
	// 裁掉保留期之外的日期
	cutoff := time.Now().In(statsCST).AddDate(0, 0, -usageRetention).Format("2006-01-02")
	for d := range u.days {
		if d < cutoff {
			delete(u.days, d)
		}
	}
	raw, err := json.MarshalIndent(usageFile{Days: u.days}, "", " ")
	if err != nil {
		log.Printf("usage stats: 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(u.file), 0o700); err != nil {
		log.Printf("usage stats: 建目录失败: %v", err)
		return
	}
	tmp := u.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("usage stats: 写盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, u.file); err != nil {
		log.Printf("usage stats: 替换失败: %v", err)
	}
}

// Flush 立即落盘（测试与优雅退出用）。
func (u *UsageStats) Flush() {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.flushLocked()
}

// ---------------------------------------------------------------------------
// 快照（admin API）
// ---------------------------------------------------------------------------

// UsageAggOut 聚合 + 换算后的金额（SPEC §34.2 拍板：白嫖金额 = 全部用量按
// "正常 API 调用花费"折算——token × 模型牌价倍率 × 基准 × 区域积分单价）。
// credits_cny = 实际消耗积分折价（上游 credit 字段，仅国内域上报）；
// free_cny = 其中促销免费直用部分；uncertain_* = 促销缓存未冷、暂未归类的用量。
type UsageAggOut struct {
	Requests          int64   `json:"requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CachedTokens      int64   `json:"cached_tokens"`
	Credits           float64 `json:"credits"`
	FreeRequests      int64   `json:"free_requests"`
	FreeTokens        int64   `json:"free_tokens"`
	UncertainRequests int64   `json:"uncertain_requests,omitempty"`
	UncertainTokens   int64   `json:"uncertain_tokens,omitempty"`
	APICostCNY        float64 `json:"api_cost_cny"`
	CreditsCNY        float64 `json:"credits_cny"`
	FreeCNY           float64 `json:"free_cny"`
	FreePriced        bool    `json:"free_priced"`
}

// listMultiplier 模型的牌价倍率（积分/千 token 的乘数）：先取所属区域，倍率被
// 促销烘成 0（如 dsv41f 全球 "x0.00"）或未命中时回落国内牌价作参考——回落时
// 积分单价也用国内的（同一参考口径）。
func (u *UsageStats) listMultiplier(realm, model string) (mult float64, priceRealm string, ok bool) {
	if m, k := promoMultiplier(realm, model); k && m > 0 {
		return m, realm, true
	}
	if realm != "cn" {
		if m, k := promoMultiplier("cn", model); k && m > 0 {
			return m, "cn", true
		}
	}
	return 0, "", false
}

// freeMoney 免费直用金额（SPEC §34.2）：默认按该模型牌价倍率折算成积分
// （= 正常 API 调用花费）再按参考区域的积分牌价折钱；pricing.free_cny_per_mtoken>0
// 时用固定单价覆盖。倍率未知（促销缓存冷/模型不在清单）→ 未定价。
func (u *UsageStats) freeMoney(realm, model string, tokens int64) (float64, bool) {
	if u.pricing.FreeCNYPerMToken > 0 {
		return float64(tokens) / 1e6 * u.pricing.FreeCNYPerMToken, true
	}
	mult, priceRealm, ok := u.listMultiplier(realm, model)
	if !ok {
		return 0, false
	}
	credits := float64(tokens) / 1000 * mult * u.pricing.CreditPer1KMult
	return credits * u.creditCNY(priceRealm), true
}

// fill 聚合 → 输出（金额按该行的区域与模型折算）。agg 允许为 nil：跨日后
// "昨天用过、今天没用"的模型在今日档就是无记录，按零值出参（否则空指针）。
func (u *UsageStats) fill(realm, model string, agg *UsageAgg) UsageAggOut {
	if agg == nil {
		agg = &UsageAgg{}
	}
	out := UsageAggOut{Requests: agg.Requests, InputTokens: agg.InputTokens, OutputTokens: agg.OutputTokens,
		CachedTokens: agg.CachedTokens, Credits: agg.Credits, FreeRequests: agg.FreeRequests, FreeTokens: agg.FreeTokens,
		UncertainRequests: agg.UncertainRequests, UncertainTokens: agg.UncertainTokens}
	out.CreditsCNY = agg.Credits * u.creditCNY(realm)
	out.FreeCNY, out.FreePriced = u.freeMoney(realm, model, agg.FreeTokens)
	out.APICostCNY = u.apiCost(realm, model, agg.InputTokens+agg.OutputTokens)
	return out
}

// apiCost 正常 API 调用花费：token × 模型牌价倍率 × 基准 × 区域积分单价。
// 倍率未知（缓存冷/模型不在清单/倍率被烘成 0 且国内也无）→ 0（未定价）。
func (u *UsageStats) apiCost(realm, model string, tokens int64) float64 {
	mult, priceRealm, ok := u.listMultiplier(realm, model)
	if !ok {
		return 0
	}
	return float64(tokens) / 1000 * mult * u.pricing.CreditPer1KMult * u.creditCNY(priceRealm)
}

// UsageModelRow 按模型的累计/今日两行。
type UsageModelRow struct {
	Family string      `json:"family"`
	Realm  string      `json:"realm"`
	Model  string      `json:"model"`
	Total  UsageAggOut `json:"total"`
	Today  UsageAggOut `json:"today"`
}

// UsageDayRow 单日汇总（北京自然日）：面板「按天」视图的一行。
// 字段与 UsageAggOut 同形（内嵌），金额同样按各行区域折算。
type UsageDayRow struct {
	Date string `json:"date"`
	UsageAggOut
}

// UsageSnapshot /admin/api/usage 的响应体。
type UsageSnapshot struct {
	Enabled bool            `json:"enabled"`
	Date    string          `json:"date"`
	Days    []UsageDayRow   `json:"days"`
	Today   UsageAggOut     `json:"today"`
	Total   UsageAggOut     `json:"total"`
	Models  []UsageModelRow `json:"models"`
	Pricing Pricing         `json:"pricing"`
}

// moneyAcc 跨区域汇总的累加器：金额在累加时按各自行所属区域折算，
// 避免"先跨区域求和再折算"用错单价。
type moneyAcc struct {
	agg        UsageAgg
	apiCNY     float64
	creditsCNY float64
	freeCNY    float64
	freePriced bool
}

func (u *UsageStats) addRealm(a *moneyAcc, realm, model string, agg *UsageAgg) {
	a.agg.add(agg)
	a.apiCNY += u.apiCost(realm, model, agg.InputTokens+agg.OutputTokens)
	a.creditsCNY += agg.Credits * u.creditCNY(realm)
	if cny, priced := u.freeMoney(realm, model, agg.FreeTokens); priced {
		a.freePriced = true
		a.freeCNY += cny
	}
}

func (u *UsageStats) out(a *moneyAcc) UsageAggOut {
	out := u.fill("", "", &a.agg) // realm/model="" 只取计数；金额用累加值覆盖
	out.APICostCNY = a.apiCNY
	out.CreditsCNY, out.FreeCNY, out.FreePriced = a.creditsCNY, a.freeCNY, a.freePriced
	return out
}

// Snapshot 今日 / 累计 / 按模型 / 逐日（默认近 usageDaysDefault 天）。
func (u *UsageStats) Snapshot() *UsageSnapshot { return u.SnapshotDays(usageDaysDefault) }

// SnapshotDays 同 Snapshot，但逐日视图取最近 n 天（n<=0 时用默认值）。
func (u *UsageStats) SnapshotDays(n int) *UsageSnapshot {
	if n <= 0 {
		n = usageDaysDefault
	}
	if n > usageRetention {
		n = usageRetention
	}
	today := time.Now().In(statsCST).Format("2006-01-02")
	u.mu.Lock()
	defer u.mu.Unlock()
	// 冷窗口自愈：落账时促销缓存未冷 → uncertain 的用量，按当前促销状态一次性
	// 折叠进 free 或 paid（改写存储，永久修正）。缓存仍冷 → 保留，下次快照再试。
	// 注意：用"当前"促销状态回溯历史 uncertain 用量——促销跨日过期时会有毫级误差，
	// 换取的是冷窗口数据不丢失。
	for _, dayMap := range u.days {
		for k, agg := range dayMap {
			if agg.UncertainTokens == 0 && agg.UncertainRequests == 0 {
				continue
			}
			parts := strings.SplitN(k, "|", 3)
			if len(parts) != 3 {
				continue
			}
			if free, ok := promoFree(parts[1], parts[2], time.Now()); ok {
				if free {
					agg.FreeTokens += agg.UncertainTokens
					agg.FreeRequests += agg.UncertainRequests
				}
				agg.UncertainTokens, agg.UncertainRequests = 0, 0
			}
		}
	}
	snap := &UsageSnapshot{Enabled: true, Date: today, Pricing: u.pricing}
	type key struct{ family, realm, model string }
	totals := map[key]*UsageAgg{}
	dayAccs := map[string]*moneyAcc{} // 逐日汇总（金额按各行区域折算）
	var totalAcc, todayAcc moneyAcc
	for date, dayMap := range u.days {
		for k, agg := range dayMap {
			parts := strings.SplitN(k, "|", 3)
			if len(parts) != 3 {
				continue
			}
			kk := key{parts[0], parts[1], parts[2]}
			if totals[kk] == nil {
				totals[kk] = &UsageAgg{}
			}
			totals[kk].add(agg)
			u.addRealm(&totalAcc, kk.realm, kk.model, agg)
			if date == today {
				u.addRealm(&todayAcc, kk.realm, kk.model, agg)
			}
			if dayAccs[date] == nil {
				dayAccs[date] = &moneyAcc{}
			}
			u.addRealm(dayAccs[date], kk.realm, kk.model, agg)
		}
	}
	snap.Total = u.out(&totalAcc)
	snap.Today = u.out(&todayAcc)
	dates := make([]string, 0, len(dayAccs))
	for d := range dayAccs {
		dates = append(dates, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates))) // 日期倒序：最新在前
	for i, d := range dates {
		if i >= n {
			break
		}
		snap.Days = append(snap.Days, UsageDayRow{Date: d, UsageAggOut: u.out(dayAccs[d])})
	}
	for kk, agg := range totals {
		row := UsageModelRow{Family: kk.family, Realm: kk.realm, Model: kk.model}
		row.Total = u.fill(kk.realm, kk.model, agg)
		// 今日档可能没有这个模型（跨日后"昨天用过、今天没用"）：nil 走零值。
		row.Today = u.fill(kk.realm, kk.model, u.days[today][kk.family+"|"+kk.realm+"|"+kk.model])
		snap.Models = append(snap.Models, row)
	}
	sort.Slice(snap.Models, func(i, j int) bool {
		ti, tj := snap.Models[i].Total, snap.Models[j].Total
		return ti.InputTokens+ti.OutputTokens > tj.InputTokens+tj.OutputTokens
	})
	return snap
}

// isBenefitModel 华为福利模型（maas_type benefit，每日 token 额度）→ 免费直用。
// 其余华为模型走真实计费（用户自己的华为账户），不计入白嫖。
func isBenefitModel(model string) bool {
	for _, m := range staticModels {
		if m.id == model {
			return m.access == upstream.AccessBenefit
		}
	}
	return false
}
