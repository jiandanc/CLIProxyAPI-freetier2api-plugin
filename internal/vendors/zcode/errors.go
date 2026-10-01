package zcode

// 上游错误的分类与映射。
//
// 这里**不定义**本供应商自己的错误类型：根层的 errorToPluginError（protocol.go）
// 只认 *workbuddy.Error 这一套共享词汇表，各家自建的类型会被统一降级成
// 「502 可重试」——连 401 凭证失效都传不出去，宿主因此不会停用坏账号。
// 因此本文件只做一件事：把 ZCode 的上游响应翻译成 workbuddy.Kind。

import (
	"encoding/json"
	"net/http"
	"strings"

	"freetier2api-plugin/internal/vendors/workbuddy"
)

// 上游业务码（实测自 zcode.z.ai 与 api.z.ai）。
const (
	codeSuccess         = 0
	codeParameterError  = 3001 // 缺 X-Device-Mid 等必需参数
	codeSessionExpired  = 3004 // OAuth 轮询会话过期
	codeModelNotAllowed = 3006 // 该账号套餐不含此模型
	codeCaptchaFailed   = 3007 // 验证码校验失败（Plan 通道）
	codeDailyQuota      = 1005 // 每日额度用完（额度耗尽的判定依据之一）
	codeMaxTokens       = 1210 // max_tokens 超出 [1,131072]
	codeUnusualActivity = 3012 // 风控「unusual activity」
)

// Classify 把上游响应翻译成共享的上游错误类型。
//
// status 传 0 表示「HTTP 层无错误信号」（上游以 200 包装业务错误）；此时
// HTTPStatus 由 Kind 推导，避免把 200 当成错误的 HTTP 码透传给宿主。
//
// 分类顺序即优先级：验证码挑战必须先于鉴权判定——上游把 403 同时用于
// 「凭证失效」与「人机校验」，先判 403 会把只是要过码的账号错杀成失效。
func Classify(status int, body string) *workbuddy.Error {
	var payload struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := firstNonEmpty(payload.Msg, payload.Error.Message, textPreview(body))
	lower := strings.ToLower(body)

	switch {
	// 1) 验证码挑战（Plan 通道）：403 + captcha 文案，或 400 + code=3007
	case isCaptchaChallenge(status, payload.Code, lower):
		return &workbuddy.Error{
			Kind:   workbuddy.KindWAFBlock,
			Status: status,
			Msg:    "触发上游人机验证码校验。该验证码为阿里云无痕验证（无图片可作答），本插件未内置浏览器求解器——对话请改用 API Key 通道，领取活动套餐需在官方客户端完成",
		}

	// 2) 风控：405 + 3012 unusual activity（高频请求触发，属临时限制）
	case isRiskControl(status, payload.Code, lower):
		return &workbuddy.Error{
			Kind:   workbuddy.KindWAFBlock,
			Status: status,
			Msg:    "触发上游风控限制（3012 unusual activity）",
		}

	// 3) 鉴权失败：凭证失效，宿主应停用该账号
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &workbuddy.Error{
			Kind:   workbuddy.KindSessionDead,
			Status: status,
			Msg:    firstNonEmpty(msg, "凭证失效"),
		}

	// 4) 额度耗尽
	case status == http.StatusPaymentRequired || promptExhausted(lower, payload.Code):
		return &workbuddy.Error{
			Kind:   workbuddy.KindHardCredit,
			Status: pickStatus(status, http.StatusPaymentRequired),
			Msg:    firstNonEmpty(msg, "额度已用完"),
		}

	// 5) 限流
	case status == http.StatusTooManyRequests:
		return &workbuddy.Error{
			Kind:   workbuddy.KindSoftRate,
			Status: status,
			Msg:    firstNonEmpty(msg, "上游限流"),
		}

	// 6) 该后端无此模型（按账号×模型避让）
	case payload.Code == codeModelNotAllowed:
		return &workbuddy.Error{
			Kind:   workbuddy.KindModelBlocked,
			Status: status,
			Msg:    firstNonEmpty(msg, "该账号套餐不包含此模型"),
		}

	// 7) 参数非法（max_tokens 越界等）：换号无意义
	case payload.Code == codeMaxTokens || payload.Code == codeParameterError:
		return &workbuddy.Error{
			Kind:   workbuddy.KindBadParams,
			Status: status,
			Msg:    firstNonEmpty(msg, "请求参数被上游拒绝"),
		}

	// 8) 上游 5xx
	case status >= 500:
		return &workbuddy.Error{
			Kind:   workbuddy.KindServer,
			Status: status,
			Msg:    msg,
		}

	default:
		return &workbuddy.Error{
			Kind:   workbuddy.KindClient,
			Status: status,
			Msg:    msg,
		}
	}
}

// pickStatus 在 HTTP 层无错误信号（status=0）时用兜底状态码。
//
// 上游的 HTTP 状态码通常就是对的，优先透传；但业务错误可能以 HTTP 200
// 承载，此时必须换成语义正确的码——把 200 当错误的 HTTP 码透传会让宿主
// 判不出该冷却还是该停用。
func pickStatus(status, fallback int) int {
	if status >= 400 {
		return status
	}
	return fallback
}

// isCaptchaChallenge 判断响应是否为验证码挑战。
//
// 上游两种形态：403 + captcha/verify 文案（或挑战头），400 + code=3007。
func isCaptchaChallenge(status, code int, lowerBody string) bool {
	if code == codeCaptchaFailed {
		return true
	}
	if status != http.StatusForbidden && status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(lowerBody, "captcha") || strings.Contains(lowerBody, "verify")
}

// isRiskControl 判断响应是否为风控拦截（3012 unusual activity）。
func isRiskControl(status, code int, lowerBody string) bool {
	if code == codeUnusualActivity {
		return true
	}
	return status == http.StatusMethodNotAllowed && strings.Contains(lowerBody, "unusual activity")
}

// promptExhausted 判断正文是否表达「额度耗尽」。
//
// 上游在不同租户/区域的文案不一致，中英双通道比对。
func promptExhausted(lowerBody string, code int) bool {
	if code == codeDailyQuota {
		return true
	}
	for _, marker := range []string{
		"insufficient", "quota", "balance", "exhaust",
		"额度", "余额不足", "积分不足",
	} {
		if strings.Contains(lowerBody, marker) {
			return true
		}
	}
	return false
}

// textPreview 截断正文用于错误消息，避免把整页响应塞进日志。
func textPreview(text string) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 300 {
		return trimmed[:300]
	}
	return trimmed
}
