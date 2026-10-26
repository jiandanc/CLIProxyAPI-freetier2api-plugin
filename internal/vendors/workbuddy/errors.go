package workbuddy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
)

// 本文件实现上游错误分类。
//
// 为什么需要它：上游用 HTTP 状态码 + 业务 code + 自然语言消息三种手段混合表达错误，
// 而同一种 HTTP 状态码可能对应完全不同的处置（例如 429 既可能是限流、也可能是余额耗尽）。
// 分类结果决定：是冷却这个账号、禁用这个账号、只避让这个模型，还是根本不罚账号。
//
// 判定顺序是有语义的，不能重排（见 Classify 的注释）。

// 本供应商使用的错误类别（与 core.FailureKind 一一对应）：
//   - none：表示成功。
//   - hard_credit：余额耗尽（402 或余额关键词）→ 长冷却到次日。
//   - soft_rate：限流（429）→ 短冷却，指数退避。
//   - session_dead：会话失效（12153）→ 禁用，需重新登录。
//   - not_found：上游偶发 404 → 固定短冷却，不累计熔断。
//   - server：上游 5xx → 计入熔断。
//   - content_blocked：内容策略拦截 → 不罚账号，可降级重试。
//   - channel_rejected：渠道校验拦截（11128）→ 请求级，不罚号、不换号，保留上游说明。
//   - bad_params：参数非法 → 不罚账号，但仍换号重试。
//   - account_fault：账号级故障（request illegal / trial 未激活）→ 冷却轮换。
//   - model_blocked：该后端无此模型（11102）→ 按（账号, 模型）避让。
//   - waf_block：上游 WAF 拦截（403 非业务信封）→ 账号软冷却。
//   - prompt_too_long：上下文超限（11115）→ 请求级，不罚号不轮转。
//   - image_invalid：图片无效 → 请求级，不罚号不轮转。
//   - client：其他 4xx / 业务错误。

