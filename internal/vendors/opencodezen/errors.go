package opencodezen

// 本文件实现 OpenCode ZEN 的上游错误分类。
//
// 分类的意义不在于「记录得好看」，而在于**决定宿主该怎么处置这个账号**：
//   - 凭证失效（401/403）→ 让宿主把账号标记为坏，停止路由；
//   - 限流（429）→ 保留账号，稍后重试；
//   - 上游抖动（5xx / 网络）→ 保留账号，稍后重试；
//   - 请求本身有问题（400/422）→ 保留账号（是这次请求的问题，不是账号的）。

import (
	"fmt"
	"net/http"
	"strings"
)

// Error 是一次上游调用的错误，带可处置的分类。
type Error struct {
	// Status 是 HTTP 状态码（0 表示网络层失败，没有响应）。
	Status int
	// Code 是业务错误码（上游给了才有）。
	Code string
	// Msg 是可读说明。
	Msg string
	// Kind 是处置分类。
	Kind ErrorKind
}

// ErrorKind 是错误处置分类。
type ErrorKind string

const (
	// KindCredential 表示凭证失效：宿主应停止使用该账号。
	KindCredential ErrorKind = "credential"
	// KindRateLimit 表示被限流：保留账号，稍后重试。
	KindRateLimit ErrorKind = "rate_limit"
	// KindTransient 表示上游抖动：保留账号，稍后重试。
	KindTransient ErrorKind = "transient"
	// KindClient 表示请求本身有问题：保留账号（不是账号的错）。
	KindClient ErrorKind = "client"
)

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Status > 0 {
		return fmt.Sprintf("opencodezen upstream %d: %s", e.Status, e.Msg)
	}
	return "opencodezen upstream: " + e.Msg
}

// Classify 按 HTTP 状态码与响应体分类一个上游错误。
func Classify(status int, body string) *Error {
	return &Error{
		Status: status,
		Msg:    truncateForMessage(body, 300),
		Kind:   kindForStatus(status),
	}
}

// kindForStatus 把状态码映射成处置分类。
func kindForStatus(status int) ErrorKind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// 403 在 ZEN 上也可能表示「免费层拒绝该请求形态」而非凭证失效，
		// 但那同样说明这把 key 在当前形态下不可用，交给宿主停用是合理的。
		return KindCredential
	case status == http.StatusTooManyRequests:
		return KindRateLimit
	case status >= 500:
		return KindTransient
	case status >= 400:
		return KindClient
	default:
		return KindTransient
	}
}

// IsCredentialError 报告错误是否表示凭证失效。
func IsCredentialError(err error) bool {
	upstreamErr, okUpstream := err.(*Error)
	return okUpstream && upstreamErr.Kind == KindCredential
}

// truncateForMessage 截断过长的上游响应，避免错误信息淹没日志。
func truncateForMessage(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "..."
}
