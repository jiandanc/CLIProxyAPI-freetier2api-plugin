package zcode

// 错误分类：解析上游 HTTP 状态码与响应体。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type ErrorKind string

const (
	KindCredential ErrorKind = "credential"
	KindRateLimit  ErrorKind = "rate_limit"
	KindRisk       ErrorKind = "risk"
	KindTransient  ErrorKind = "transient"
	KindClient     ErrorKind = "client"
)

type Error struct {
	Kind    ErrorKind `json:"kind"`
	Status  int       `json:"status"`
	Code    int       `json:"code,omitempty"`
	Message string    `json:"message"`
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("zcode upstream error [%d/%d]: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("zcode upstream error [%d]: %s", e.Status, e.Message)
}

func Classify(status int, body string) error {
	var payload struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := firstNonEmpty(payload.Msg, payload.Error.Message, body)

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if strings.Contains(body, "3007") || strings.Contains(strings.ToLower(msg), "captcha") {
			return &Error{Kind: KindRisk, Status: status, Code: payload.Code, Message: "触发人机验证码校验"}
		}
		return &Error{Kind: KindCredential, Status: status, Code: payload.Code, Message: msg}

	case status == http.StatusPaymentRequired:
		return &Error{Kind: KindCredential, Status: status, Code: payload.Code, Message: "额度不足或套餐已过期"}

	case status == http.StatusMethodNotAllowed && (strings.Contains(body, "3012") || strings.Contains(msg, "unusual activity")):
		return &Error{Kind: KindRisk, Status: status, Code: 3012, Message: "触发上游风控限制 (3012 unusual activity)"}

	case status == http.StatusTooManyRequests:
		return &Error{Kind: KindRateLimit, Status: status, Code: payload.Code, Message: msg}

	case status >= 500:
		return &Error{Kind: KindTransient, Status: status, Code: payload.Code, Message: msg}

	default:
		return &Error{Kind: KindClient, Status: status, Code: payload.Code, Message: msg}
	}
}
