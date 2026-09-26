package main

// 本文件实现成长任务的动作表与执行。
//
// 动作表的**顺序即依赖序**：first_buddy 依赖前置的活跃上报，
// 专家类任务需要先拿到市场里的真实专家 id。执行时必须按表序推进。
//
// 成本分级（决定执行代价）：
//   - 纯事件上报：零额度消耗（多数任务）；
//   - 真实对话：消耗额度（Model_chat、专家类、技能尝鲜、夜猫子）。

import (
	"context"
	"fmt"
	"strings"

	"workbuddy2api-plugin/internal/cb"
	"workbuddy2api-plugin/internal/logger"
	"workbuddy2api-plugin/internal/tasks"
)

// taskAction 是一个可自动完成的成长任务。
type taskAction struct {
	// Code 是任务标识。
	Code string
	// Desc 是任务说明（供管理页展示）。
	Desc string
	// Run 执行任务动作，返回可读的执行说明。
	Run func(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error)
	// UsesChat 标记该动作是否消耗真实对话额度。
	UsesChat bool
}

// autoActions 是「一键完成」覆盖的任务表（顺序即执行顺序）。
//
// 与用户口径一致：17 个可自动完成的任务，唯一不做的 Expert_Philanthropy
// 需要真实捐款（服务端校验捐赠回执，无法绕过）。
var autoActions = []taskAction{
	{Code: "chat_5", Desc: "上报 5 条对话活跃事件", Run: runChatFive},
	{Code: "first_buddy", Desc: "解锁并领养第一只 Buddy", Run: runFirstBuddy},
	{Code: "Model_chat_GLM5.2", Desc: "glm-5.2 真实对话一次并对齐上报", Run: runModelChat, UsesChat: true},
	{Code: "RichMeow_Chat", Desc: "桌面指纹对话事件链", Run: runRichMeow},
	{Code: "Buddy_App", Desc: "进入 Buddy 应用事件链", Run: runBuddyApp},
	{Code: "Buddy_App_QQ", Desc: "进入企鹅教师助手事件链", Run: runBuddyApp},
	{Code: "automation_1", Desc: "上报定时任务创建事件", Run: runAutomation},
	{Code: "Library_read", Desc: "上报资料库阅读事件", Run: runLibraryRead},
	{Code: "template_5", Desc: "上报模板使用事件组 ×5", Run: runTemplateUse},
	{Code: "playbook_prompt", Desc: "上报灵感案例发送事件组", Run: runPlaybookPrompt},
	{Code: "create_canvas", Desc: "上报设计画布创建事件组", Run: runDesignCanvas},
	{Code: "expert_5", Desc: "真实专家召唤+使用链 ×5", Run: runExpertFive, UsesChat: true},
	{Code: "Expert_team_use_3", Desc: "真实专家团召唤+使用链 ×3", Run: runExpertTeam, UsesChat: true},
	{Code: "Hp_Appearance", Desc: "设置主题 + 皮肤生效事件", Run: runAppearance},
	{Code: "skill_1", Desc: "真实对话 + 技能加载事件", Run: runSkillFresh, UsesChat: true},
	{Code: "Expert_lighthouse", Desc: "轻量云专家召唤+使用链", Run: runExpertLighthouse, UsesChat: true},
	{Code: "black_cat", Desc: "夜猫子：夜间窗口内 glm-5.2 对话", Run: runBlackCat, UsesChat: true},
}

// autoActionFor 按任务码取动作。
func autoActionFor(code string) *taskAction {
	trimmed := strings.TrimSpace(code)
	for index := range autoActions {
		if autoActions[index].Code == trimmed {
			return &autoActions[index]
		}
	}
	return nil
}

// autoActionIndex 返回任务在动作表里的位置（用于队列排序）。
func autoActionIndex(code string) int {
	trimmed := strings.TrimSpace(code)
	for index := range autoActions {
		if autoActions[index].Code == trimmed {
			return index
		}
	}
	return len(autoActions)
}

// 各任务动作的实现。
//
// 全部遵循同一形状：构造事件链（或发真实对话）→ 上报。
// 具体的事件结构由 tasks 包提供，本文件只负责编排。

// runChatFive 上报 5 条对话活跃事件。
//
// 这是 first_buddy 的前置，因此排在动作表第一位。
func runChatFive(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	for index := 0; index < 5; index++ {
		conversationID := fmt.Sprintf("wb2api-chat5-%d-%d", nowUnixMs(), index)
		if errReport := client.ReportChatActivity(ctx, credential, conversationID, "", "", ""); errReport != nil {
			return "", errReport
		}
		if !sleepCtx(ctx, 1050*1e6) {
			return "", ctx.Err()
		}
	}
	return "上报 5 条活跃事件", nil
}

