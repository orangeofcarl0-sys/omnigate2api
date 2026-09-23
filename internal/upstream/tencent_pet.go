// 腾讯成长中心·宠物面（SPEC §32.2/§32.7）：探险状态机四端点、盲盒能量额度、
// 免费领养链路（协议→buddy/first）与宠物列表。
package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"omnigate2api/internal/auth"
)

// PetTravel 宠物探险状态（travel/status 响应 data）。
type PetTravel struct {
	State             string      `json:"state"` // idle | traveling | arrived
	DailyLimitReached bool        `json:"daily_limit_reached"`
	ArriveAt          int64       `json:"arrive_at"`
	ServerNow         int64       `json:"server_now"`
	RewardCredit      int64       `json:"reward_credit"`
	RecordID          json.Number `json:"record_id"` // 归来领取凭据（claim body）
	Location          struct {
		Name string `json:"name"`
	} `json:"location"`
}

// PetLocation 探险地点（travel/config locations 元素）。id 真实形态为数字
// （活测实证 2026-09-06），json.Number 保类型透传进 depart body。
type PetLocation struct {
	ID               json.Number `json:"id"`
	Name             string      `json:"name"`
	DurationHoursMin int         `json:"duration_hours_min"`
	DurationHoursMax int         `json:"duration_hours_max"`
}

// ErrPetNoUnclaimed 宠物无未领取奖励（幂等：已领过）。
var ErrPetNoUnclaimed = errors.New("pet: no unclaimed reward")

// ---------------------------------------------------------------------------
// 成长中心宠物探险（SPEC §32.2 拍板：四端点状态机，只走 API 直连）
// ---------------------------------------------------------------------------

// petRequest 活动域请求（统一机制·SPEC §32）。
func (c *TencentClient) petRequest(acct *auth.Auth, method, path string, body []byte) ([]byte, int, error) {
	return c.activityDo(acct, method, path, body)
}

// PetTravelStatus 查询宠物探险状态。
func (c *TencentClient) PetTravelStatus(acct *auth.Auth) (*PetTravel, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for pet status")
	}
	raw, status, err := c.petRequest(acct, http.MethodGet, "/activity/growth/buddy/travel/status", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int64     `json:"code"`
		Msg  string    `json:"msg"`
		Data PetTravel `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("pet status parse: %w", uerr)
	}
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("pet status failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	return &env.Data, nil
}

// PetTravelConfig 探险可选地点（首个为默认目的地）。
func (c *TencentClient) PetTravelConfig(acct *auth.Auth) ([]PetLocation, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for pet config")
	}
	raw, status, err := c.petRequest(acct, http.MethodGet, "/activity/growth/buddy/travel/config", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Locations []PetLocation `json:"locations"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("pet config parse: %w", uerr)
	}
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("pet config failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	return env.Data.Locations, nil
}

