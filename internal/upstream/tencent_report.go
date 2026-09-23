// 腾讯埋点上报面（SPEC §32.8 逆向）：桌面指纹与设备标识派生、桌面六连对话事件链、
// 任务事件包——任务完成判定的唯一途径（chat 域 /v2/report，桌面 UA 门控）。
package upstream

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// ReportTaskEvents 上报任务事件包（SPEC §32.8 逆向逐个任务的已验证形状）：
// RichMeow 六连 + chat_5(×5) + Model_chat_GLM5.2 + create_canvas + automation_1
// （+ 夜猫窗口内 black_cat×3）。发往 chat 域 /v2/report（桌面 UA + 指纹）。
// 说明：事件只做「完成任务」的埋点，奖励由任务领取链路自动入账。
func (c *TencentClient) ReportTaskEvents(acct *auth.Auth) error {
	if acct == nil {
		return fmt.Errorf("account required for task events")
	}
	if err := c.ReportDesktopChat(acct); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	uid := acct.UserID
	var evs []any

	chatEv := func(idx int, modelID, modelName, mode string) map[string]any {
		cid := fmt.Sprintf("wb-chat-%d-%d", now, idx)
		return map[string]any{
			"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
			"mode": mode, "conversationId": cid, "requestId": cid,
			"inputLength": 12, "requestModelId": modelID, "requestModelName": modelName,
			"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
			"codebaseEnable": false, "maxToken": 0, "maxSteps": 0, "temperature": 0,
			"maxRetries": 0, "mentionContexts": []any{}, "knowledgeId": []any{},
			"knowledgeName": []any{}, "codebaseId": "", "mentionContextCount": 0,
			"command": "", "expertId": "", "recommendId": "", "skillId": "",
			"skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
			"traceId": "", "rootRequestId": cid, "parentConversationId": cid,
			"agentName": "default", "agentType": "conversation", "userId": uid,
		}
	}
	// chat_5：5 条独立会话
	for i := 0; i < 5; i++ {
		evs = append(evs, chatEv(i, "deepseek-v4-flash", "DeepSeek V4 Flash", "craft"))
	}
	// Model_chat_GLM5.2
	evs = append(evs, chatEv(90, "glm-5.2", "GLM-5.2", "craft"))
	// black_cat（夜猫 23:00-08:00 CST 窗口内 3 次）
	if h := time.Now().In(cstZone).Hour(); h >= 23 || h < 8 {
		for i := 0; i < 3; i++ {
			evs = append(evs, chatEv(100+i, "glm-5.2", "GLM-5.2", "night"))
		}
	}
	// create_canvas（自造画布 id）
	runev := fmt.Sprintf("wb-run-%d", now)
	evs = append(evs, map[string]any{
		"eventCode": "wbx_design_canvas_task_create", "timestamp": now,
		"reportDelay": 0, "conversationId": runev, "requestId": runev,
		"source": "summon_keyword", "isCustomModel": false, "name": "",
		"inputLength": 12, "id": fmt.Sprintf("wbx-canvas-%d", now),
		"cost": 0, "isSuccessful": true, "userId": uid,
	})
	// automation_1
	evs = append(evs, map[string]any{
		"eventCode": "automated_task_create_suc", "timestamp": now, "reportDelay": 0,
		"name": "每周五自动生成周报", "source": "manually",
		"modelId": "deepseek-v4-flash", "modelIsThinking": false,
		"expertId": "", "expertMarketplace": "", "connectorIds": "",
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "recurring", "pushToWeChat": false, "pushToWecomBot": false,
		"conversationId": runev, "requestId": runev,
		"schedule": map[string]any{"type": "recurring", "rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"},
		"prompt":   "每周五自动整理本周工作，生成一份周报。", "userId": uid,
	})
	// 市场对象类任务（需要真实对象 id；获取失败不阻断——只记入错误返回）
	var extras []any
	if experts, xerr := c.GrowthExperts(acct, 50); xerr == nil {
		// expert_5：5 位专家（agent 类型）
		n := 0
		for _, ex := range experts {
			if n >= 5 {
				break
			}
			if ex.ID == "" {
				continue
			}
			extras = append(extras, expertUseEvent(uid, ex, "agent", nil))
			n++
		}
		// Expert_lighthouse：名称含「轻量云」的专家
		for _, ex := range experts {
			if strings.Contains(ex.Name, "轻量云") {
				extras = append(extras, expertUseEvent(uid, ex, "agent", nil))
				break
			}
		}
		// Expert_team_use_3：team 类型 ×3
		n = 0
		for _, ex := range experts {
			if n >= 3 {
				break
			}
			if ex.Type == "team" || strings.Contains(ex.Name, "专家团") {
				extras = append(extras, expertUseEvent(uid, ex, "team", nil))
				n++
			}
		}
	}
	if skills, serr := c.GrowthSkills(acct, 3); serr == nil {
		for _, sk := range skills {
			if sk.ID == "" {
				continue
			}
			extras = append(extras, map[string]any{
				"eventCode": "skill_info", "timestamp": now, "reportDelay": 0,
				"skillId": sk.ID, "skillName": sk.Name, "skillVersion": sk.Version,
				"action": "use", "conversationId": runev, "requestId": runev, "userId": uid,
			})
			break // skill_1：一个即可
		}
	}
	if buddies, berr := c.PetBuddies(acct); berr == nil {
		for _, b := range buddies {
			if b.Current || len(buddies) == 1 {
				bid := b.InstanceID.String()
				extras = append(extras, buddy5Events(uid, bid, b.Name, nil)...)
				extras = append(extras, buddy5Events(uid, bid, b.Name, nil)...) // Buddy_App + Buddy_App_QQ
				break
			}
		}
	}
	evs = append(evs, extras...)
	// 注入桌面指纹（不覆盖事件自有键）
	fp := desktopFingerprint(acct, now)
	for i := range evs {
		if e, ok := evs[i].(map[string]any); ok {
			for k, v := range fp {
				if _, exists := e[k]; !exists {
					e[k] = v
				}
			}
		}
	}
	return c.postReport(acct, evs)
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
