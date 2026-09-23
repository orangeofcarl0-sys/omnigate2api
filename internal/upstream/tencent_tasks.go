// 腾讯成长中心·任务面（SPEC §32.8）：任务列表/接单/领奖（双形态字段兼容）、
// 专家与技能市场（任务对象 id 来源）。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// GrowthExperts 拉取专家市场（pageSize 上限由调用方控制）。
func (c *TencentClient) GrowthExperts(acct *auth.Auth, pageSize int) ([]MarketExpert, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required")
	}
	raw, err := c.marketPost(acct, "/v2/operation-platform/market/expert/list",
		map[string]any{"page": 1, "pageSize": pageSize})
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
func expertUseEvent(uid string, ex MarketExpert, expertType string, base map[string]any) map[string]any {
	now := time.Now().UnixMilli()
	cid := fmt.Sprintf("wb-ex-%d", now)
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
