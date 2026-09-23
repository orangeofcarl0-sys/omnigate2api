// 腾讯成长中心·任务面（SPEC §32.8）：任务列表/接单/领奖（双形态字段兼容）、
// 专家与技能市场（任务对象 id 来源）。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"omnigate2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 成长中心任务（SPEC §32 阶段 3：积分/能量主来源，宠物盲盒能量的唯一入口）
// ---------------------------------------------------------------------------

// GrowthTask 成长中心任务。**双形态实证**（2026-09-23 活测对照）：
//   - CN 当前赛季（19 任务）：task_code / accept_status(not_accepted|…) / reward_* 全量；
//   - 全球版与旧赛季（5 任务 stub）：code / status(available)（accept 返回 task not found）。
//
// 两代命名并存解析，对外统一走 TaskCode()/TaskStatus()。
type GrowthTask struct {
	Code         string `json:"code"`
	TaskCodeRaw  string `json:"task_code"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	LevelName    string `json:"level_name"`
	Locked       bool   `json:"locked"`
	Status       string `json:"status"`        // 旧形态：available|accepted|…
	AcceptStatus string `json:"accept_status"` // CN 当前形态：not_accepted|accepted|in_progress|completed|claimed
	RewardCredit int64  `json:"reward_credit"`
	RewardEnergy int64  `json:"reward_energy"`
}

// TaskCode 任务编码（两代命名兼容，task_code 优先——CN 当前赛季用后者）。
func (t GrowthTask) TaskCode() string {
	if t.TaskCodeRaw != "" {
		return t.TaskCodeRaw
	}
	return t.Code
}

// TaskStatus 任务状态（两代命名兼容，accept_status 优先）。
// 归一化：available/not_accepted 等价（可接单）。
func (t GrowthTask) TaskStatus() string {
	st := t.AcceptStatus
	if st == "" {
		st = t.Status
	}
	return st
}

// Actionable 是否可接单（两代首态等价）。
func (t GrowthTask) Actionable() bool {
	st := t.TaskStatus()
	return st == "available" || st == "not_accepted"
}

// Completed 是否已完成待领奖。
func (t GrowthTask) Completed() bool {
	return t.TaskStatus() == "completed"
}

// GrowthTasks 查询任务列表。
func (c *TencentClient) GrowthTasks(acct *auth.Auth) ([]GrowthTask, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for growth tasks")
	}
	raw, status, err := c.petRequest(acct, http.MethodGet, "/v2/activity/growth/tasks", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Code  int64        `json:"code"`
		Msg   string       `json:"msg"`
		Tasks []GrowthTask `json:"tasks"`
		Data  struct {
			Tasks []GrowthTask `json:"tasks"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("growth tasks parse: %w", uerr)
	}
	if err := checkBiz("growth tasks", status, env.Code, env.Msg); err != nil {
		return nil, err
	}
	if len(env.Tasks) > 0 {
		return env.Tasks, nil
	}
	return env.Data.Tasks, nil
}

