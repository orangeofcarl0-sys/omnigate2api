// 腾讯埋点上报面（SPEC §32.8 逆向）：桌面指纹与设备标识派生、桌面六连对话事件链、
// 任务事件包——任务完成判定的唯一途径（chat 域 /v2/report，桌面 UA 门控）。
package upstream

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"omnigate2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 活跃上报与宠物领养（SPEC §32.7；社区实证：report 前置 → agreement → buddy/first）
// ---------------------------------------------------------------------------

// cstZone 北京时间（任务窗口判定用）。
var cstZone = time.FixedZone("CST", 8*3600)

// deriveDeviceID 由 uid+盐稳定派生设备标识（md5 hex 32 位，SPEC §32.8 逆向：
// 对齐 community deriveID，同一账号恒定——模拟固定设备，勿每次随机；
// 仅用于埋点指纹注入，不参与业务逻辑）。
func deriveDeviceID(uid, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:])
}

// desktopFingerprint 桌面端埋点公共指纹（SPEC §32.8 逆向：逐字段对齐官方
// 桌面客户端；userId 为账号 uid，缺失则服务端静默丢弃）。
func desktopFingerprint(acct *auth.Auth, now int64) map[string]any {
	nick := acct.UserName
	return map[string]any{
		"timezone": "Asia/Shanghai", "reportDelay": 2000,
		"userId": acct.UserID, "username": nick, "userNickname": nick,
		"product": "SaaS", "releaseDate": 1789036585355,
		"commit":  "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName": "WorkBuddy", "ideType": "WorkBuddy", "ideVersion": "5.5.6",
		"machineId": deriveDeviceID(acct.UserID, "machine"),
		"sessionId": deriveDeviceID(acct.UserID, "session"),
		"extName":   "workbuddy-desktop", "extVersion": "5.5.6",
		"os": "win32", "arch": "x64", "osVersion": "10.0.26220",
		"cpuCores": 20, "memorySize": 24,
		"timestamp": now, "presentAt": now,
	}
}

// ReportDesktopChat 上报「桌面端成功对话」六连事件链（SPEC §32.8 逆向落地）：
// agent_task_created → chat_message_send → chat_request_send → chat_message_response
// → chat_message_status → chat_request_response，逐条注入桌面指纹；发往 chat 域
// /v2/report。用于解锁 first_buddy（宠物领养前置）。
func (c *TencentClient) ReportDesktopChat(acct *auth.Auth) error {
	if acct == nil {
		return fmt.Errorf("account required for report")
	}
	now := time.Now().UnixMilli()
	cid := fmt.Sprintf("wb-%d", now)
	rid, mid := cid, cid
	fp := func() map[string]any {
		f := desktopFingerprint(acct, now)
		f["conversationId"] = cid
		f["requestId"] = rid
		return f
	}
	ev := func(code string, extra map[string]any) map[string]any {
		e := map[string]any{"eventCode": code, "timestamp": now, "mode": "craft"}
		for k, v := range extra {
			e[k] = v
		}
		for k, v := range fp() {
			if _, exists := e[k]; !exists {
				e[k] = v
			}
		}
		return e
	}
	events := []any{
		ev("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local",
			"requestModelId": "glm-5.2", "requestModelName": "GLM-5.2",
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": cid, "messageId": mid, "buddyId": "", "buddyName": "",
		}),
		ev("chat_message_send", map[string]any{
			"messageId": mid + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": rid, "rootRequestId": rid, "parentConversationId": cid,
			"agentName": "cli", "agentType": "main",
		}),
		ev("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": rid, "rootRequestId": rid, "parentConversationId": cid,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id": cid, "codebuddy.conversation_request_id": rid,
		}),
		ev("chat_message_response", map[string]any{
			"messageId": mid + "-assistant", "responseModelId": "glm-5.2",
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": now, "traceId": rid, "conversationId": cid,
			"rootRequestId": rid, "parentConversationId": cid,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id": cid, "codebuddy.conversation_request_id": rid,
		}),
		ev("chat_message_status", map[string]any{
			"messageId": mid + "-assistant", "messageErrorCode": "0",
			"traceId": rid, "rootRequestId": rid, "parentConversationId": cid,
			"agentName": "cli", "agentType": "main",
		}),
		ev("chat_request_response", map[string]any{
			"toolCallCount": 0,
			"inputToken":    120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": rid, "parentConversationId": cid,
		}),
	}
	return c.postReport(acct, events)
}

