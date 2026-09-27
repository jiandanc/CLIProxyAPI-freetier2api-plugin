package qoder

// 本文件实现 Qoder 的错误分类与插件错误构造。
//
// 分类结果决定宿主对凭证的处置：401 禁用账号、502 换号重试、503 稍后再试。
// 分类错会让「上游排队」被误判成「凭证失效」，把一个好账号标坏。

import (
	"net/http"
	"strings"

	"freetier2api-plugin/internal/vendors/qoder/bridge"
)

// PluginError 是携带 HTTP 状态码的插件错误。
//
// 与 core.PluginError 同形，但本包不 import core（core 的 Vendor 接口由根层
// 适配器实现，避免协议层依赖骨架层），因此这里独立定义并由适配器转换。
type PluginError struct {
	Code       string
	Message    string
	Retryable  bool
	HTTPStatus int
}

// Error 实现 error 接口。
func (e *PluginError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// NewError 构造一个 Qoder 插件错误。
func NewError(code, message string, status int) *PluginError {
	return &PluginError{Code: code, Message: message, HTTPStatus: status}
}

// ClassifyCredentialError 把上游错误分类成插件错误。
//
// 判定顺序有语义，不可重排：
//  1. 排队信号优先——上游排队时返回的错误文本里也可能出现 token 字样，
//     先判排队才不会把有效账号标成失效；
//  2. 凭证类（401/403/unauthorized/token expired）→ 401，让宿主禁用该账号；
//  3. 传输层瞬时错误 → 502，让宿主换号重试；
//  4. 其余 → 502（当作上游故障而不是账号问题）。
func ClassifyCredentialError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	lower := strings.ToLower(message)
	if _, queued := bridge.ParseQueueSignal(message); queued {
		return &PluginError{Code: "qoder_model_busy", Message: message, HTTPStatus: http.StatusServiceUnavailable}
	}
	switch {
	case strings.Contains(message, "HTTP 401"), strings.Contains(message, "HTTP 403"),
		strings.Contains(lower, "unauthorized"), strings.Contains(lower, "forbidden"),
		strings.Contains(lower, "invalid token"), strings.Contains(lower, "token expired"):
		return &PluginError{Code: "qoder_credential_invalid", Message: message, HTTPStatus: http.StatusUnauthorized}
	case bridge.IsTransientTransport(err):
		return &PluginError{Code: "qoder_upstream_transient", Message: message, HTTPStatus: http.StatusBadGateway, Retryable: true}
	default:
		return &PluginError{Code: "qoder_upstream_error", Message: message, HTTPStatus: http.StatusBadGateway}
	}
}

// classifyCredentialError 是 ClassifyCredentialError 的包内别名。
func classifyCredentialError(err error) error { return ClassifyCredentialError(err) }
