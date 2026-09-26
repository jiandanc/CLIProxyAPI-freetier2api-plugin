// Package tasks 实现 CodeBuddy 的成长任务与活动体系。
//
// 本包是纯逻辑层，依赖 cb 包完成出站（经宿主 HTTP 桥）。
//
// 整个体系的本质是**四套客户端指纹**：同一个 /v2/report 端点，
// 服务端按请求的指纹归属到不同的任务族：
//
//	CLI/billing 指纹  计费域 /v2/report    对话活跃上报（chat_5、连登、领养前置）
//	桌面指纹          对话域 /v2/report    桌面行为事件链（RichMeow、模板、专家…）
//	web 指纹          Web 域 /v2/report    资料库阅读等 web 侧行为
//	小程序指纹        计费域 /v2/report    开学季与小程序专属任务
//
// 三条普适规律（都是从真实失败中总结的）：
//   - /v2/report 的 body 恒为**数组**，不是单个对象；
//   - 上报 200 ≠ 计分，必须轮询回读（普通 4×3s、mp 2×3s、开学季 3×2.5s）；
//   - 事件必须带 userId，缺失时服务端 200 但静默丢弃。
package tasks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"workbuddy2api-plugin/internal/cb"
)

// 节流参数。全部照抄原项目实测值——这些数字是风控边界，
// 调快会触发上游限流，调慢只是浪费时间。
const (
	// reportGap 是连续上报之间的间隔。
	reportGap = 1050 * time.Millisecond
	// mpActionGap 是小程序任务写动作之间的间隔。
	mpActionGap = 2 * time.Second
	// expertSummonGap 是专家召唤链的间隔（实测 8s 成功率最高，取 6s 折中）。
	expertSummonGap = 6 * time.Second
	// nightChatGap 是夜间对话之间的间隔。
	nightChatGap = 4 * time.Second
	// acceptBatchGap 是批量接受任务的批间间隔。
	acceptBatchGap = 1050 * time.Millisecond
	// acceptBatchSize 是批量接受的每批数量。
	acceptBatchSize = 20
)

// 异步计分的轮询参数。
var (
	// claimPollAttempts 与 claimPollGap 决定普通任务的等待预算（约 12 秒）。
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
	// mpPollAttempts / mpPollGap 是小程序任务的等待预算（约 6 秒）。
	mpPollAttempts = 2
	mpPollGap      = 3 * time.Second
	// schoolPollLoops / schoolPollGap 是开学季的等待预算（约 7.5 秒）。
	schoolPollLoops = 3
	schoolPollGap   = 2500 * time.Millisecond
)

