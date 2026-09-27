package opencodezen

// 本文件定义 OpenCode ZEN 的上游端点与请求头。
//
// 端点来自上游网关项目的实测值（opencode2api 的 internal/config/config.go
// 与 internal/gateway/upstream.go）：base 是 https://opencode.ai/zen，
// 协议路径按模型的原生协议选择。
//
// **本插件只处理 chat-completions**（Claude / Codex 客户端由宿主翻译），
// 因此这里只保留 chat 路径。ZEN 上游还提供 /v1/responses、/v1/messages、
// /v1/systemone，那些由宿主侧的多协议翻译覆盖，插件不重复实现。

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
)

const (
	// DefaultBaseURL 是 ZEN 的上游基地址。
	DefaultBaseURL = "https://opencode.ai/zen"
	// ChatPath 是 chat-completions 的路径。
	ChatPath = "/v1/chat/completions"
	// ModelsPath 是模型清单的路径。
	ModelsPath = "/v1/models"

	// clientVersion 是伪装用的 OpenCode 客户端版本。
	//
	// 上游按客户端标识做分发，UA 与 x-opencode-client 必须成对出现：
	// 只改一个会被识别成非官方客户端。
	clientVersion = "1.18.31"
	// userAgentFormat 是上游客户端的 UA 形态（实测值）。
	userAgentFormat = "opencode/%s (%s %s; %s)"
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

// UserAgent 返回伪装成官方 OpenCode 客户端的 UA。
//
// 形态与上游网关实测值一致：opencode/<版本> (<os> <arch>; <go 版本>)。
func UserAgent() string {
	return fmt.Sprintf(userAgentFormat, clientVersion, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// ApplyChatHeaders 写入 chat-completions 请求所需的全部头。
//
// 认证与客户端标识是**硬门槛**：ZEN 上游按 x-opencode-client 与 UA 判定
// 请求来源，缺失会被当成未知客户端拒绝。
func ApplyChatHeaders(req *http.Request, apiKey, sessionID, requestID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("x-session-affinity", sessionID)
	req.Header.Set("X-Session-Id", sessionID)
	req.Header.Set("x-opencode-request", requestID)
	req.Header.Set("x-opencode-project", "global")
}

// ApplyModelsHeaders 写入模型清单请求的头（只需认证）。
func ApplyModelsHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("x-opencode-client", "cli")
}
