package tabbit

// 常量与端点定义：Tabbit 供应商配置。
// 参考：/Users/jiandan/Workspaces/tabbit2api

const (
	VendorID   = "tabbit"
	VendorName = "Tabbit"

	// 默认上游端点（支持通过凭证内的 base_url 或环境变量重写为本地网关 http://127.0.0.1:50124）
	DefaultBaseURL = "https://web.tabbit.ai/proxy/v1"
	DefaultLocalGW = "http://127.0.0.1:50124"

	EpChatCompletions = "/chat/completions"
	EpModels          = "/model_config/models?a=0"
	EpV1Models        = "/models"
)