// PetDepart 派出宠物前往地点（locationID 为 config 原生 id，数字/字符串保类型）。
func (c *TencentClient) PetDepart(acct *auth.Auth, locationID json.Number) error {
	if acct == nil {
		return fmt.Errorf("account required for pet depart")
	}
	body, _ := json.Marshal(map[string]any{"location_id": locationID})
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/travel/depart", body)
	if err != nil {
		return err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &env)
	if status >= 400 || env.Code != 0 {
		return fmt.Errorf("pet depart failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	return nil
}

// PetClaim 领取归来奖励（recordID 非空时按 2026-09 契约带 record_id）；
// 已领/无未领（400 no unclaimed）→ ErrPetNoUnclaimed（幂等）。
func (c *TencentClient) PetClaim(acct *auth.Auth, recordID string) (int64, error) {
	if acct == nil {
		return 0, fmt.Errorf("account required for pet claim")
	}
	body := []byte("{}")
	if recordID != "" && recordID != "0" {
		body, _ = json.Marshal(map[string]any{"record_id": json.RawMessage(recordID)})
	}
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/travel/claim", body)
	if err != nil {
		return 0, err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit int64 `json:"credit"`
			Reward int64 `json:"reward_credit"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Code == 400 && strings.Contains(strings.ToLower(env.Msg), "no unclaimed") {
		return 0, ErrPetNoUnclaimed
	}
	if status >= 400 || env.Code != 0 {
		return 0, fmt.Errorf("pet claim failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	credit := env.Data.Credit
	if credit == 0 {
		credit = env.Data.Reward
	}
	return credit, nil
}

// PetQuota 宠物盲盒能量额度（能量为开盒唯一出口）。Balance 为能量余额
// （全球版实测存在 2026-09-23）。
type PetQuota struct {
	Affordable   int `json:"affordable"`
	MaxOpenCount int `json:"max_open_count"`
	CostPerOpen  int `json:"cost_per_open"`
	Balance      int `json:"balance"`
}

// PetQuota 查询宠物盲盒能量额度。
func (c *TencentClient) PetQuota(acct *auth.Auth) (*PetQuota, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for pet quota")
	}
	raw, status, err := c.petRequest(acct, http.MethodGet, "/activity/growth/buddy/quota", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Code  int64    `json:"code"`
		Msg   string   `json:"msg"`
		Quota PetQuota `json:"-"`
		Data  PetQuota `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("pet quota parse: %w", uerr)
	}
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("pet quota failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	// 字段可能在顶层或 data 内（两版契约兼容，quota 优先 data）
	q := env.Data
	if q.Affordable == 0 && q.MaxOpenCount == 0 {
		var top struct {
			Affordable   int `json:"affordable"`
			MaxOpenCount int `json:"max_open_count"`
		}
		_ = json.Unmarshal(raw, &top)
		q.Affordable, q.MaxOpenCount = top.Affordable, top.MaxOpenCount
	}
	return &q, nil
}

// PetOpenBox 开宠物盲盒（能量足够时激活宠物；count 由调用方按 quota 决定）。
func (c *TencentClient) PetOpenBox(acct *auth.Auth, count int) error {
	if acct == nil {
		return fmt.Errorf("account required for pet open")
	}
	if count < 1 {
		count = 1
	}
	tok, _ := RandomHex(16)
	body, _ := json.Marshal(map[string]any{"count": count, "client_token": tok})
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/open", body)
	if err != nil {
		return err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &env)
	if status >= 400 || env.Code != 0 {
		return fmt.Errorf("pet open failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	return nil
}

// BuddyInstance 已领养宠物（/activity/growth/buddy/list）。
type BuddyInstance struct {
	InstanceID json.Number `json:"instance_id"`
	Name       string      `json:"name"`
	Current    bool        `json:"current_buddy"`
}

// PetBuddies 已领养宠物列表（buddy5 事件链的 buddyId/buddyName 来源）。
func (c *TencentClient) PetBuddies(acct *auth.Auth) ([]BuddyInstance, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required")
	}
	raw, status, err := c.petRequest(acct, http.MethodGet, "/activity/growth/buddy/list", nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Buddies []BuddyInstance `json:"buddies"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		return nil, fmt.Errorf("buddies parse: %w", uerr)
	}
	if status >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("buddies failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 160))
	}
	return env.Data.Buddies, nil
}

// buddy5Events 五连「进入 Buddy 应用」事件（点亮 Buddy_App/_QQ）。
func buddy5Events(uid string, buddyID, buddyName string, base map[string]any) []any {
	mk := func(code string, extra map[string]any) map[string]any {
		e := map[string]any{"eventCode": code, "mode": "LOCAL", "buddyId": buddyID, "buddyName": buddyName}
		for k, v := range extra {
			e[k] = v
		}
		for k, v := range base {
			if _, exists := e[k]; !exists {
				e[k] = v
			}
		}
		return e
	}
	return []any{
		mk("buddyapp_discover_click", map[string]any{}),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// PetAdopt 领养首只宠物：先同意领养协议，再领养；成功直接发放 credit+energy。
// 幂等：已领养时两步接口均返回既有态（不视为错误）。
func (c *TencentClient) PetAdopt(acct *auth.Auth) (int64, int64, error) {
	if acct == nil {
		return 0, 0, fmt.Errorf("account required for pet adopt")
	}
	agree, _ := json.Marshal(map[string]any{"agree": true})
	if _, st, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/agreement", agree); err != nil {
		return 0, 0, fmt.Errorf("buddy agreement: %w", err)
	} else if st >= 400 {
		return 0, 0, fmt.Errorf("buddy agreement http=%d", st)
	}
	raw, status, err := c.petRequest(acct, http.MethodPost, "/activity/growth/buddy/first", []byte("{}"))
	if err != nil {
		return 0, 0, err
	}
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit       int64 `json:"credit"`
			Energy       int64 `json:"energy"`
			RewardCredit int64 `json:"reward_credit"`
			RewardEnergy int64 `json:"reward_energy"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	if status >= 400 || env.Code != 0 {
		return 0, 0, fmt.Errorf("buddy first failed http=%d code=%d msg=%s", status, env.Code, truncateStr(env.Msg, 200))
	}
	credit, energy := env.Data.Credit, env.Data.Energy
	if credit == 0 {
		credit = env.Data.RewardCredit
	}
	if energy == 0 {
		energy = env.Data.RewardEnergy
	}
	return credit, energy, nil
}