// fingerprint 生成某个账号某用途的稳定设备标识。
//
// 36 位 hex（18 字节），与官方客户端的设备 id 长度一致。
// 跨重启稳定、账号间互异、部署间隔离（混入机器盐，见 cb.SetInstallSalt）。
func fingerprint(credential *cb.Credential, purpose string) string {
	uid := credential.UIDValue()
	sum := sha256.Sum256([]byte("wbtask:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

// randomHex 生成 n 字节的随机 hex 串。
func randomHex(n int) string {
	buffer := make([]byte, n)
	if _, errRand := rand.Read(buffer); errRand != nil {
		// 极端情况下的退避：用时间戳派生，仍是唯一值。
		sum := sha256.Sum256([]byte(fmt.Sprintf("fallback-%d", time.Now().UnixNano())))
		return hex.EncodeToString(sum[:n])
	}
	return hex.EncodeToString(buffer)
}

// nowUnixMs 返回当前毫秒时间戳。
func nowUnixMs() int64 { return time.Now().UnixMilli() }

// desktopFingerprint 是桌面指纹族注入每个事件的公共字段。
//
// 这些字段共同把请求标识成「来自一台固定的 Windows 桌面客户端」。
// 业务字段可以覆盖它们（map 合并时业务优先）。
func desktopFingerprint(credential *cb.Credential) map[string]any {
	now := nowUnixMs()
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       credential.UIDValue(),
		"username":     credential.NicknameValue(),
		"userNickname": credential.NicknameValue(),
		"product":      "SaaS",
		// releaseDate 与 commit 是官方桌面版的构建信息，固定值即可。
		"releaseDate": int64(1789036585355),
		"commit":      "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":     "WorkBuddy",
		"ideType":     "WorkBuddy",
		"ideVersion":  desktopClientVersion,
		"machineId":   fingerprint(credential, "machine"),
		"sessionId":   fingerprint(credential, "session"),
		"extName":     "workbuddy-desktop",
		"extVersion":  desktopClientVersion,
		"os":          "win32",
		"arch":        "x64",
		"osVersion":   "10.0.26220",
		"cpuCores":    20,
		"memorySize":  24,
		"timestamp":   now,
		"presentAt":   now,
	}
}

// desktopClientVersion 是桌面指纹族的版本号（与 CLI 族刻意不同）。
const desktopClientVersion = "5.5.6"

// mergeEvent 把业务字段合并进指纹基底（业务字段优先）。
func mergeEvent(base map[string]any, event map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(event))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range event {
		merged[key] = value
	}
	return merged
}

// DesktopChatSequence 构造 6 事件对话链（多数任务的骨架）。
//
// 其中 chat_message_response 必须带 isSuccessful:true —— 服务端对事件链有
// 真实性校验，没有成功回执的链不会被计分。
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []map[string]any {
	now := nowUnixMs()
	traceID := requestID
	assistantMessageID := messageID + "-assistant"
	return []map[string]any{
		{
			"eventCode": "agent_task_created",
			"source":    "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		},
		{
			"eventCode": "chat_message_send",
			"messageId": assistantMessageID, "historyCount": 0, "isContextTruncated": false,
			"currentStepCount": 1, "traceId": traceID, "rootRequestId": requestID,
			"parentConversationId": conversationID, "agentName": "cli", "agentType": "main",
		},
		{
			"eventCode":   "chat_request_send",
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false,
			"maxToken": 0, "maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": "", "knowledgeName": "",
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"expertId": "", "recommendId": "", "skillId": "", "skillCount": 0,
			"totalCount": 0, "fileUri": "", "presentAt": now,
			"traceId": traceID, "rootRequestId": requestID, "parentConversationId": conversationID,
			"conversationId": conversationID, "messageId": messageID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		},
		{
			"eventCode": "chat_message_response",
			"messageId": assistantMessageID, "responseModelId": modelID, "responseModelName": modelName,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": now, "traceId": traceID, "conversationId": conversationID,
			"parentConversationId": conversationID, "presentAt": now,
		},
		{
			"eventCode": "chat_message_status",
			"messageId": assistantMessageID, "messageErrorCode": "0",
			"traceId": traceID, "conversationId": conversationID, "presentAt": now,
		},
		{
			"eventCode": "chat_request_response",
			"mode":      "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"isSuccessful": true, "finishReason": "stop",
			"conversationId": conversationID, "requestId": requestID,
			"traceId": traceID, "parentConversationId": conversationID, "presentAt": now,
		},
	}
}

// DesktopBuddyAppSequence 构造「进入 Buddy 应用」事件链（5 事件）。
//
// 同一组事件同时满足 Buddy_App 与 Buddy_App_QQ 两个任务：
// 后者只要求「进入企鹅教师助手」，而本链的事件里带了该应用的标识。
func DesktopBuddyAppSequence(buddyID, buddyName string) []map[string]any {
	base := map[string]any{"mode": "LOCAL", "buddyId": buddyID, "buddyName": buddyName, "position": 2}
	return []map[string]any{
		mergeEvent(base, map[string]any{"eventCode": "buddyapp_discover_click"}),
		mergeEvent(base, map[string]any{"eventCode": "buddyapp_show", "elementId": buddyID, "elementName": buddyName}),
		mergeEvent(base, map[string]any{"eventCode": "buddyapp_enter_click", "elementId": buddyID, "elementName": buddyName, "isFirstPage": "1"}),
		mergeEvent(base, map[string]any{"eventCode": "buddyapp_auth_confirm_click"}),
		mergeEvent(base, map[string]any{"eventCode": "buddyapp_bindaccount_skip_click"}),
	}
}

// DefaultBuddyID / DefaultBuddyName 是企鹅教师助手应用（Buddy_App_QQ 要求的目标）。
const (
	DefaultBuddyID   = "cb_y5Dy46tPQGGWtueMxXbe"
	DefaultBuddyName = "企鹅教师助手"
)

// DesktopAutomationCreateEvent 构造「定时任务创建成功」事件（automation_1）。
func DesktopAutomationCreateEvent(name string) []map[string]any {
	return []map[string]any{{
		"eventCode": "automated_task_create_suc", "name": name, "source": "manually",
		"modelId": "fast-model", "modelIsThinking": true, "connectorCount": 0,
		"skills": "", "skillCount": 0, "scheduleType": "once", "mode": "LOCAL",
	}}
}

// DesktopTemplateUseSequence 构造模板使用链（template_5）。
//
// = 对话链 + 两个模板事件。template_id 服务端不校验真实性，因此可用占位 id。
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []map[string]any {
	events := DesktopChatSequence(conversationID, requestID, randomHex(16), "fast-model", "fast-model")
	events = append(events,
		map[string]any{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		map[string]any{
			"eventCode": "template_used", "template_id": templateID, "task_mode": "working",
		},
	)
	return events
}

// DesktopPlaybookPromptSequence 构造灵感案例链（playbook_prompt）。
//
// 判据是 playbook_prompt_send（真正的发送动作），不是卡片点击或曝光。
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []map[string]any {
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	events := DesktopChatSequence(conversationID, requestID, randomHex(16), "fast-model", "fast-model")
	events = append(events,
		mergeEvent(payload, map[string]any{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		}),
		mergeEvent(payload, map[string]any{
			"eventCode": "playbook_cta_click", "source": "discover", "position": 0,
		}),
		mergeEvent(payload, map[string]any{
			"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID,
		}),
	)
	return events
}

// DesktopDesignCanvasSequence 构造设计画布链（create_canvas）。
func DesktopDesignCanvasSequence(conversationID, requestID string) []map[string]any {
	events := DesktopChatSequence(conversationID, requestID, randomHex(16), "fast-model", "fast-model")
	suffix := requestID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	events = append(events,
		map[string]any{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		map[string]any{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + suffix,
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
	return events
}

// SchoolChatTimesEvents 构造小程序对话事件（开学季与 mp 任务共用）。
//
// activityID 非空时带上（开学季任务要求它，缺失则不点亮）。
func SchoolChatTimesEvents(conversationID, activityID string) []map[string]any {
	requestID := "wb2api-" + randomHex(8)
	messageID := "msg-" + requestID
	if len(requestID) > 8 {
		messageID = "msg-" + requestID[len(requestID)-8:]
	}
	event := map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false,
		"maxToken": 0, "maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": "", "knowledgeName": "",
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"expertId": "", "recommendId": "", "skillId": "", "skillCount": 0,
		"totalCount": 0, "fileUri": "", "presentAt": nowUnixMs(),
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": messageID, "agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
	}
	if activityID != "" {
		event["activityId"] = activityID
	}
	return []map[string]any{event}
}

// MiniChatModelEvent 构造带指定模型的小程序对话事件。
func MiniChatModelEvent(conversationID, modelID, modelName string) []map[string]any {
	events := SchoolChatTimesEvents(conversationID, "")
	if len(events) == 0 {
		return events
	}
	events[0]["requestModelId"] = modelID
	events[0]["requestModelName"] = modelName
	return events
}

// MiniExpertUseEvent 构造小程序专家使用事件（Sequential_Tasks_2）。
//
// 注意：**不带 conversationId、不带 activityId** —— 真实事件就是这两个字段都没有。
// expertID 必须是市场里的真实 ex_ id，空 id 服务端不入账。
func MiniExpertUseEvent(expertID, expertName, expertType string) []map[string]any {
	return []map[string]any{{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": "2.2.8",
		"source":     "mini_program",
		"id":         expertID, "name": expertID, "expertTitle": expertName,
		"type": "send_message", "characterCount": 12, "expertType": expertType,
	}}
}

// SchoolSeasonActivityID 是开学季活动的活动标识（缺它不点亮校园日任务）。
const SchoolSeasonActivityID = "school_open_day_2026"

// 桌面指纹族请求上下文。
type desktopContext struct {
	credential *cb.Credential
	events     []map[string]any
}

// run 让事件带上指纹并发出去。
func (c *desktopContext) run(ctx context.Context, client *cb.Client) error {
	base := desktopFingerprint(c.credential)
	merged := make([]map[string]any, 0, len(c.events))
	for _, event := range c.events {
		merged = append(merged, mergeEvent(base, event))
	}
	return client.ReportDesktopEvents(ctx, c.credential, merged)
}

// ApplyDesktopFingerprint 给事件链注入桌面指纹（导出给 main 包编排使用）。
func ApplyDesktopFingerprint(credential *cb.Credential, events []map[string]any) []map[string]any {
	return withFingerprint(credential, events)
}

// DesktopExpertSummonSequence 构造专家召唤链（3 事件）。
//
// 关键字段：id 必须是市场真实 ex_ id，type 取专家的首个分类。
func DesktopExpertSummonSequence(expert cb.MarketExpert) []map[string]any {
	category := expert.Category
	if category == "" {
		category = "expert-all"
	}
	version := expert.Version
	if version == "" {
		version = "1.0.0"
	}
	pageURL := "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html"
	return []map[string]any{
		{
			"eventCode": "web_element_click", "source": expert.ID, "type": category, "version": version,
			"elementId": "expert_summon_click", "elementName": "立即召唤", "pageURL": pageURL,
		},
		{
			"eventCode": "expert_summon_click", "id": expert.ID, "name": expert.Name,
			"expertTitle": expert.Profession, "type": "expert-all", "position": 0,
			"expertType": expert.Type, "version": version, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": expert.ID, "name": expert.Name,
			"expertTitle": expert.Profession, "type": "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent 构造专家实际使用事件。
//
// requestId 必须是**真实对话的服务端 id**：自造 UUID 不会被计分。
func DesktopExpertActualUseEvent(expert cb.MarketExpert, conversationID, requestID string) []map[string]any {
	category := expert.Category
	if category == "" {
		category = "expert-all"
	}
	version := expert.Version
	if version == "" {
		version = "1.0.0"
	}
	return []map[string]any{{
		"eventCode": "expert_actual_use", "id": expert.ID, "name": expert.Name,
		"expertTitle": expert.Profession, "type": category, "expertType": expert.Type,
		"source": "builtin", "version": version,
		"cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID,
		"messageId":      "msg-" + tailOfString(requestID, 8),
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"mode": "craft",
	}}
}

// tailOfString 取字符串末尾 n 个字符。
func tailOfString(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}