// ---------------------------------------------------------------------------
// 任务事件表（SPEC §32.8：新增任务 = 加一行；对象 id 来源与事件形状见各 build）
// ---------------------------------------------------------------------------

// taskEventCtx 事件包上下文（一次上报的共享输入：账号/时间/市场对象）。
type taskEventCtx struct {
	acct       *auth.Auth
	now        int64
	uid        string
	cid        string // run 级会话 id（canvas/automation/skill 复用）
	experts    []MarketExpert
	skills     []MarketSkill
	buddies    []BuddyInstance
	cases      []PlaybookCase    // playbook_prompt
	lighthouse []MarketExpert    // Expert_lighthouse（关键词检索结果）
	scenes     []Scene           // template_5
	themes     []AppearanceTheme // Hp_Appearance
}

// buddyAppID / buddyAppName buddy5 事件承载应用（社区实测：企鹅教师助手，
// 同一组事件同时满足 Buddy_App 与 Buddy_App_QQ）。
const (
	buddyAppID   = "cb_y5Dy46tPQGGWtueMxXbe"
	buddyAppName = "企鹅教师助手"
)

// chatEvent 构造一条 chat_request_send（chat_5 / Model_chat_GLM5.2 / black_cat 共用）。
func (x *taskEventCtx) chatEvent(idx int, modelID, modelName, mode string) map[string]any {
	cid := fmt.Sprintf("wb-chat-%d-%d", x.now, idx)
	return map[string]any{
		"eventCode": "chat_request_send", "timestamp": x.now, "reportDelay": 0,
		"mode": mode, "conversationId": cid, "requestId": cid,
		"inputLength": 12, "requestModelId": modelID, "requestModelName": modelName,
		"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
		"codebaseEnable": false, "maxToken": 0, "maxSteps": 0, "temperature": 0,
		"maxRetries": 0, "mentionContexts": []any{}, "knowledgeId": []any{},
		"knowledgeName": []any{}, "codebaseId": "", "mentionContextCount": 0,
		"command": "", "expertId": "", "recommendId": "", "skillId": "",
		"skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": x.now,
		"traceId": "", "rootRequestId": cid, "parentConversationId": cid,
		"agentName": "default", "agentType": "conversation", "userId": x.uid,
	}
}

// taskEventSpec 一类任务的埋点事件构造（表驱动：code 与服务端任务清单/社区 MAPPING 对照）。
type taskEventSpec struct {
	code  string
	build func(*taskEventCtx) []any
}

