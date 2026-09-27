package cline

// 本文件实现 Cline 的上游错误分类。
//
// 分类的意义不在于「记录得好看」，而在于**决定宿主该怎么处置这个账号**：
//   - 凭证失效（401/403）→ 让宿主把账号标记为坏，停止路由；
//   - 限流（429）→ 保留账号，稍后重试（Cline 的 429 会带恢复时间文案）；
//   - 上游抖动（5xx / 网络）→ 保留账号，稍后重试；
//   - 请求本身有问题（400/422）→ 保留账号（是这次请求的问题，不是账号的）。

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	// RetryAfter 是上游给出的重试时刻（429 带文案时解析，否则零值）。
	RetryAfter time.Time
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
		return fmt.Sprintf("cline upstream %d: %s", e.Status, e.Msg)
	}
	return "cline upstream: " + e.Msg
}

// cooldownPattern 从 429 的响应体里解析恢复时间。
//
// 上游的文案形如 "Try again in 1h 1m" / "Try again in 30s"。
var cooldownPattern = regexp.MustCompile(`(?i)try again in\s+((?:\d+[hms]\s*)+)`)

// cooldownPartPattern 解析时长片段（1h / 30m / 45s）。
var cooldownPartPattern = regexp.MustCompile(`(?i)(\d+)\s*([hms])`)

// Classify 按 HTTP 状态码与响应体分类一个上游错误。
func Classify(status int, body string) *Error {
	err := &Error{
		Status: status,
		Msg:    truncateForMessage(body, 300),
		Kind:   kindForStatus(status),
	}
	if status == http.StatusTooManyRequests {
		err.RetryAfter = parseCooldown(body)
	}
	return err
}

// kindForStatus 把状态码映射成处置分类。
func kindForStatus(status int) ErrorKind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
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

// parseCooldown 从 429 文案里解析恢复时刻；解析不出返回零值。
//
// 兜底不在这里做：调用方按「零值 = 未知」处理即可，硬编一个 1 小时
// 会让上游明明只要求等 30 秒时也白等一小时。
func parseCooldown(body string) time.Time {
	matches := cooldownPattern.FindStringSubmatch(body)
	if len(matches) < 2 {
		return time.Time{}
	}
	var total time.Duration
	for _, part := range cooldownPartPattern.FindAllStringSubmatch(matches[1], -1) {
		value, errParse := strconv.Atoi(part[1])
		if errParse != nil {
			continue
		}
		switch strings.ToLower(part[2]) {
		case "h":
			total += time.Duration(value) * time.Hour
		case "m":
			total += time.Duration(value) * time.Minute
		case "s":
			total += time.Duration(value) * time.Second
		}
	}
	if total <= 0 {
		return time.Time{}
	}
	return time.Now().Add(total)
}

// IsCredentialError 报告错误是否表示凭证失效。
func IsCredentialError(err error) bool {
	upstreamErr, okUpstream := err.(*Error)
	return okUpstream && upstreamErr.Kind == KindCredential
}

// IsRateLimit 报告错误是否为限流。
func IsRateLimit(err error) bool {
	upstreamErr, okUpstream := err.(*Error)
	return okUpstream && upstreamErr.Kind == KindRateLimit
}

// truncateForMessage 截断过长的上游响应，避免错误信息淹没日志。
func truncateForMessage(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "..."
}
