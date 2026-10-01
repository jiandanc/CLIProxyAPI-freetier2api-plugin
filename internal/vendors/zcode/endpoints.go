package zcode

// 上游常量收口：URL、端点、客户端指纹与模型名。
//
// 约定与其它供应商一致：**禁止在别处硬编码上游常量**，一律引用本文件。
// 参考 zcode2api/app/constants.py（其自身又标注「值均来自源码实证」）。

const (
	// VendorID 是本供应商在 freetier2api 插件内的唯一标识。
	VendorID = "zcode"
	// VendorName 是管理端与列表的展示名。
	VendorName = "ZCode"

	// ── 上游 origin ─────────────────────────────────────────────────────────

	// ZCodeOrigin 承载 Plan 通道、计费族与 OAuth CLI。
	ZCodeOrigin = "https://zcode.z.ai"
	// ZAIOrigin 承载 API Key 通道与 API Key 兑换链。
	ZAIOrigin = "https://api.z.ai"

	// ── 对话端点 ────────────────────────────────────────────────────────────

	// PathPlanMessages 是 Plan 通道（Bearer JWT）。上游**强制**校验
	// X-Aliyun-Captcha-Verify-Param：没有真实浏览器求解器时该通道不可用，
	// 因此本插件默认走 PathAPIMessages。
	PathPlanMessages = "/api/v1/zcode-plan/anthropic/v1/messages"
	// PathAPIMessages 是 API Key 回退通道，免验证码。
	PathAPIMessages = "/api/anthropic/v1/messages"

	// ── OAuth CLI（headless，服务端中转）─────────────────────────────────────
	//
	// 从 origin 起算的完整路径（与 ZCodeOrigin 拼接后使用）。

	PathOAuthInit = "/api/v1/oauth/cli/init"
	PathOAuthPoll = "/api/v1/oauth/cli/poll" // + /{flow_id}

	// ── API Key 兑换链（api.z.ai）───────────────────────────────────────────

	PathZLogin       = "/api/auth/z/login"
	PathCustomerInfo = "/api/biz/customer/getCustomerInfo"

	// ── 计费 / 额度（zcode.z.ai）────────────────────────────────────────────

	// BillingBase 是计费族基准（current/balance/preview/claim）。
	BillingBase = ZCodeOrigin + "/api/v1/zcode-plan"

	PathBillingCurrent = "/billing/current"
	PathBillingBalance = "/billing/balance"
	PathUsage          = "/usage"
	PathClientConfigs  = "/api/v1/client/configs"

	// ── 客户端指纹 ──────────────────────────────────────────────────────────

	// ClientAppVersion 是官方桌面端现行版本。
	//
	// 上游按客户端版本判定兼容性（版本过旧会被拒），因此跟进官方升版时要
	// 同步改这里。
	ClientAppVersion = "3.14.3"

	// UserAgent 与 ClientAppVersion 同源，避免两者漂移。
	UserAgent = "ZCode/" + ClientAppVersion

	AnthropicVersion = "2023-06-01"
	ClientAgent      = "glm"
	HTTPReferer      = ZCodeOrigin + "/"
	ClientTitle      = "Z Code@electron"
	ReleaseChannel   = "stable"

	// DefaultPlatform/DefaultOSVersion/DefaultLanguage/DefaultTimezone 是
	// **无账号上下文**时的伪装值（模型探测、测试）。有账号时一律取该账号的
	// 设备档案——每账号一台独立设备，避免多账号聚成「同一台机器」。
	DefaultPlatform  = "darwin-arm64"
	DefaultOSVersion = "25.5.0"
	DefaultLanguage  = "zh-CN"
	DefaultTimezone  = "Asia/Shanghai"

	// ── 限制 ────────────────────────────────────────────────────────────────

	// MaxTokensLimit 是上游 max_tokens 的合法上限（超限报 400 code 1210）。
	MaxTokensLimit = 131072

	// DefaultMaxTokens 是请求未指定 max_tokens 时的取值。
	DefaultMaxTokens = 8192

	// ── 验证码头 ────────────────────────────────────────────────────────────

	// CaptchaHeader 是 Plan 通道的验证令牌；REGION 与它成对下发，缺失易 3007。
	CaptchaHeader       = "X-Aliyun-Captcha-Verify-Param"
	CaptchaRegionHeader = "X-Aliyun-Captcha-Verify-Region"
)