// taskEventSpecs 任务事件表。已实证点亮：chat_5 / Model_chat_GLM5.2 / create_canvas /
// automation_1 / skill_1 / Buddy_App / RichMeow（六连，ReportDesktopChat）。
// 待补（需各自对象来源）：template_5(Hp/scenes)、Library_read(web 域)、Hp_Appearance(主题
// resourceKey)、playbook_prompt(灵感案例)、Expert_Philanthropy(真实捐款，不可伪造)。
var taskEventSpecs = []taskEventSpec{
	{"chat_5", func(x *taskEventCtx) []any {
		var out []any
		for i := 0; i < 5; i++ {
			out = append(out, x.chatEvent(i, "deepseek-v4-flash", "DeepSeek V4 Flash", "craft"))
		}
		return out
	}},
	{"Model_chat_GLM5.2", func(x *taskEventCtx) []any {
		return []any{x.chatEvent(90, "glm-5.2", "GLM-5.2", "craft")}
	}},
	{"black_cat", func(x *taskEventCtx) []any {
		// 夜猫窗口 23:00-08:00（CST）内 3 次
		if h := time.Now().In(cstZone).Hour(); h < 23 && h >= 8 {
			return nil
		}
		var out []any
		for i := 0; i < 3; i++ {
			out = append(out, x.chatEvent(100+i, "glm-5.2", "GLM-5.2", "night"))
		}
		return out
	}},
	{"create_canvas", func(x *taskEventCtx) []any {
		// 自造画布 id（服务端不校验归属）
		return []any{map[string]any{
			"eventCode": "wbx_design_canvas_task_create", "timestamp": x.now,
			"reportDelay": 0, "conversationId": x.cid, "requestId": x.cid,
			"source": "summon_keyword", "isCustomModel": false, "name": "",
			"inputLength": 12, "id": fmt.Sprintf("wbx-canvas-%d", x.now),
			"cost": 0, "isSuccessful": true, "userId": x.uid,
		}}
	}},
	{"automation_1", func(x *taskEventCtx) []any {
		return []any{map[string]any{
			"eventCode": "automated_task_create_suc", "timestamp": x.now, "reportDelay": 0,
			"name": "每周五自动生成周报", "source": "manually",
			"modelId": "deepseek-v4-flash", "modelIsThinking": false,
			"expertId": "", "expertMarketplace": "", "connectorIds": "",
			"connectorCount": 0, "skills": "", "skillCount": 0,
			"scheduleType": "recurring", "pushToWeChat": false, "pushToWecomBot": false,
			"conversationId": x.cid, "requestId": x.cid,
			"schedule": map[string]any{"type": "recurring", "rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"},
			"prompt":   "每周五自动整理本周工作，生成一份周报。", "userId": x.uid,
		}}
	}},
	{"expert_5", func(x *taskEventCtx) []any {
		var out []any
		for i, ex := range x.experts {
			if len(out) >= 5 {
				break
			}
			out = append(out, expertUseEvent(x.uid, 200+i, ex, "agent", nil))
		}
		return out
	}},
	{"Expert_lighthouse", func(x *taskEventCtx) []any {
		// 关键词检索优先（lighthouse / 轻量云）；失败回落内置轻量云专家 id
		for _, kw := range []string{"lighthouse", "轻量云"} {
			for _, ex := range x.lighthouse {
				if strings.Contains(kw, "lighthouse") || strings.Contains(ex.Name, "轻量云") {
					return []any{expertUseEvent(x.uid, 300, ex, "agent", nil)}
				}
			}
		}
		if len(x.lighthouse) > 0 {
			return []any{expertUseEvent(x.uid, 300, x.lighthouse[0], "agent", nil)}
		}
		return []any{expertUseEvent(x.uid, 300, MarketExpert{ID: "ex_2cvvUZQhDyeJ", Name: "腾讯轻量云专家", Type: "agent"}, "agent", nil)}
	}},
	{"Expert_team_use_3", func(x *taskEventCtx) []any {
		var out []any
		for i, ex := range x.experts {
			if len(out) >= 3 {
				break
			}
			if ex.Type == "team" {
				out = append(out, expertUseEvent(x.uid, 400+i, ex, "team", nil))
			}
		}
		return out
	}},
	{"playbook_prompt", func(x *taskEventCtx) []any {
		if len(x.cases) == 0 {
			return nil
		}
		cs := x.cases[0]
		cid := fmt.Sprintf("wb-pb-%d", x.now)
		return []any{map[string]any{
			"eventCode": "playbook_prompt_send", "timestamp": x.now, "reportDelay": 0,
			"id": cs.ID, "name": cs.Title, "type": cs.Type, "promptLength": 0,
			"isOfficial": 1, "skills": "", "skillNames": "", "expertId": "", "expertName": "",
			"categoryId": "", "categoryName": "", "query": "", "source": "discover",
			"conversationId": cid, "requestId": cid, "ext1": "discover", "userId": x.uid,
		}}
	}},
	{"Library_read", func(x *taskEventCtx) []any {
		return nil // web 域事件由 ReportLibraryRead 单独上报（域不同）
	}},
	{"template_5", func(x *taskEventCtx) []any {
		// 场景 id 来自 /console/as/support/scenes（失败回落内置表）
		var out []any
		for i, sc := range x.scenes {
			if len(out) >= 5 {
				break
			}
			cid := fmt.Sprintf("wb-tpl-%d-%d", x.now, i)
			out = append(out, map[string]any{
				"eventCode": "agent_task_created_with_template", "timestamp": x.now,
				"reportDelay": 0, "isCustomModel": true, "id": sc.ID, "name": sc.Name,
				"requestId": cid, "conversationId": cid, "userId": x.uid,
			})
		}
		return out
	}},
	{"Hp_Appearance", func(x *taskEventCtx) []any {
		var out []any
		for _, th := range x.themes {
			out = append(out, map[string]any{
				"eventCode": "appearance_skin_apply", "timestamp": x.now, "reportDelay": 0,
				"action": "apply", "source": "settings_close", "id": th.ID,
				"vipLevel": th.VipLevel, "series": th.Series, "type": "unknown",
				"name": th.Name, "userId": x.uid,
			})
			break // 一个主题即可
		}
		return out
	}},
	{"skill_1", func(x *taskEventCtx) []any {
		for _, sk := range x.skills {
			if sk.ID == "" {
				continue
			}
			return []any{map[string]any{
				"eventCode": "skill_info", "timestamp": x.now, "reportDelay": 0,
				"skillId": sk.ID, "skillName": sk.Name, "skillVersion": sk.Version,
				"action": "use", "conversationId": x.cid, "requestId": x.cid, "userId": x.uid,
			}}
		}
		return nil
	}},
	{"Buddy_App", func(x *taskEventCtx) []any {
		return buddy5Events(x.uid, buddyAppID, buddyAppName, nil)
	}},
	{"Buddy_App_QQ", func(x *taskEventCtx) []any {
		// 同一组五连事件同时满足 Buddy_App 与 Buddy_App_QQ（社区两账号实测）
		return buddy5Events(x.uid, buddyAppID, buddyAppName, nil)
	}},
}

