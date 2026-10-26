package workbuddy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

func TestClassifyOrderingMatters(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   core.FailureKind
	}{
		// 11102 必须先判：它可能是 400 也可能是 404，晚判会被 404 分支吞掉。
		{"model blocked on 400", 400, `{"code":11102,"msg":"service info not found"}`, core.FailureKindModelBlocked},
		{"model blocked on 404", 404, `{"code":11102,"msg":"service info not found"}`, core.FailureKindModelBlocked},
		// 429 必须早于余额关键词：429 正文常带 "quota exceeded" 这类跨界的措辞，
		// 先判余额会把限流误冷却到次日，白扔一个号。
		{"429 with credit wording stays rate limit", 429, `{"code":1,"msg":"quota exceeded, too many requests"}`, core.FailureKindSoftRate},
		// 真正的余额耗尽由 402 或业务码 14018 捕获。
		{"402 is hard credit", 402, `{"code":1,"msg":"payment required"}`, core.FailureKindHardCredit},
		{"429 with 14018 is hard credit", 429, `{"code":14018,"msg":"insufficient"}`, core.FailureKindHardCredit},
		// 会话失效优先于账号故障。
		{"session dead", 401, `{"code":12153,"msg":"Offline user session not found"}`, core.FailureKindSessionDead},
		// 账号故障不能按 11140 判定（该 code 同时承载模型级限流文案），只看关键词。
		{"account fault by keyword", 400, `{"msg":"request illegal"}`, core.FailureKindAccountFault},
		// 11140 带限流文案时必须按文案归为限流，而不是账号故障——
		// 按 code 判定会把一个只是被限流的账号当成被封禁处理。
		{"11140 with rate-limit wording is soft rate", 400, `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, core.FailureKindSoftRate},
		// 上下文超限是请求级错误，必须先于 404/5xx/WAF 判定。
		{"prompt too long on 400", 400, `{"code":11115,"msg":"prompt is too long"}`, core.FailureKindPromptTooLong},
		{"prompt too long on 413", 413, `{"code":11115,"msg":"prompt is too long"}`, core.FailureKindPromptTooLong},
		// WAF：403 且没有业务信封。
		{"waf block", 403, `<html>Request blocked by WAF</html>`, core.FailureKindWAFBlock},
		{"403 with envelope is not waf", 403, `{"code":1000,"msg":"quota exceeded"}`, core.FailureKindHardCredit},
		// 图片无效。
		{"image invalid", 400, `{"code":11135,"msg":"invalid_image_data"}`, core.FailureKindImageInvalid},
		// 内容策略拦截。
		{"content blocked", 400, `{"msg":"blocked by security policy"}`, core.FailureKindContentBlocked},
		// 渠道校验失败：上游会包在 HTTP 500 里，仍必须按请求级拒绝处理。
		{"channel rejected on 500", 500, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, core.FailureKindChannelRejected},
		{"channel rejected on 200", 200, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, core.FailureKindChannelRejected},
		// 11128 只出现在 requestId 里不算渠道校验失败。
		{"11128 in requestId is not channel rejected", 400, `{"code":12345,"msg":"bad request","requestId":"trace-11128-abc"}`, core.FailureKindClient},
		// 参数非法。
		{"bad params", 400, `{"msg":"Unmarshal chat params failed"}`, core.FailureKindBadParams},
		// 其它 4xx。
		{"other client error", 400, `{"code":12345,"msg":"whatever"}`, core.FailureKindClient},
		// 偶发 404（无 11102 标记）。
		{"plain 404", 404, `{"msg":"not found"}`, core.FailureKindNotFound},
		// 上游故障。
		{"server error", 500, `{"msg":"internal error"}`, core.FailureKindServer},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Classify(testCase.status, testCase.body)
			if got.Kind != testCase.want {
				t.Fatalf("Classify(%d, %q).Kind = %s, want %s",
					testCase.status, testCase.body, got.Kind, testCase.want)
			}
		})
	}
}

// TestClassifyModelBlockedNeedsFieldBoundary 验证 11102 只认同独立字段。
//
// 若做整段文本子串匹配，"11102" 出现在 requestId 里就会误避让一个可用模型。
func TestClassifyModelBlockedNeedsFieldBoundary(t *testing.T) {
	// 11102 只出现在 requestId 里：不该被判成模型不可用。
	body := `{"code":12345,"msg":"bad request","requestId":"trace-11102-abc"}`
	got := Classify(http.StatusBadRequest, body)
	if got.Kind == core.FailureKindModelBlocked {
		t.Fatalf("11102 inside requestId must not trigger model blocking: %s", got.Kind)
	}
	// code 字段真的是 11102：应当命中。
	if got := Classify(http.StatusBadRequest, `{"code":11102}`); got.Kind != core.FailureKindModelBlocked {
		t.Fatalf("explicit code 11102 must be model blocked, got %s", got.Kind)
	}
}

// TestClassifyRetryableAndStatus 验证可重试判定与状态码映射。
//
// 宿主按 HTTP 状态码决定凭证处置，这张映射表是「账号处置矩阵」的落点。
func TestClassifyRetryableAndStatus(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		wantStatus    int
	}{
		{"rate limit is retryable", 429, `{"msg":"rate limit"}`, true, 429},
		{"content blocked is not retryable", 400, `{"msg":"blocked by security policy"}`, false, 400},
		{"prompt too long is not retryable", 400, `{"msg":"prompt is too long"}`, false, 400},
		{"image invalid is not retryable", 400, `{"msg":"invalid_image_data"}`, false, 400},
		{"bad params is not retryable", 400, `{"msg":"Unmarshal chat params failed"}`, false, 400},
		// 渠道校验失败：上游用 500 表达，但宿主必须按请求级错误处置（400 不冷却账号）。
		{"channel rejected is not retryable", 500, `{"code":11128,"msg":"unapproved channel"}`, false, 400},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			classified := Classify(testCase.status, testCase.body)
			if classified.Retryable() != testCase.wantRetryable {
				t.Fatalf("Retryable() = %v, want %v", classified.Retryable(), testCase.wantRetryable)
			}
			if classified.HTTPStatus() != testCase.wantStatus {
				t.Fatalf("HTTPStatus() = %d, want %d", classified.HTTPStatus(), testCase.wantStatus)
			}
		})
	}
}

// TestParseRateResetAndRetryAfter 验证上游等待信息的解析。
func TestParseRateResetAndRetryAfter(t *testing.T) {
	// 中文文案优先。
	reset := ParseRateReset(`{"msg":"额度恢复将在 2026-09-27 04:00:00 重置"}`)
	if reset.IsZero() {
		t.Fatal("chinese rate reset wording must be parsed")
	}
	if reset.Hour() != 4 {
		t.Fatalf("reset hour = %d, want 4", reset.Hour())
	}

	header := http.Header{}
	header.Set("Retry-After", "30")
	if got := ParseRetryAfter(header, time.Now()); got != 30*time.Second {
		t.Fatalf("Retry-After = %v, want 30s", got)
	}
	// 离谱的值被钳制（上游偶发返回极大值）。
	header.Set("Retry-After", "999999")
	if got := ParseRetryAfter(header, time.Now()); got > time.Hour*2 {
		t.Fatalf("Retry-After must be capped, got %v", got)
	}
	// HTTP-Date 形态被忽略（不是纯数字）。
	header.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	if got := ParseRetryAfter(header, time.Now()); got != 0 {
		t.Fatalf("http-date Retry-After must be ignored, got %v", got)
	}
}

// TestClassifyIsModelRateLimit 验证模型级限流的识别。
func TestClassifyIsModelRateLimit(t *testing.T) {
	if !IsModelRateLimit(`{"code":6004,"msg":"rate-limiting"}`) {
		t.Fatal("6004 must be recognized as model-level rate limit")
	}
	if IsModelRateLimit(`{"code":1,"msg":"rate limit"}`) {
		t.Fatal("non-6004 must not be model-level rate limit")
	}
}

// TestChannelRejectedKeepsReadableDetail 验证 11128 不会退化成裸数字码。
//
// 宿主对不可重试的错误直接回给客户端；错误码与消息都必须保留 11128 与上游说明，
// 否则用户只能看到「no auth available / last upstream error: 11128」。
func TestChannelRejectedKeepsReadableDetail(t *testing.T) {
	body := `{"actions":["EDIT_INPUT","SUBMIT_FEEDBACK"],"code":11128,` +
		`"msg":"Illegal API invocation from an unapproved channel",` +
		`"displayMsg":{"en":"Request blocked. Please send it again"}}`
	classified := Classify(http.StatusInternalServerError, body)
	pluginErr, okPlugin := core.ToPluginError(classified).(*core.PluginError)
	if !okPlugin {
		t.Fatalf("ToPluginError() = %T, want *core.PluginError", core.ToPluginError(classified))
	}
	if pluginErr.Code != "upstream_channel_rejected" {
		t.Fatalf("code = %q, want upstream_channel_rejected", pluginErr.Code)
	}
	if pluginErr.Retryable || pluginErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("retryable=%v status=%d, want false/400", pluginErr.Retryable, pluginErr.HTTPStatus)
	}
	for _, want := range []string{"11128", "unapproved channel"} {
		if !strings.Contains(pluginErr.Message, want) {
			t.Fatalf("message %q must contain %q", pluginErr.Message, want)
		}
	}
}

// channelRejectedBody 是 Issue #42 的原始 11128 正文：上游新增渠道校验后的真实回包。
const channelRejectedBody = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel",` +
	`"requestId":"01a176819f987048670eda9f9a912d5",` +
	`"displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"},` +
	`"displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client. Please resend from the official client.",` +
	`"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。",` +
	`"zh-hant":"本次呼叫未通過通路驗證。"},` +
	`"actions":["EDIT_INPUT","SUBMIT_FEEDBACK"]}`

// TestChannelRejectedMessageFoldsMultilingualBody 验证 11128 折叠后的消息仍然可读、可反馈。
//
// 上游正文是 en/zh/zh-hant 三份文案的 JSON，直接透传只会让用户看到一坨 500；
// 折叠后必须仍保留上游 msg、中文提示与 requestId（反馈上游时唯一能对齐的凭据）。
func TestChannelRejectedMessageFoldsMultilingualBody(t *testing.T) {
	failure := Classify(http.StatusInternalServerError, channelRejectedBody)
	if failure.Kind != core.FailureKindChannelRejected || failure.Status != http.StatusBadRequest {
		t.Fatalf("kind=%s status=%d, want channel_rejected/400", failure.Kind, failure.Status)
	}
	if failure.Retryable() {
		t.Fatal("channel rejection must not be retryable: 换号同样过不了渠道校验")
	}
	for _, want := range []string{
		"Illegal API invocation from an unapproved channel",
		"这次调用没通过渠道校验",
		"code=11128",
		"requestId=01a176819f987048670eda9f9a912d5",
	} {
		if !strings.Contains(failure.Msg, want) {
			t.Fatalf("message %q must contain %q", failure.Msg, want)
		}
	}
	if strings.Contains(failure.Msg, "displayTips") {
		t.Fatalf("message must not dump the raw envelope: %q", failure.Msg)
	}
}

// TestChannelRejectedKeepsRawBodyInLog 验证拦截证据进日志。
//
// 消息折叠后正文不再随错误返回；日志必须留全量原文（含 requestId），
// 否则 Issue #42 截图里的信息在排障时会彻底丢失。
func TestChannelRejectedKeepsRawBodyInLog(t *testing.T) {
	_, since := logger.Snapshot(0, 1)
	enrichClassifyError(http.StatusInternalServerError, channelRejectedBody, http.Header{})
	entries, _ := logger.Snapshot(since, 200)
	for _, entry := range entries {
		if strings.Contains(entry.Message, "channel verification") &&
			strings.Contains(entry.Message, "01a176819f987048670eda9f9a912d5") {
			return
		}
	}
	t.Fatalf("channel rejection must be logged with the upstream requestId, got %d entries", len(entries))
}

// TestChannelRejectedFallsBackToRawBody 验证非 JSON 正文不会被折叠吞掉。
func TestChannelRejectedFallsBackToRawBody(t *testing.T) {
	failure := Classify(http.StatusForbidden, "Illegal API invocation from an unapproved channel")
	if failure.Kind != core.FailureKindChannelRejected {
		t.Fatalf("kind=%s, want channel_rejected", failure.Kind)
	}
	if !strings.Contains(failure.Msg, "Illegal API invocation from an unapproved channel") {
		t.Fatalf("message %q must keep the raw body", failure.Msg)
	}
}