// runFirstBuddy 领养第一只 Buddy（含前置活跃上报）。
func runFirstBuddy(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	return tasks.RunTravel(ctx, client, credential), nil
}

// runModelChat 用 glm-5.2 发一次真实对话，并上报对齐模型的事件。
func runModelChat(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-glm-%d", nowUnixMs())
	if errChat := client.DesktopChat(ctx, credential, "", "glm-5.2", "GLM-5.2", conversationID); errChat != nil {
		return "", errChat
	}
	if errReport := client.ReportChatActivity(ctx, credential, conversationID, "", "glm-5.2", "GLM-5.2"); errReport != nil {
		return "", errReport
	}
	return "glm-5.2 对话 + 上报", nil
}

// runRichMeow 上报桌面对话事件链（6 事件）。
func runRichMeow(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-meow-%d", nowUnixMs())
	requestID := newHexID()
	events := withDesktopFingerprint(credential,
		tasks.DesktopChatSequence(conversationID, requestID, newHexID(), "fast-model", "fast-model"))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "桌面对话事件链", nil
}

// runBuddyApp 上报 Buddy 应用事件链（同时满足 Buddy_App 与 Buddy_App_QQ）。
func runBuddyApp(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	events := withDesktopFingerprint(credential,
		tasks.DesktopBuddyAppSequence(tasks.DefaultBuddyID, tasks.DefaultBuddyName))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "Buddy 应用事件链", nil
}

// runAutomation 上报定时任务创建事件。
func runAutomation(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	events := withDesktopFingerprint(credential, tasks.DesktopAutomationCreateEvent("wb2api 自动化"))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "定时任务创建事件", nil
}

// runLibraryRead 上报资料库阅读事件（web 指纹）。
func runLibraryRead(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	const pageURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if errReport := client.ReportWebElementClick(ctx, credential, pageURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); errReport != nil {
		return "", errReport
	}
	return "资料库阅读事件", nil
}

// runTemplateUse 上报模板使用事件组 ×5。
func runTemplateUse(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	templates := []struct{ id, name string }{
		{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"},
		{"4", "活动策划"}, {"5", "代码评审"},
	}
	for _, template := range templates {
		conversationID := fmt.Sprintf("wb2api-tpl-%d-%s", nowUnixMs(), template.id)
		requestID := newHexID()
		events := withDesktopFingerprint(credential,
			tasks.DesktopTemplateUseSequence(conversationID, requestID, template.id, template.name))
		if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
			return "", errReport
		}
		if !sleepCtx(ctx, 1050*1e6) {
			return "", ctx.Err()
		}
	}
	return "模板使用事件 ×5", nil
}

// runPlaybookPrompt 上报灵感案例发送事件组。
func runPlaybookPrompt(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-pb-%d", nowUnixMs())
	requestID := fmt.Sprintf("wb2api-pb-req-%d", nowUnixMs())
	events := withDesktopFingerprint(credential,
		tasks.DesktopPlaybookPromptSequence(conversationID, requestID,
			"pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸"))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "灵感案例事件组", nil
}

// runDesignCanvas 上报设计画布创建事件组。
func runDesignCanvas(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-canvas-%d", nowUnixMs())
	requestID := newHexID()
	events := withDesktopFingerprint(credential,
		tasks.DesktopDesignCanvasSequence(conversationID, requestID))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "设计画布事件组", nil
}

// runExpertFive 专家召唤 + 真实使用链 ×5。
//
// 关键约束：expert_id 必须来自**专家市场**（自造 id 不入账），
// 且真实对话的 requestId 必须取自**服务端 SSE**（自造 UUID 不计数）。
func runExpertFive(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	return runExpertChain(ctx, client, credential, "agent", 5)
}

// runExpertTeam 专家团召唤 + 使用链 ×3。
func runExpertTeam(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	return runExpertChain(ctx, client, credential, "team", 3)
}