// currentBuddy 取当前宠物（buddy5 事件链的 buddyId/buddyName 来源）。
func (x *taskEventCtx) currentBuddy() (string, string, bool) {
	for _, b := range x.buddies {
		if b.Current || len(x.buddies) == 1 {
			return b.InstanceID.String(), b.Name, true
		}
	}
	return "", "", false
}

// ReportTaskEvents 上报任务事件包（SPEC §32.8 表驱动）：遍历 taskEventSpecs 生成各类任务
// 的埋点事件，市场对象按需拉取（失败仅跳过相关任务，不阻断其他）。事件只做「完成任务」
// 的埋点，奖励由任务领取链路自动入账。
func (c *TencentClient) ReportTaskEvents(acct *auth.Auth) error {
	if acct == nil {
		return fmt.Errorf("account required for task events")
	}
	if err := c.ReportDesktopChat(acct); err != nil {
		return err
	}
	x := &taskEventCtx{acct: acct, now: time.Now().UnixMilli(), uid: acct.UserID}
	x.cid = fmt.Sprintf("wb-run-%d", x.now)
	x.experts = c.GrowthExpertsPaged(acct, 3, "")
	x.cases = c.GrowthPlaybookCases()
	x.scenes = c.GrowthScenes(acct)
	x.themes = c.GrowthThemes(acct)
	for _, kw := range []string{"lighthouse", "轻量云"} {
		if got := c.GrowthExpertsPaged(acct, 2, kw); len(got) > 0 {
			x.lighthouse = append(x.lighthouse, got...)
		}
	}
	if skills, err := c.GrowthSkills(acct, 3); err == nil {
		x.skills = skills
	}
	if buddies, err := c.PetBuddies(acct); err == nil {
		x.buddies = buddies
	}
	var evs []any
	for _, spec := range taskEventSpecs {
		evs = append(evs, spec.build(x)...)
	}
	// 注入桌面指纹（不覆盖事件自有键）
	fp := desktopFingerprint(acct, x.now)
	for i := range evs {
		if e, ok := evs[i].(map[string]any); ok {
			for k, v := range fp {
				if _, exists := e[k]; !exists {
					e[k] = v
				}
			}
		}
	}
	if err := c.postReport(acct, evs); err != nil {
		return err
	}
	// Library_read 走 web 域（Origin/Referer/platform 头与 chat 域不同），单独上报；
	// 失败不阻断 chat 域事件（下一 Tick 自然重试）。
	if werr := c.ReportLibraryRead(acct); werr != nil {
		log.Printf("web report (Library_read) failed: %v", werr)
	}
	return nil
}

