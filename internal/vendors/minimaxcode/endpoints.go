package minimaxcode

import (
	"strings"
)

// Region 是 MiniMax Code 服务部署区域。
type Region string

const (
	RegionCN     Region = "cn"
	RegionGlobal Region = "global"

	VendorIDCN       = "minimaxcodecn"
	VendorNameCN     = "MiniMax Code 国内版"
	VendorIDGlobal   = "minimaxcodeglobal"
	VendorNameGlobal = "MiniMax Code 国际版"

	// 国内版 (CN)
	AccountHostCN = "https://account.minimax.cn"
	AgentHostCN   = "https://agent.minimaxi.com"
	StreamHostCN  = "https://agent-stream.minimaxi.com"

	// 国际版 (Global)
	AccountHostGlobal = "https://account.minimax.io"
	AgentHostGlobal   = "https://agent.minimax.io"
	StreamHostGlobal  = "https://agent-stream.minimax.io"

	// OAuth2 设备码配置（与桌面客户端对齐）
	ClientID         = "mcode-public"
	OAuthScope       = "agent.default"
	OAuthAudience    = "agent-backend"
	DeviceGrantType  = "urn:ietf:params:oauth:grant-type:device_code"
	RefreshGrantType = "refresh_token"

	// 官方签名协议盐值与后缀
	SignatureSalt   = "I*7Cf%WZ#S&%1RlZJ&C2"
	SignatureSuffix = "ooui"

	// 客户端 UA 指纹
	DesktopUA = "MiniMaxAgent Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) MiniMaxAgent/3.0.73 Chrome/148.0.7778.280 Safari/537.36"
	WebUA     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

	// 端点路径
	EpDeviceCode    = "/oauth2/device/code"
	EpToken         = "/oauth2/token"
	EpUserInfo      = "/v1/api/user/info"
	EpConfig        = "/minimax-cloud/api/v1/config"
	EpAgentList     = "/minimax-cloud/api/v1/agent"
	EpConnections   = "/minimax-cloud/api/v1/channel/connections"
	EpSession       = "/minimax-cloud/api/v1/agent/%s/session"
	EpMessage       = "/minimax-cloud/api/v1/session/%s/message"
	EpCheckinStatus = "/minimax-cloud/api/v1/signin/status"
	EpCheckinClaim  = "/minimax-cloud/api/v1/signin/claim"
	EpMembership    = "/matrix/api/v1/commerce/get_membership_info"
	EpCreditDetails = "/minimax-cloud/api/v1/credit/details"
)

// VendorIDFor 返回某区域对应的 VendorID。
func VendorIDFor(r Region) string {
	if r == RegionGlobal {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// VendorNameFor 返回某区域对应的展示名。
func VendorNameFor(r Region) string {
	if r == RegionGlobal {
		return VendorNameGlobal
	}
	return VendorNameCN
}

// AccountHostFor 返回账号服务基础地址。
func AccountHostFor(r Region) string {
	if r == RegionCN {
		return AccountHostCN
	}
	return AccountHostGlobal
}

// AgentHostFor 返回 Agent 业务基础地址。
func AgentHostFor(r Region, override string) string {
	if trimmed := strings.TrimRight(strings.TrimSpace(override), "/"); trimmed != "" {
		return trimmed
	}
	if r == RegionCN {
		return AgentHostCN
	}
	return AgentHostGlobal
}

// StreamHostFor 返回流式对话消息基础地址。
func StreamHostFor(r Region, override string) string {
	base := AgentHostFor(r, override)
	// 如果是 agent.xxx 转换为 agent-stream.xxx
	if strings.Contains(base, "://agent.") {
		return strings.Replace(base, "://agent.", "://agent-stream.", 1)
	}
	if r == RegionCN {
		return StreamHostCN
	}
	return StreamHostGlobal
}
