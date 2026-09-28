package codearts

// 常量与端点定义：华为云 CodeArts Agent 配置。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/upstream/const.go

const (
	VendorID   = "codearts"
	VendorName = "CodeArts"

	// 华为云 CodeArts API 网关
	SnapEngineApiHost = "https://snap-access.cn-north-4.myhuaweicloud.com"
	SnapManagerHost   = SnapEngineApiHost + "/snap-manager"
	PortalHost        = "https://codearts.huaweicloud.com/portal"
	BenefitHost       = "https://opengw.developer.huaweicloud.com"

	ClientID = "codearts-agent"

	// 端点路径
	EpChatV2        = "/api/v2/chat/completions"
	EpModelBuiltin  = "/v1/model/builtin"
	EpBenefitConfig = "/api/v1/gateway/config"
	EpBenefitClaim  = "/api/v1/benefit/claim"
	EpLoginTicket   = "/v1/login/ticket"
	EpOAuthTokens   = "/v1/oauth2/tokens"
)
