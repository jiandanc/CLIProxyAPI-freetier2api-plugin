package codearts

// 错误分类：解析华为云 CodeArts 错误码。

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
	KindTransient  ErrorKind = "transient"
	KindClient     ErrorKind = "client"
)

type Error struct {
	Kind    ErrorKind `json:"kind"`
	Status  int       `json:"status"`
	Code    string    `json:"code,omitempty"`
	Message string    `json:"message"`
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("codearts upstream error [%d/%s]: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("codearts upstream error [%d]: %s", e.Status, e.Message)
}

func Classify(status int, body string) error {
	var payload struct {
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
		Error        string `json:"error"`
		Message      string `json:"message"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := firstNonEmpty(payload.ErrorMessage, payload.Message, payload.Error, body)
	code := firstNonEmpty(payload.ErrorCode, payload.Error)

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || strings.Contains(code, "ExpiredToken") || strings.Contains(code, "InvalidToken"):
		return &Error{Kind: KindCredential, Status: status, Code: code, Message: msg}

	case strings.Contains(body, "benefit not found") || strings.Contains(code, "InferHub.4004.200"):
		return &Error{Kind: KindCredential, Status: status, Code: code, Message: "未领取免费福利模型配额，请在管理端领取或开启自动领取"}

	case status == http.StatusTooManyRequests || strings.Contains(code, "Throttling"):
		return &Error{Kind: KindRateLimit, Status: status, Code: code, Message: msg}

	case status >= 500:
		return &Error{Kind: KindTransient, Status: status, Code: code, Message: msg}

	default:
		return &Error{Kind: KindClient, Status: status, Code: code, Message: msg}
	}
}
