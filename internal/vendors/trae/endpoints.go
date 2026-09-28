package trae

// 常量与端点定义：Trae 国内版与国际版服务配置。
// 参考：/Users/jiandan/Workspaces/trae2api/src/realms.js

type Region string

const (
	RegionCN     Region = "cn"
	RegionGlobal Region = "global"

	VendorIDCN       = "traecn"
	VendorNameCN     = "Trae 国内版"
	VendorIDGlobal   = "traeglobal"
	VendorNameGlobal = "Trae 国际版"

	// 国内版 (CN)
	HostChatCN    = "https://trae-api-cn.mchost.guru"
	HostAuthCN    = "https://api.trae.cn"
	HostConsoleCN = "https://www.trae.cn"
	ClientIDCN    = "en1oxy7wnw8j9n"
	IDEVersionCN  = "3.3.67"

	// 国际版 (Global/SG)
	HostChatGlobal    = "https://coresg-normal.trae.ai"
	HostAuthGlobal    = "https://growsg-normal.trae.ai"
	HostConsoleGlobal = "https://www.trae.ai"
	ClientIDGlobal    = "ono9krqynydwx5"
	IDEVersionGlobal  = "3.5.51"

	// 公共端点路径
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"

	AppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
)

type Config struct {
	Region      Region
	ChatHost    string
	AuthHost    string
	ConsoleHost string
	ClientID    string
	IDEVersion  string
}

func ConfigFor(r Region) Config {
	if r == RegionGlobal {
		return Config{
			Region:      RegionGlobal,
			ChatHost:    HostChatGlobal,
			AuthHost:    HostAuthGlobal,
			ConsoleHost: HostConsoleGlobal,
			ClientID:    ClientIDGlobal,
			IDEVersion:  IDEVersionGlobal,
		}
	}
	return Config{
		Region:      RegionCN,
		ChatHost:    HostChatCN,
		AuthHost:    HostAuthCN,
		ConsoleHost: HostConsoleCN,
		ClientID:    ClientIDCN,
		IDEVersion:  IDEVersionCN,
	}
}
