package workbuddy

// 本文件实现上游错误分类。
//
// 为什么需要它：上游用 HTTP 状态码 + 业务 code + 自然语言消息三种手段混合表达错误，
// 而同一种 HTTP 状态码可能对应完全不同的处置（例如 429 既可能是限流、也可能是余额耗尽）。
// 分类结果决定：是冷却这个账号、禁用这个账号、只避让这个模型，还是根本不罚账号。
//
// 判定顺序是有语义的，不能重排（见 Classify 的注释）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind 是错误类别。
type Kind int

const (
	// KindNone 表示成功。
	KindNone Kind = iota
	// KindHardCredit 余额耗尽（402 或余额关键词）→ 长冷却到次日。
	KindHardCredit
	// KindSoftRate 限流（429）→ 短冷却，指数退避。
	KindSoftRate
	// KindSessionDead 会话失效（12153）→ 禁用，需重新登录。
	KindSessionDead
	// KindNotFound 上游偶发 404 → 固定短冷却，不累计熔断。
	KindNotFound
	// KindServer 上游 5xx → 计入熔断。
	KindServer
	// KindContentBlocked 内容策略拦截 → 不罚账号，可降级重试。
	KindContentBlocked
	// KindBadParams 参数非法 → 不罚账号，但仍换号重试。
	KindBadParams
	// KindAccountFault 账号级故障（request illegal / trial 未激活）→ 冷却轮换。
	KindAccountFault
	// KindModelBlocked 该后端无此模型（11102）→ 按（账号, 模型）避让。
	KindModelBlocked
	// KindWAFBlock 上游 WAF 拦截（403 非业务信封）→ 账号软冷却。
	KindWAFBlock
	// KindPromptTooLong 上下文超限（11115）→ 请求级，不罚号不轮转。
	KindPromptTooLong
	// KindImageInvalid 图片无效 → 请求级，不罚号不轮转。
	KindImageInvalid
	// KindClient 其他 4xx / 业务错误。
	KindClient
)

// String 返回稳定的类别名（用于日志与错误码）。
func (k Kind) String() string {
	switch k {
	case KindNone:
		return "none"
	case KindHardCredit:
		return "hard_credit"
	case KindSoftRate:
		return "soft_rate"
	case KindSessionDead:
		return "session_dead"
	case KindNotFound:
		return "not_found"
	case KindServer:
		return "server"
	case KindContentBlocked:
		return "content_blocked"
	case KindBadParams:
		return "bad_params"
	case KindAccountFault:
		return "account_fault"
	case KindModelBlocked:
		return "model_blocked"
	case KindWAFBlock:
		return "waf_block"
	case KindPromptTooLong:
		return "prompt_too_long"
	case KindImageInvalid:
		return "image_invalid"
	case KindClient:
		return "client"
	}
	return "unknown"
}

// Error 是带分类的上游错误。
type Error struct {
	Kind Kind
	// Status 是上游 HTTP 状态码。
	Status int
	// Msg 是上游返回的正文（原文透传，不做规范化）。
	Msg string
	// RetryAfter 是上游明示的等待时长（来自响应头），0 表示未给出。
	RetryAfter time.Duration
	// ResetAt 是上游文案里给出的重置墙钟时刻，零值表示未给出。
	ResetAt time.Time
}

// Error 实现 error 接口。
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// HTTPStatus 返回映射给宿主的 HTTP 状态码。
//
// 这张表是「原项目账号处置矩阵」在插件形态下的等价物：插件不再自己维护
// 冷却状态，而是把语义翻译成宿主认得的 HTTP 状态码，由宿主决定处置。
func (e *Error) HTTPStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	if e.Status > 0 {
		// 上游状态码本身通常就是对的（402/429/401/403/4xx/5xx），优先透传。
		return e.Status
	}
	switch e.Kind {
	case KindHardCredit:
		return http.StatusPaymentRequired
	case KindSoftRate:
		return http.StatusTooManyRequests
	case KindSessionDead:
		return http.StatusUnauthorized
	case KindNotFound, KindModelBlocked:
		return http.StatusNotFound
	case KindServer:
		return http.StatusBadGateway
	case KindWAFBlock:
		return http.StatusForbidden
	default:
		return http.StatusBadRequest
	}
}