// GrowthAcceptTasks 批量接单（≤20/批）；返回 task_code → status/message。
func (c *TencentClient) GrowthAcceptTasks(acct *auth.Auth, codes []string) (map[string]string, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for task accept")
	}
	if len(codes) == 0 {
		return map[string]string{}, nil
	}
	if len(codes) > 20 {
		codes = codes[:20]
	}
	body, _ := json.Marshal(map[string]any{"task_codes": codes})
	raw, status, err := c.petRequest(acct, http.MethodPost, "/v2/activity/growth/tasks/accept", body)
	if err != nil {
		return nil, err
	}
	type acceptResult struct {
		TaskCode string `json:"task_code"`
		Status   string `json:"status"`
		Message  string `json:"message"`
	}
	var env struct {
		Code    int64          `json:"code"`
		Msg     string         `json:"msg"`
		Results []acceptResult `json:"results"`
		Data    struct {
			Results []acceptResult `json:"results"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	if err := checkBiz("task accept", status, env.Code, env.Msg); err != nil {
		return nil, err
	}
	// 真实嵌套为 data.results（2026-09-23 实证）；顶层形态兼容保留。
	results := env.Results
	if len(results) == 0 {
		results = env.Data.Results
	}
	out := make(map[string]string, len(results))
	for _, r := range results {
		msg := r.Status
		if r.Message != "" {
			msg += ": " + r.Message
		}
		out[r.TaskCode] = msg
	}
	return out, nil
}

// GrowthClaimTask 领取任务奖励（路径带 task_code，空 body）。
func (c *TencentClient) GrowthClaimTask(acct *auth.Auth, code string) (int64, int64, bool, error) {
	if acct == nil {
		return 0, 0, false, fmt.Errorf("account required for task claim")
	}
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/tasks/"+code+"/claim", []byte("{}"))
	if err != nil {
		return 0, 0, false, err
	}
	var env struct {
		Code           int64  `json:"code"`
		Msg            string `json:"msg"`
		Credit         int64  `json:"credit"`
		Energy         int64  `json:"energy"`
		AlreadyClaimed bool   `json:"already_claimed"`
	}
	_ = json.Unmarshal(raw, &env)
	if err := checkBiz("task claim", status, env.Code, env.Msg); err != nil {
		return 0, 0, false, err
	}
	return env.Credit, env.Energy, env.AlreadyClaimed, nil
}

// MarketExpert 专家市场条目（/v2/operation-platform/market/expert/list）。
type MarketExpert struct {
	ID   string `json:"expert_id"`
	Name string `json:"agent_name"`
	Type string `json:"expert_type"`
}

// MarketSkill 技能市场条目（/v2/operation-platform/market/skill/list）。
type MarketSkill struct {
	ID      string `json:"skill_id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// marketPost 市场域 POST（chat 域；统一机制·SPEC §32）。
func (c *TencentClient) marketPost(acct *auth.Auth, path string, body any) ([]byte, error) {
	raw, _ := json.Marshal(body)
	out, status, err := c.chatDo(acct, http.MethodPost, path, raw, false)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("market %s http=%d: %s", path, status, truncateStr(string(out), 160))
	}
	return out, nil
}

// MarketQuery 专家市场查询参数（SPEC §32.8：字段名 page_size 为下划线形态，
// 此前误用 pageSize 导致每页仅返回默认 20 条）。
type MarketQuery struct {
	Page     int
	PageSize int
	Keyword  string // 空 = 不过滤
}

// GrowthExperts 拉取专家市场一页。
func (c *TencentClient) GrowthExperts(acct *auth.Auth, q MarketQuery) ([]MarketExpert, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required")
	}
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.PageSize <= 0 {
		q.PageSize = 50
	}
	body := map[string]any{"page": q.Page, "page_size": q.PageSize}
	if q.Keyword != "" {
		body["keyword"] = q.Keyword
	}
	raw, err := c.marketPost(acct, "/v2/operation-platform/market/expert/list", body)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data struct {
			Experts []MarketExpert `json:"experts"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("experts parse: %w", uerr)
	}
	return env.Data.Experts, nil
}

// GrowthExpertsPaged 按页抓取（去重），最多 maxPages 页。
func (c *TencentClient) GrowthExpertsPaged(acct *auth.Auth, maxPages int, keyword string) []MarketExpert {
	var out []MarketExpert
	seen := map[string]bool{}
	for page := 1; page <= maxPages; page++ {
		ex, err := c.GrowthExperts(acct, MarketQuery{Page: page, PageSize: 50, Keyword: keyword})
		if err != nil || len(ex) == 0 {
			break
		}
		for _, e := range ex {
			if e.ID == "" || seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	return out
}

// Scene 模板场景（template_5 的对象 id 来源）。
type Scene struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GrowthScenes 拉取场景清单（失败回落内置表，社区 DEFAULT_SCENES 同源）。
func (c *TencentClient) GrowthScenes(acct *auth.Auth) []Scene {
	fallback := []Scene{{"0", "幻灯片"}, {"2", "视频生成"}, {"4", "深度研究"}, {"6", "文档处理"},
		{"8", "数据分析"}, {"10", "可视化"}, {"12", "金融服务"}, {"14", "产品管理"}}
	if acct == nil {
		return fallback
	}
	raw, status, err := c.chatDo(acct, http.MethodGet, "/console/as/support/scenes?locale=zh-CN", nil, false)
	if err != nil || status >= 400 {
		return fallback
	}
	var env struct {
		Data struct {
			Scenes []struct {
				ID   json.Number `json:"id"`
				Name string      `json:"name"`
			} `json:"scenes"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil || len(env.Data.Scenes) == 0 {
		return fallback
	}
	out := make([]Scene, 0, len(env.Data.Scenes))
	for _, s := range env.Data.Scenes {
		out = append(out, Scene{ID: s.ID.String(), Name: s.Name})
	}
	return out
}

// AppearanceTheme 外观主题（Hp_Appearance 的 resourceKey 来源）。
type AppearanceTheme struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	VipLevel string `json:"vip_level"`
	Series   string `json:"series"`
}