// webUserAgent 浏览器 UA（web 域埋点；Library_read 用）。
const webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// libraryDocURL 空间文档页（Library_read 的 pageURL）。
const libraryDocURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"

// webBase 资料库/成长中心 web 域（与 chat 域分离：Origin/Referer/X-Domain 需一致）。
const webBase = "https://www.workbuddy.cn"

// ReportLibraryRead 上报 web 域「资料库介绍点击」事件（SPEC §32.8：Library_read 走
// web 域，浏览器形状指纹 + x-client-platform: web；服务端按 web 域归因该任务）。
func (c *TencentClient) ReportLibraryRead(acct *auth.Auth) error {
	if acct == nil {
		return fmt.Errorf("account required for web report")
	}
	now := time.Now().UnixMilli()
	ev := map[string]any{
		"eventCode": "web_element_click", "timestamp": now, "reportDelay": 0,
		"pageURL": libraryDocURL, "elementId": "library_doc_intro_click",
		"elementName": "WorkBuddy资料库介绍",
		"os":          "Win32", "arch": "", "osVersion": "10.0", "userAgent": webUserAgent,
		"machineId":    deriveDeviceID(acct.UserID, "webmachine"),
		"userId":       acct.UserID,
		"userNickname": acct.UserName,
	}
	body, _ := json.Marshal([]any{ev})
	req, err := http.NewRequest(http.MethodPost, webBase+"/v2/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("Origin", webBase)
	req.Header.Set("Referer", libraryDocURL)
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("X-User-Id", acct.UserID)
	req.Header.Set("X-Domain", webBase)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("web report failed http=%d: %s", resp.StatusCode, truncateStr(string(raw), 160))
	}
	return nil
}

// PlaybookCase 灵感案例（playbook_prompt 的对象 id 来源）。
type PlaybookCase struct {
	ID    string
	Title string
	Type  string
}

// GrowthPlaybookCases 拉取灵感案例注册表（静态 CDN；失败回落内置表，社区同源）。
func (c *TencentClient) GrowthPlaybookCases() []PlaybookCase {
	fallback := []PlaybookCase{{"worker-ledger-freedom-dashboard", "打工人小账本", "other"}}
	req, err := http.NewRequest(http.MethodGet, "https://static.workbuddy.cn/workbuddy/playbook/registry.json", nil)
	if err != nil {
		return fallback
	}
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return fallback
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fallback
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var doc struct {
		Cases []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Type  string `json:"artifact_type"`
		} `json:"cases"`
	}
	if uerr := json.Unmarshal(raw, &doc); uerr != nil || len(doc.Cases) == 0 {
		return fallback
	}
	out := make([]PlaybookCase, 0, 4)
	for _, cs := range doc.Cases {
		if cs.ID != "" {
			out = append(out, PlaybookCase{cs.ID, cs.Title, cs.Type})
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// postReport 埋点上报（chat 域 + 桌面 UA 形态；统一机制·SPEC §32.8）。
func (c *TencentClient) postReport(acct *auth.Auth, events []any) error {
	body, _ := json.Marshal(events)
	raw, status, err := c.chatDo(acct, http.MethodPost, "/v2/report", body, true)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("report failed http=%d: %s", status, truncateStr(string(raw), 160))
	}
	return nil
}
