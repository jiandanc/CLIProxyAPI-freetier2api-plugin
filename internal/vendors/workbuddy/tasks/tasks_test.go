package tasks

// 本文件测试事件链构造与指纹注入。
//
// 这些结构是上游计分的判据，字段名或常量写错会让任务静默不计分
// （上报仍返回 200），因此值得用测试把关键字段锁住。

import (
	"testing"
	"time"

	"freetier2api-plugin/internal/vendors/workbuddy"
)

// testCredential 构造一个测试凭证。
func testCredential(uid string) *workbuddy.Credential {
	credential := &workbuddy.Credential{
		AccessToken:  "at",
		RefreshToken: "rt",
		UID:          uid,
		Nickname:     "测试账号",
		Domain:       "www.codebuddy.cn",
	}
	credential.SetRealm(workbuddy.RegionCN)
	return credential
}

// TestDesktopChatSequenceShape 验证对话链的结构与关键字段。
func TestDesktopChatSequenceShape(t *testing.T) {
	events := DesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")
	if len(events) != 6 {
		t.Fatalf("chat sequence must have 6 events, got %d", len(events))
	}

	codes := make([]string, 0, len(events))
	for _, event := range events {
		code, _ := event["eventCode"].(string)
		codes = append(codes, code)
	}
	want := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	for index, expected := range want {
		if codes[index] != expected {
			t.Fatalf("event %d = %q, want %q", index, codes[index], expected)
		}
	}

	// chat_message_response 必须带成功回执：服务端对事件链有真实性校验，
	// 缺了它整条链不会计分。
	response := events[3]
	if response["isSuccessful"] != true {
		t.Fatal("chat_message_response must carry isSuccessful=true")
	}
	// 会话标识必须贯穿。
	if events[2]["codebuddy.session_id"] != "conv-1" {
		t.Fatalf("session id missing on chat_request_send: %v", events[2]["codebuddy.session_id"])
	}
	if events[2]["codebuddy.conversation_request_id"] != "req-1" {
		t.Fatal("conversation request id must be the round-aggregation key")
	}
}

// TestDesktopBuddyAppSequence 验证 Buddy 应用链（同时满足两个任务）。
func TestDesktopBuddyAppSequence(t *testing.T) {
	events := DesktopBuddyAppSequence(DefaultBuddyID, DefaultBuddyName)
	if len(events) != 5 {
		t.Fatalf("buddy app sequence must have 5 events, got %d", len(events))
	}
	for index, event := range events {
		if event["buddyId"] != DefaultBuddyID {
			t.Fatalf("event %d must carry the buddy id", index)
		}
		if event["mode"] != "LOCAL" {
			t.Fatalf("event %d mode = %v, want LOCAL", index, event["mode"])
		}
	}
}

// TestDesktopTemplateUseSequence 验证模板链 = 对话链 + 2 个模板事件。
func TestDesktopTemplateUseSequence(t *testing.T) {
	events := DesktopTemplateUseSequence("conv", "req", "tpl-1", "深度研究")
	if len(events) != 8 {
		t.Fatalf("template sequence = chat(6) + 2, got %d", len(events))
	}
	if events[6]["eventCode"] != "agent_task_created_with_template" {
		t.Fatalf("event 6 = %v", events[6]["eventCode"])
	}
	if events[7]["eventCode"] != "template_used" {
		t.Fatalf("event 7 = %v", events[7]["eventCode"])
	}
	// 模板 id 会带到事件里（服务端不校验真实性，但不能为空）。
	if events[7]["template_id"] != "tpl-1" {
		t.Fatal("template id must be carried")
	}
}

// TestDesktopPlaybookPromptSequence 验证灵感案例链的判据事件。
func TestDesktopPlaybookPromptSequence(t *testing.T) {
	events := DesktopPlaybookPromptSequence("conv", "req", "case-1", "案例名")
	if len(events) != 9 {
		t.Fatalf("playbook sequence = chat(6) + 3, got %d", len(events))
	}
	// 判据是 playbook_prompt_send（真正的发送动作），不是卡片点击。
	if events[8]["eventCode"] != "playbook_prompt_send" {
		t.Fatalf("last event = %v, want playbook_prompt_send", events[8]["eventCode"])
	}
	if events[8]["conversationId"] != "conv" || events[8]["requestId"] != "req" {
		t.Fatal("prompt send must carry conversation and request ids")
	}
}

// TestDesktopDesignCanvasSequence 验证设计画布链。
func TestDesktopDesignCanvasSequence(t *testing.T) {
	events := DesktopDesignCanvasSequence("conv", "abcdefgh12345678")
	if len(events) != 8 {
		t.Fatalf("canvas sequence = chat(6) + 2, got %d", len(events))
	}
	if events[6]["eventCode"] != "wbx_design_canvas_task_create" {
		t.Fatalf("event 6 = %v", events[6]["eventCode"])
	}
	if events[7]["eventCode"] != "wbx_design_canvas_open" {
		t.Fatalf("event 7 = %v", events[7]["eventCode"])
	}
	// 画布 id 由 requestId 尾部派生。
	if events[7]["id"] != "ardot-file-12345678" {
		t.Fatalf("canvas id = %v, want ardot-file-<last8>", events[7]["id"])
	}
}