// runExpertChain 是专家类任务的共用实现。
func runExpertChain(ctx context.Context, client *cb.Client, credential *cb.Credential, expertType string, count int) (string, error) {
	experts, errList := client.MarketExpertList(ctx, credential, expertType)
	if errList != nil {
		return "", errList
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场没有可用的 %s 类型专家", expertType)
	}
	completed := 0
	for index := 0; index < count; index++ {
		expert := experts[index%len(experts)]
		conversationID := fmt.Sprintf("wb2api-expert-%d-%d", nowUnixMs(), index)

		// 召唤链。
		if errReport := client.ReportDesktopEvents(ctx, credential,
			withDesktopFingerprint(credential, tasks.DesktopExpertSummonSequence(expert))); errReport != nil {
			return "", errReport
		}
		if !sleepCtx(ctx, 6e9) {
			return "", ctx.Err()
		}
		// 真实对话取服务端 requestId。
		requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, expert.ID, "fast-model", conversationID)
		if errChat != nil {
			logger.Debug("expert chat failed for %s: %v", expert.ID, errChat)
			continue
		}
		// 使用链。
		if errReport := client.ReportDesktopEvents(ctx, credential,
			withDesktopFingerprint(credential, tasks.DesktopExpertActualUseEvent(expert, conversationID, requestID))); errReport != nil {
			return "", errReport
		}
		completed++
		if !sleepCtx(ctx, 6e9) {
			return "", ctx.Err()
		}
	}
	if completed == 0 {
		return "", fmt.Errorf("专家使用链一次都没成功")
	}
	return fmt.Sprintf("专家使用链 %d 次", completed), nil
}

// runAppearance 设置主题并上报皮肤生效事件。
//
// 两步缺一不可：**只调 set 接口不计分**，必须叠加事件。
func runAppearance(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	if errSet := client.DesktopAppearanceSet(ctx, credential, "theme-tkmw7j"); errSet != nil {
		return "", errSet
	}
	events := withDesktopFingerprint(credential, []map[string]any{{
		"eventCode":   "appearance_skin_apply",
		"resourceKey": "theme-tkmw7j", "kind": "theme", "mode": "LOCAL",
	}})
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "主题设置 + 皮肤事件", nil
}

// runSkillFresh 真实对话 + 技能加载事件。
func runSkillFresh(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-skill-%d", nowUnixMs())
	requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, "", "fast-model", conversationID)
	if errChat != nil {
		return "", errChat
	}
	events := withDesktopFingerprint(credential, []map[string]any{{
		"eventCode": "skill_info", "skillId": "skill_2097350077599879168",
		"conversationId": conversationID, "requestId": requestID,
		"parentConversationId": conversationID, "mode": "LOCAL",
	}})
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "技能加载事件", nil
}

// runExpertLighthouse 轻量云专家召唤 + 使用链。
//
// 与普通专家链的三处差异（决定能否入账）：
// agent_task_created 带 has_expert=true、expert_actual_use 的 mode 为 LOCAL 且 type 为空、cost 为 0。
func runExpertLighthouse(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	const expertID = "ex_2cvvUZQhDyeJ"
	const expertName = "轻量云专家"
	conversationID := fmt.Sprintf("wb2api-lighthouse-%d", nowUnixMs())

	created := tasks.DesktopChatSequence(conversationID, newHexID(), newHexID(), "fast-model", "fast-model")
	if len(created) > 0 {
		created[0]["has_expert"] = true
		created[0]["expert_id"] = expertID
		created[0]["expert_name"] = expertName
		created[0]["expert_industry_id"] = ""
	}
	if errReport := client.ReportDesktopEvents(ctx, credential, withDesktopFingerprint(credential, created)); errReport != nil {
		return "", errReport
	}
	if !sleepCtx(ctx, 6e9) {
		return "", ctx.Err()
	}

	requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, expertID, "fast-model", conversationID)
	if errChat != nil {
		return "", errChat
	}
	events := withDesktopFingerprint(credential, []map[string]any{{
		"eventCode": "expert_actual_use", "id": expertID, "name": expertName,
		"expertTitle": expertName, "type": "", "expertType": "builtin",
		"source": "builtin", "version": "1.0.0",
		"cost": 0, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID,
		"messageId":      "msg-" + tailOf(requestID, 8),
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"mode": "LOCAL",
	}})
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "轻量云专家链", nil
}

// runBlackCat 夜猫子任务补足。
func runBlackCat(ctx context.Context, client *cb.Client, credential *cb.Credential) (string, error) {
	return tasks.RunBlackcat(ctx, client, credential)
}

// withDesktopFingerprint 给事件链注入桌面指纹。
func withDesktopFingerprint(credential *cb.Credential, events []map[string]any) []map[string]any {
	return tasks.ApplyDesktopFingerprint(credential, events)
}

// tailOf 取字符串末尾 n 个字符。
func tailOf(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}
