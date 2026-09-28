package tabbit

// 错误分类：解析 Tabbit 上游错误响应。

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
	Code    int       `json:"code,omitempty"`
	Message string    `json:"message"`
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("tabbit upstream error [%d/%d]: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("tabbit upstream error [%d]: %s", e.Status, e.Message)
}

func Classify(status int, body string) error {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := firstNonEmpty(payload.Error.Message, payload.Message, body)

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &Error{Kind: KindCredential, Status: status, Message: msg}

	case status == http.StatusTooManyRequests || strings.Contains(body, "429") || strings.Contains(body, "[492]"):
		return &Error{Kind: KindRateLimit, Status: status, Message: msg}

	case status >= 500:
		return &Error{Kind: KindTransient, Status: status, Message: msg}

	default:
		return &Error{Kind: KindClient, Status: status, Message: msg}
	}
}