// TestSchoolChatTimesEventsActivityID 验证开学季的 activityId 门控。
//
// 校园日任务要求事件带 activityId，缺失则不点亮。
func TestSchoolChatTimesEventsActivityID(t *testing.T) {
	withActivity := SchoolChatTimesEvents("conv", SchoolSeasonActivityID)
	if len(withActivity) != 1 {
		t.Fatalf("expected 1 event, got %d", len(withActivity))
	}
	if withActivity[0]["activityId"] != SchoolSeasonActivityID {
		t.Fatal("activity id must be present when requested")
	}

	withoutActivity := SchoolChatTimesEvents("conv", "")
	if _, okActivity := withoutActivity[0]["activityId"]; okActivity {
		t.Fatal("activity id must be absent when not requested")
	}
	// 小程序事件用 mp 身份。
	if withoutActivity[0]["agentName"] != "mp" {
		t.Fatalf("agentName = %v, want mp", withoutActivity[0]["agentName"])
	}
}

// TestMiniChatModelEvent 验证小程序指定模型事件。
func TestMiniChatModelEvent(t *testing.T) {
	events := MiniChatModelEvent("conv", "glm-5.2", "GLM-5.2")
	if events[0]["requestModelId"] != "glm-5.2" || events[0]["requestModelName"] != "GLM-5.2" {
		t.Fatalf("model fields missing: %+v", events[0])
	}
}

// TestMiniExpertUseEventShape 验证小程序专家事件的字段形态。
//
// 真实事件**不带** conversationId / activityId，多带字段反而可能不被识别。
func TestMiniExpertUseEventShape(t *testing.T) {
	events := MiniExpertUseEvent("ex_realid", "专家名", "agent")
	event := events[0]
	if event["eventCode"] != "expert_actual_use" {
		t.Fatalf("eventCode = %v", event["eventCode"])
	}
	if event["type"] != "send_message" {
		t.Fatalf("mp expert event type must be send_message, got %v", event["type"])
	}
	if event["source"] != "mini_program" {
		t.Fatalf("source = %v, want mini_program", event["source"])
	}
	for _, absent := range []string{"conversationId", "activityId"} {
		if _, okField := event[absent]; okField {
			t.Fatalf("%s must be absent on mp expert events", absent)
		}
	}
}

// TestApplyDesktopFingerprintInjectsCommonFields 验证指纹注入。
func TestApplyDesktopFingerprintInjectsCommonFields(t *testing.T) {
	credential := testCredential("uid-1")
	events := ApplyDesktopFingerprint(credential, []map[string]any{
		{"eventCode": "x", "userId": "business-wins"},
	})
	event := events[0]
	// 业务字段优先于指纹基底。
	if event["userId"] != "business-wins" {
		t.Fatalf("business field must win over the fingerprint base, got %v", event["userId"])
	}
	// 指纹字段补齐。
	for _, key := range []string{"machineId", "sessionId", "extName", "ideName", "timezone"} {
		if _, okField := event[key]; !okField {
			t.Fatalf("fingerprint field %q must be injected", key)
		}
	}
	if event["extName"] != "workbuddy-desktop" {
		t.Fatalf("extName = %v (this is what routes the event to the desktop task family)", event["extName"])
	}
}

// TestFingerprintStability 验证设备指纹的稳定性与隔离性。
func TestFingerprintStability(t *testing.T) {
	credentialA := testCredential("uid-a")
	credentialB := testCredential("uid-b")

	machineA := fingerprint(credentialA, "machine")
	if machineA != fingerprint(credentialA, "machine") {
		t.Fatal("fingerprint must be stable")
	}
	if len(machineA) != 36 {
		t.Fatalf("fingerprint length = %d, want 36", len(machineA))
	}
	if fingerprint(credentialB, "machine") == machineA {
		t.Fatal("different accounts must get different fingerprints")
	}
	if fingerprint(credentialA, "session") == machineA {
		t.Fatal("different purposes must get different fingerprints")
	}
}

// TestInNightWindow 验证夜猫子窗口判定。
//
// 窗口外跑夜猫子任务既消耗额度又不计分，因此边界必须准确。
func TestInNightWindow(t *testing.T) {
	cases := []struct {
		hour int
		want bool
	}{
		{0, true}, {7, true}, {8, false}, {12, false},
		{22, false}, {23, true},
	}
	for _, testCase := range cases {
		now := time.Date(2026, 9, 26, testCase.hour, 30, 0, 0, time.Local)
		if got := InNightWindow(now); got != testCase.want {
			t.Fatalf("InNightWindow(%02d:30) = %v, want %v", testCase.hour, got, testCase.want)
		}
	}
}

// TestRandomHex 验证随机 ID 生成。
func TestRandomHex(t *testing.T) {
	first := randomHex(8)
	second := randomHex(8)
	if len(first) != 16 {
		t.Fatalf("randomHex(8) length = %d, want 16", len(first))
	}
	if first == second {
		t.Fatal("randomHex must not repeat")
	}
}