// Retryable 报告宿主是否应该换一个凭证重试同一请求。
//
// 请求级错误（内容拦截、参数非法、上下文超限、图片无效）换号也没用，返回 false；
// 账号级错误换号有意义，返回 true。
func (e *Error) Retryable() bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case KindContentBlocked, KindPromptTooLong, KindImageInvalid, KindBadParams:
		return false
	default:
		return true
	}
}

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
	contentBlockedMarkers = []string{
		"blocked by security policy", "unapproved channel", "illegal api invocation",
	}
	invalidImageMarkers = []string{
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
//   - 11115（上下文超限）必须早于 404/5xx/WAF/内容策略：它是请求级错误，
//     罚账号是错的；
//   - accountFault 必须早于 429：账号被封的措辞里也可能带限流字样。
func Classify(status int, body string) *Error {
	lower := strings.ToLower(body)

	// 0) 模型级避让：只比对独立字段，绝不做整段文本子串匹配——
	// 否则 "11102" 撞在 requestId 上会误避让一个可用模型。
	if isModelBlocked(status, body) {
		return &Error{Kind: KindModelBlocked, Status: status, Msg: body}
	}
	// 1) 余额耗尽（明确的 402）。
	if status == http.StatusPaymentRequired {
		return &Error{Kind: KindHardCredit, Status: status, Msg: body}
	}
	// 2) 会话失效。
	if containsAny(lower, sessionDeadMarkers) {
		return &Error{Kind: KindSessionDead, Status: status, Msg: body}
	}
	// 3) 账号级故障（注意：不能按 11140 判定——该 code 也承载模型级限流文案）。
	if containsAny(lower, accountFaultMarkers) {
		return &Error{Kind: KindAccountFault, Status: status, Msg: body}
	}
	// 4) 429 + 业务码 14018 表示余额真的耗尽。
	if status == http.StatusTooManyRequests && hasBusinessCode(body, "14018") {
		return &Error{Kind: KindHardCredit, Status: status, Msg: body}
	}
	// 5) 普通限流。
	if status == http.StatusTooManyRequests {
		return &Error{Kind: KindSoftRate, Status: status, Msg: body}
	}
	// 6) 余额关键词（在 429 之后）。
	if containsAny(lower, hardMarkers) {
		return &Error{Kind: KindHardCredit, Status: status, Msg: body}
	}
	// 7) 限流关键词。
	if containsAny(lower, softRateMarkers) {
		return &Error{Kind: KindSoftRate, Status: status, Msg: body}
	}
	// 8) 上下文超限（请求级）。
	if (status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusRequestEntityTooLarge) &&
		containsAny(body, promptTooLongMarkers) {
		return &Error{Kind: KindPromptTooLong, Status: status, Msg: body}
	}
	// 9) 偶发 404。
	if status == http.StatusNotFound {
		return &Error{Kind: KindNotFound, Status: status, Msg: body}
	}
	// 10) 上游故障。
	if status >= 500 {
		return &Error{Kind: KindServer, Status: status, Msg: body}
	}
	// 11) WAF 拦截：403 且没有业务信封（说明不是上游业务返回的）。
	if IsWAFBlocked(status, body) {
		return &Error{Kind: KindWAFBlock, Status: status, Msg: body}
	}
	// 12) 图片无效。
	if status == http.StatusBadRequest && (codeMarker(lower, "11135") || containsAny(lower, invalidImageMarkers)) {
		return &Error{Kind: KindImageInvalid, Status: status, Msg: body}
	}
	// 13) 内容策略拦截。
	if status >= 400 && containsAny(lower, contentBlockedMarkers) {
		return &Error{Kind: KindContentBlocked, Status: status, Msg: body}
	}
	// 14) 参数非法。
	if status >= 400 && (strings.Contains(lower, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode)) {
		return &Error{Kind: KindBadParams, Status: status, Msg: body}
	}
	// 15) 其他 4xx。
	if status >= 400 {
		return &Error{Kind: KindClient, Status: status, Msg: body}
	}
	return &Error{Kind: KindNone, Status: status, Msg: body}
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

// containsAny 报告 text 是否包含任一 marker（text 应已小写）。
func containsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
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
