package core

import (
	"encoding/json"
	"fmt"
)

// envelope 是 CPA 插件 ABI 的 JSON 信封，与宿主 cpasdk/pluginabi.Envelope 对齐。
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

// envelopeError 携带可选 HTTP 状态码；宿主据此决定冷却或轮转当前凭证。
type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// PluginError 是插件内部的结构化错误：可携带 HTTP 状态码与可重试标记。
//
// HTTPStatus 是插件与宿主之间最重要的契约之一：宿主按它决定凭证处置
// （402 长冷却 / 429 短冷却 / 401 禁用 / 403 软冷却），见 conductor_cooldown.go。
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

// NewPluginError 构造一个带 HTTP 状态码的插件错误。
func NewPluginError(code, message string, status int) *PluginError {
	return &PluginError{Code: code, Message: message, HTTPStatus: status}
}

// RetryablePluginError 构造一个可重试的插件错误：宿主会换一个凭证重试同一请求。
func RetryablePluginError(code, message string, status int) *PluginError {
	return &PluginError{Code: code, Message: message, Retryable: true, HTTPStatus: status}
}

// OKEnvelope 把任意结果序列化为成功信封。
func OKEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal result: %w", errMarshal)
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// ErrorEnvelope 构造失败信封（无 HTTP 状态码，宿主按 500 处理）。
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// ErrorEnvelopeFromError 把业务错误转成信封并保留 HTTP 状态码。
//
// 对 *PluginError 用类型断言而非 errors.As：插件内部没有错误包装链，
// 断言既够用又能避免把包装过的错误误判成结构化错误。
func ErrorEnvelopeFromError(err error) []byte {
	if err == nil {
		return ErrorEnvelope("plugin_error", "unknown error")
	}
	if pluginErr, ok := err.(*PluginError); ok {
		raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
			Code:       pluginErr.Code,
			Message:    pluginErr.Message,
			Retryable:  pluginErr.Retryable,
			HTTPStatus: pluginErr.HTTPStatus,
		}})
		return raw
	}
	return ErrorEnvelope("plugin_error", err.Error())
}
