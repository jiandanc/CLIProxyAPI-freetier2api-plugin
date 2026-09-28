package zcode

// 常量定义：ZCode 供应商相关标识、端点与默认配置。
// 参考：D:\Workspace\zcode2api\app\constants.py

const (
	// VendorID 是本供应商在 freetier2api 插件内的唯一标识。
	VendorID = "zcode"
	// VendorName 是管理端与列表的展示名。
	VendorName = "ZCode"

	// 上游域名
	DefaultZCodeOrigin = "https://zcode.z.ai"
	DefaultAPIBase     = "https://api.z.ai"

	// 对话路径
	PathPlanMessages = "/api/v1/zcode-plan/anthropic/v1/messages"
	PathAPIMessages  = "/api/anthropic/v1/messages"

	// OAuth 路径
	PathOAuthInit = "/api/v1/oauth/cli/init"
	PathOAuthPoll = "/api/v1/oauth/cli/poll" // + /{flow_id}

	// 兑换 API Key 路径（api.z.ai）
	PathZLogin       = "/api/auth/z/login"
	PathCustomerInfo = "/api/biz/customer/getCustomerInfo"

	// 额度与签到领取路径（zcode.z.ai）
	PathBillingBalance = "/api/v1/zcode-plan/billing/balance"
	PathBillingCurrent = "/api/v1/zcode-plan/billing/current"
	PathBillingPreview = "/api/v1/zcode-plan/billing/preview"
	PathBillingClaim   = "/api/v1/zcode-plan/billing/claim"
	PathEventReport    = "/api/v1/event/report"

	// 客户端伪装标识（与官方 ZCode 桌面端保持一致）
	ClientAppVersion = "3.11.2"
	ClientUA         = "ZCode/" + ClientAppVersion
	AnthropicVersion = "2023-06-01"

	// 最大 tokens 限制（上游合法限制 131072）
	MaxTokensLimit = 131072
)