// 关键词表。
//
// 全部小写比对，中文与英文双通道：上游对不同租户/区域返回的文案不一致。
var (
	hardMarkers = []string{
		"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
		"quota exceeded", "quota exhaust", "payment required", "credit not enough", "not enough credit",
		"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	}
	softRateMarkers = []string{
		"rate limit", "rate-limiting", "rate-limited", "too many requests", "too many",
		"usage limit", "请求过于频繁", "限流",
	}
	sessionDeadMarkers  = []string{"offline user session not found", "12153"}
	accountFaultMarkers = []string{
		"request illegal", "trial not activated", "trial version is not yet activated",
	}
	contentBlockedMarkers = []string{"blocked by security policy"}
	// channelRejectedMarkers 是渠道校验失败（11128）的文案：说明请求不是官方客户端发出的。
	channelRejectedMarkers = []string{"unapproved channel", "illegal api invocation"}
	invalidImageMarkers    = []string{
		"invalid image_url content", "invalid_image_data", "replace the image",
	}
	promptTooLongMarkers = []string{
		`"code":11115`, `"code": 11115`, `"code":"11115"`, "prompt is too long",
	}
	alreadyCheckinMarkers = []string{"已签到", "already"}
)

const (
	badParamsMarkerMsg  = "unmarshal chat params failed"
	badParamsMarkerCode = `"code":11101`
	// modelRateLimitCode 是「模型级」限流：只避让该模型，不惩罚整个账号。
	modelRateLimitCode = "6004"
	// modelBlockCode 是「该后端无此模型」。
	modelBlockCode = "11102"
	// channelRejectedCode 是渠道校验失败（非官方客户端）。
	channelRejectedCode = "11128"

	// ModelBlockReason 是（账号, 模型）避让的固定原因串。
	ModelBlockReason = "11102 model not available"
	// ModelRateLimitReason 是模型级限流的固定原因串。
	ModelRateLimitReason = "6004 model rate limited"
)

var (
	modelRateLimitRe = regexp.MustCompile(`"code"\s*:\s*"?6004"?`)
	cnResetRe        = regexp.MustCompile(`将在 (.+?) 重置`)
	enResetRe        = regexp.MustCompile(`(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)
	// softRateResetLoc 是上游重置时刻使用的时区（固定 UTC+8，不依赖系统 tzdata）。
	softRateResetLoc    = time.FixedZone("UTC+8", 8*3600)
	softRateTimeLayout  = "2006-01-02 15:04:05"
	retryAfterSanityCap = 2 * time.Hour
)

// Classify 按状态码与响应体判定错误类别。
//
// 判定顺序是关键，尤其这几条：
//   - 11102 必须最先：它可能是 400 也可能是 404，晚判会被 404 分支吞掉；
//   - 429 必须早于 hardMarkers：429 的正文高频携带 "quota exceeded"/"额度不足"
//     这类跨计费与限流两界的措辞，hardMarkers 先判会把限流误归余额耗尽，
//     白扔一个号约 12 小时。真正的余额耗尽由 402 或业务码 14018 捕获；
//   - 11115（上下文超限）与 11128（渠道校验）必须早于 404/5xx/WAF/内容策略：
//     它们是请求级错误，罚账号是错的；
//   - accountFault 必须早于 429：账号被封的措辞里也可能带限流字样。
func Classify(status int, body string) *core.Failure {
	lower := strings.ToLower(body)

	// 0) 模型级避让：只比对独立字段，绝不做整段文本子串匹配——
	// 否则 "11102" 撞在 requestId 上会误避让一个可用模型。
	if isModelBlocked(status, body) {
		return &core.Failure{Kind: core.FailureKindModelBlocked, Status: status, Msg: body}
	}
	// 渠道校验失败（11128）：上游常把它包在 HTTP 500 里返回，但它是请求级拒绝。
	// 按 5xx 处理会让宿主把全部账号当服务端故障冷却，后续重试退化成
	// 503 auth_unavailable（只剩数字码，可读说明丢失）。这里改写成 400，
	// 并把多语言正文折叠成一条可读消息（原文由调用方写进日志留证）。
	if codeMarker(lower, channelRejectedCode) || core.ContainsAny(lower, channelRejectedMarkers) {
		return &core.Failure{Kind: core.FailureKindChannelRejected, Status: http.StatusBadRequest,
			Code: channelRejectedCode, Msg: channelRejectedMessage(body)}
	}
	// 1) 余额耗尽（明确的 402）。
	if status == http.StatusPaymentRequired {
		return &core.Failure{Kind: core.FailureKindHardCredit, Status: status, Msg: body}
	}
	// 2) 会话失效。
	if core.ContainsAny(lower, sessionDeadMarkers) {
		return &core.Failure{Kind: core.FailureKindSessionDead, Status: status, Msg: body}
	}
	// 3) 账号级故障（注意：不能按 11140 判定——该 code 也承载模型级限流文案）。
	if core.ContainsAny(lower, accountFaultMarkers) {
		return &core.Failure{Kind: core.FailureKindAccountFault, Status: status, Msg: body}
	}
	// 4) 429 + 业务码 14018 表示余额真的耗尽。
	if status == http.StatusTooManyRequests && hasBusinessCode(body, "14018") {
		return &core.Failure{Kind: core.FailureKindHardCredit, Status: status, Msg: body}
	}
	// 5) 普通限流。
	if status == http.StatusTooManyRequests {
		return &core.Failure{Kind: core.FailureKindSoftRate, Status: status, Msg: body}
	}
	// 6) 余额关键词（在 429 之后）。
	if core.ContainsAny(lower, hardMarkers) {
		return &core.Failure{Kind: core.FailureKindHardCredit, Status: status, Msg: body}
	}
	// 7) 限流关键词。
	if core.ContainsAny(lower, softRateMarkers) {
		return &core.Failure{Kind: core.FailureKindSoftRate, Status: status, Msg: body}
	}
	// 8) 上下文超限（请求级）。
	if (status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusRequestEntityTooLarge) &&
		core.ContainsAny(body, promptTooLongMarkers) {
		return &core.Failure{Kind: core.FailureKindPromptTooLong, Status: status, Msg: body}
	}
	// 9) 偶发 404。
	if status == http.StatusNotFound {
		return &core.Failure{Kind: core.FailureKindNotFound, Status: status, Msg: body}
	}
	// 10) 上游故障。
	if status >= 500 {
		return &core.Failure{Kind: core.FailureKindServer, Status: status, Msg: body}
	}
	// 11) WAF 拦截：403 且没有业务信封（说明不是上游业务返回的）。
	if IsWAFBlocked(status, body) {
		return &core.Failure{Kind: core.FailureKindWAFBlock, Status: status, Msg: body}
	}
	// 12) 图片无效。
	if status == http.StatusBadRequest && (codeMarker(lower, "11135") || core.ContainsAny(lower, invalidImageMarkers)) {
		return &core.Failure{Kind: core.FailureKindImageInvalid, Status: status, Msg: body}
	}
	// 13) 内容策略拦截。
	if status >= 400 && core.ContainsAny(lower, contentBlockedMarkers) {
		return &core.Failure{Kind: core.FailureKindContentBlocked, Status: status, Msg: body}
	}
	// 14) 参数非法。
	if status >= 400 && (strings.Contains(lower, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode)) {
		return &core.Failure{Kind: core.FailureKindBadParams, Status: status, Msg: body}
	}
	// 15) 其他 4xx。
	if status >= 400 {
		return &core.Failure{Kind: core.FailureKindClient, Status: status, Msg: body}
	}
	return &core.Failure{Kind: core.FailureKindNone, Status: status, Msg: body}
}

// isModelBlocked 判断是否为「该后端无此模型」（11102）。
//
// 只比对**独立字段**（code / errCode / error_code 与 msg / message），
// 遍历顶层与 error 子对象两层，绝不做整段文本子串匹配。
func isModelBlocked(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		return false
	}
	if !strings.Contains(body, modelBlockCode) && !strings.Contains(body, modelBlockMsgMarker) {
		// 轻量预检短路：连标志串都没有就不用解析 JSON 了。
		return false
	}
	var fields map[string]any
	if errUnmarshal := json.Unmarshal([]byte(body), &fields); errUnmarshal != nil {
		return false
	}
	if objectHasModelBlock(fields) {
		return true
	}
	if nested, okNested := fields["error"].(map[string]any); okNested {
		return objectHasModelBlock(nested)
	}
	return false
}

const modelBlockMsgMarker = "service info not found"

// objectHasModelBlock 检查一个 JSON 对象里的 code/msg 字段是否表示模型不可用。
func objectHasModelBlock(fields map[string]any) bool {
	for _, key := range []string{"code", "errCode", "error_code"} {
		if value, okValue := fields[key]; okValue {
			if fmt.Sprintf("%v", value) == modelBlockCode {
				return true
			}
		}
	}
	for _, key := range []string{"msg", "message"} {
		if value, okValue := fields[key].(string); okValue &&
			strings.Contains(strings.ToLower(value), modelBlockMsgMarker) {
			return true
		}
	}
	return false
}

// channelRejectedMessage 把 11128 的正文折叠成一条可读、可反馈的错误消息。
//
// 为什么要折叠：上游正文同时带 msg / displayMsg / displayTips 的中英繁体三份文案，
// 原样透传会把「为什么被拦、找谁对齐」淹在 JSON 里，用户看到的只是一坨 500。
// 折叠后只留三段：上游英文 msg（可搜索）、面向用户的中文提示、requestId。
func channelRejectedMessage(body string) string {
	var payload struct {
		Msg         string            `json:"msg"`
		RequestID   string            `json:"requestId"`
		DisplayMsg  map[string]string `json:"displayMsg"`
		DisplayTips map[string]string `json:"displayTips"`
	}
	text := ""
	if errUnmarshal := json.Unmarshal([]byte(body), &payload); errUnmarshal == nil {
		text = strings.TrimSpace(payload.Msg)
		if hint := localizedText(payload.DisplayTips, payload.DisplayMsg); hint != "" {
			if text != "" {
				text += " — "
			}
			text += hint
		}
	}
	if text == "" {
		// 正文不是可解析的业务信封（HTML 拦截页等）：透传截断后的原文，不吞证据。
		text = core.Truncate(body, 512)
	}
	if text == "" {
		text = "channel verification failed"
	}
	trace := "code=" + channelRejectedCode
	if requestID := strings.TrimSpace(payload.RequestID); requestID != "" {
		trace += " requestId=" + requestID
	}
	return text + " (" + trace + ")"
}

// localizedText 按「中文 > 英文 > 繁体」取第一条非空文案；tips 优先于 msg。
func localizedText(tips, msg map[string]string) string {
	for _, source := range []map[string]string{tips, msg} {
		for _, key := range []string{"zh", "en", "zh-hant"} {
			if text := strings.TrimSpace(source[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

// IsWAFBlocked 判断是否为 WAF 拦截。
//
// 判据：403 且正文里没有业务信封标记（"code": 或 "msg":）。
// 业务错误即使带 403 也会有这两个键。
func IsWAFBlocked(status int, body string) bool {
	if status != http.StatusForbidden {
		return false
	}
	return !strings.Contains(body, `"code":`) && !strings.Contains(body, `"msg":`)
}

// IsModelRateLimit 判断响应体是否表示模型级限流（6004）。
func IsModelRateLimit(body string) bool {
	return modelRateLimitRe.MatchString(body)
}

// IsAlreadyCheckin 判断错误是否为「今天已签到」。
//
// 签到接口对重复签到返回业务错误而非成功，调用方需要把它当成幂等成功处理。
func IsAlreadyCheckin(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range alreadyCheckinMarkers {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// ParseRateReset 从错误正文里解析上游给出的重置时刻。
//
// 中文文案优先（CN 域更常见），回落英文格式。都取不到返回零值。
func ParseRateReset(body string) time.Time {
	if match := cnResetRe.FindStringSubmatch(body); len(match) == 2 {
		if parsed, errParse := time.ParseInLocation(softRateTimeLayout, strings.TrimSpace(match[1]), softRateResetLoc); errParse == nil {
			return parsed
		}
	}
	if match := enResetRe.FindStringSubmatch(body); len(match) == 2 {
		if parsed, errParse := time.ParseInLocation(softRateTimeLayout, strings.TrimSpace(match[1]), softRateResetLoc); errParse == nil {
			return parsed
		}
	}
	return time.Time{}
}

// ParseRetryAfter 从响应头里解析上游建议的等待时长。
//
// 候选头与语义：Retry-After（秒）、Retry-After-Ms（毫秒）、
// X-Ratelimit-Reset（epoch 秒，≥12 位按毫秒解释）。
// HTTP-Date 形态与超过 2 小时的值一律忽略（上游偶发返回离谱值）。
func ParseRetryAfter(header http.Header, now time.Time) time.Duration {
	if header == nil {
		return 0
	}
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		if seconds, errAtoi := parseInt64(raw); errAtoi == nil && seconds > 0 {
			return clampRetryAfter(time.Duration(seconds) * time.Second)
		}
	}
	if raw := strings.TrimSpace(header.Get("Retry-After-Ms")); raw != "" {
		if millis, errAtoi := parseInt64(raw); errAtoi == nil && millis > 0 {
			return clampRetryAfter(time.Duration(millis) * time.Millisecond)
		}
	}
	if raw := strings.TrimSpace(header.Get("X-Ratelimit-Reset")); raw != "" {
		if value, errAtoi := parseInt64(raw); errAtoi == nil && value > 0 {
			if len(raw) >= 12 {
				value /= 1000
			}
			resetAt := time.Unix(value, 0)
			if resetAt.After(now) {
				return clampRetryAfter(resetAt.Sub(now))
			}
		}
	}
	return 0
}

// clampRetryAfter 把等待时长钳到合理区间。
func clampRetryAfter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > retryAfterSanityCap {
		return retryAfterSanityCap
	}
	return d
}

// parseInt64 解析十进制整数（上游的重试头有时带小数，这里只接受整数）。
func parseInt64(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

// codeMarker 判断正文里是否出现某个裸业务码（覆盖多种 JSON 空白/引号形态）。
func codeMarker(lower, code string) bool {
	if strings.Contains(lower, `"code":`+code) ||
		strings.Contains(lower, `"code": `+code) ||
		strings.Contains(lower, `"code":"`+code+`"`) ||
		strings.Contains(lower, `"code": "`+code+`"`) {
		return true
	}
	return strings.Contains(lower, `code=`+code)
}

// hasBusinessCode 递归查找正文里是否有 code 字段精确等于 want。
func hasBusinessCode(body, want string) bool {
	var payload any
	if errUnmarshal := json.Unmarshal([]byte(body), &payload); errUnmarshal != nil {
		return false
	}
	return scanBusinessCode(payload, want)
}

func scanBusinessCode(value any, want string) bool {
	switch typed := value.(type) {
	case map[string]any:
		if code, okCode := typed["code"]; okCode {
			if fmt.Sprintf("%v", code) == want {
				return true
			}
		}
		for _, nested := range typed {
			if scanBusinessCode(nested, want) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if scanBusinessCode(item, want) {
				return true
			}
		}
	}
	return false
}
