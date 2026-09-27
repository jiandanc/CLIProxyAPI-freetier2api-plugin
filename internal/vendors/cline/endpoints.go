package cline

// 本文件定义 Cline 的上游端点与请求头。
//
// 端点与请求头形态移植自 cline2api 的实测值（auth.go 与 admin.go）：
// Cline 上游是标准 OpenAI chat-completions，但带一整套客户端标识头，
// 缺了会被识别成非官方客户端。

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL 是 Cline 的上游基地址。
	DefaultBaseURL = "https://api.cline.bot/api/v1"
	// ChatPath 是 chat-completions 的路径。
	ChatPath = "/chat/completions"
	// ModelsPath 是推荐模型清单的路径（**无需认证**）。
	ModelsPath = "/ai/cline/recommended-models"
	// RefreshPath 是令牌续期的路径。
	RefreshPath = "/auth/refresh"
	// RegisterPath 是用 WorkOS 令牌换 Cline 令牌的路径。
	RegisterPath = "/auth/register"

	// DefaultClientVersion 是伪装用的 Cline 客户端版本。
	DefaultClientVersion = "3.0.47"
	// DefaultCoreVersion 是伪装用的 Cline 内核版本。
	DefaultCoreVersion = "0.0.66"
)

// BaseURL 返回上游基地址（末尾无斜杠）。
//
// 允许配置覆盖：上游换域名或用户要走自建网关时不必改代码。
func BaseURL(override string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return DefaultBaseURL
	}
	return trimmed
}

// UserAgent 返回伪装成官方 Cline 客户端的 UA。
func UserAgent() string {
	return "Cline/" + DefaultClientVersion
}

// ApplyChatHeaders 写入 chat-completions 请求所需的全部头。
//
// 除认证外的这一整套 X-CLIENT-* / X-PLATFORM 头是「更像官方客户端」的
// 伪装层。实测上游只硬要求 Authorization 与 Content-Type，但少这些头
// 会更容易被风控盯上，因此照官方客户端的形态补齐。
func ApplyChatHeaders(req *http.Request, bearerToken, sessionID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("HTTP-Referer", "https://cline.bot")
	req.Header.Set("X-Title", "Cline")
	req.Header.Set("X-Task-ID", sessionID)
	req.Header.Set("X-IS-MULTIROOT", "false")
	req.Header.Set("X-CLIENT-TYPE", "cline-cli")
	req.Header.Set("X-CLIENT-VERSION", DefaultClientVersion)
	req.Header.Set("X-PLATFORM", "terminal")
	req.Header.Set("X-PLATFORM-VERSION", DefaultClientVersion)
	req.Header.Set("X-CORE-VERSION", DefaultCoreVersion)
}

// ApplyJSONHeaders 写入普通 JSON 请求的头（续期、注册）。
//
// 登录与续期链路**不带**客户端伪装头：实测上游的 /auth/* 只认 Content-Type，
// 多余的伪装头反而与官方客户端的行为不一致。
func ApplyJSONHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
}

// ApplyFormHeaders 写入表单请求的头（WorkOS 设备码流程）。
func ApplyFormHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
}

// NewSessionID 生成一个会话 ID（上游要求 sess_<毫秒时间戳> 形态）。
func NewSessionID() string {
	return "sess_" + strconv.FormatInt(time.Now().UnixMilli(), 10)
}