// GrowthThemes 拉取主题目录（失败回落内置和平精英主题，社区 PE_THEME 同源）。
func (c *TencentClient) GrowthThemes(acct *auth.Auth) []AppearanceTheme {
	if acct == nil {
		return []AppearanceTheme{{"theme-tkmw7j", "和平精英激战金秋", "free", "craft"}}
	}
	body, _ := json.Marshal(map[string]any{"platform": "client", "kind": "theme", "version": "2.137.1", "lang": "zh-CN"})
	raw, status, err := c.billingDo(acct, http.MethodPost, "/v2/operation-platform/appearance/resources", body)
	if err != nil || status >= 400 {
		return []AppearanceTheme{{"theme-tkmw7j", "和平精英激战金秋", "free", "craft"}}
	}
	var env struct {
		Data struct {
			Resources []AppearanceTheme `json:"resources"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return []AppearanceTheme{{"theme-tkmw7j", "和平精英激战金秋", "free", "craft"}}
	}
	out := make([]AppearanceTheme, 0, 4)
	for _, t := range env.Data.Resources {
		if strings.Contains(t.Name, "和平精英") || strings.Contains(strings.ToLower(t.Name), "pubg") {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		out = append(out, AppearanceTheme{"theme-tkmw7j", "和平精英激战金秋", "free", "craft"})
	}
	return out
}

// GrowthSkills 拉取技能市场。
func (c *TencentClient) GrowthSkills(acct *auth.Auth, pageSize int) ([]MarketSkill, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required")
	}
	raw, err := c.marketPost(acct, "/v2/operation-platform/market/skill/list",
		map[string]any{"page": 1, "pageSize": pageSize})
	if err != nil {
		return nil, err
	}
	var env struct {
		Data struct {
			Skills []MarketSkill `json:"skills"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("skills parse: %w", uerr)
	}
	return env.Data.Skills, nil
}

// expertUseEvent 专家使用事件（expert_5 / Expert_team_use_3 / Expert_lighthouse 共用）。
// idx 参与会话/请求 id——**每个事件必须唯一**，否则服务端按会话去重只计 1 次
// （实测：5 个专家事件共用同一 cid 时 expert_5 只累计 1/5）。
func expertUseEvent(uid string, idx int, ex MarketExpert, expertType string, base map[string]any) map[string]any {
	now := time.Now().UnixMilli()
	cid := fmt.Sprintf("wb-ex-%d-%d", now, idx)
	rid := cid + "-1"
	e := map[string]any{
		"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
		"mode": "CLOUD", "id": ex.ID, "name": ex.Name,
		"expertTitle": ex.Name, "type": "", "expertType": expertType,
		"source": "builtin", "version": "", "cost": 0, "characterCount": 12,
		"conversationId": cid, "requestId": rid, "messageId": rid,
		"requestModelId": "deepseek-v4-flash", "requestModelName": "DeepSeek V4 Flash",
		"userId": uid,
	}
	for k, v := range base {
		if _, exists := e[k]; !exists {
			e[k] = v
		}
	}
	return e
}
