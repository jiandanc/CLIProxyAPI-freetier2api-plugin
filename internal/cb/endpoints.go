// Package cb 实现腾讯 CodeBuddy（WorkBuddy）的上游协议。
//
// 本包是纯逻辑层：不知道 CPA 宿存在，出站 HTTP 一律经 internal/httpx
// 走宿主的 HTTP 桥（这样宿主的代理、请求日志、账号级代理都生效）。
//
// 层次：
//
//	endpoints.go  域与端点表（cn / global 两套部署）
//	credential.go 凭证解析与写回（双形态兼容）
//	client.go     客户端与统一信封
//	errors.go     错误分类（决定凭证处置语义）
//	headers.go    四类出站请求头 + 会话头族
//	payload.go    请求体改写管线（OpenAI → CodeBuddy）
//	sanitize.go   出站请求体指纹脱敏
//	thinking.go   思维链注入与回填
//	toolpair.go   tool_call 配对重排与孤儿裁剪
//	sse.go        流式透传与聚合
//	catalog.go    模型目录探测
//	efforts.go    reasoning effort 档位表
package cb

import "strings"

// Region 是 CodeBuddy 的部署域。
//
// cn 与 global 是**两套独立部署**：域名、模型清单、reasoning 档位表都不同，
// 且**凭证不通用**——用 cn 的 token 打 global 域名会得到 401 TOKEN_EXPIRE。
type Region string

const (
	// RegionCN 是国内版（copilot.tencent.com / www.codebuddy.cn）。
	RegionCN Region = "cn"
	// RegionGlobal 是国际版（www.workbuddy.ai）。
	RegionGlobal Region = "global"
)

// NormalizeRegion 把字符串归一化为合法 Region，未知值回落 cn。
func NormalizeRegion(raw string) Region {
	if strings.EqualFold(strings.TrimSpace(raw), string(RegionGlobal)) {
		return RegionGlobal
	}
	return RegionCN
}

// IsGlobal 报告是否为国际版。
func (r Region) IsGlobal() bool { return r == RegionGlobal }

// String 实现 fmt.Stringer。
func (r Region) String() string { return string(r) }

// Endpoints 是某个域的全部上游端点。
//
// 所有 URL 集中成一张表、按 domain 取：散落在各处拼 URL 是维护灾难，
// 而且域切换（cn/global）必须成组发生，不能只换一半。
type Endpoints struct {
	// ChatBase 是对话域基址（/v2/chat/completions 在此之下）。
	ChatBase string
	// BillingBase 是计费与埋点域基址（/v2/report、/billing/meter/*）。
	BillingBase string
	// WebBase 是 Web 成长中心域基址（任务领奖 /activity/growth/tasks/<code>/claim）。
	WebBase string
	// Origin 是出站请求的 Origin / Referer 基址。
	Origin string
	// AcceptLanguage 是该域的默认语言偏好。
	AcceptLanguage string
	// PlatformLabel 是 UA 中的平台段（global 必须是 "WorkBuddy AI"，否则触发风控）。
	PlatformLabel string
}

var (
	endpointsCN = Endpoints{
		ChatBase:       "https://copilot.tencent.com",
		BillingBase:    "https://www.codebuddy.cn",
		WebBase:        "https://www.workbuddy.cn",
		Origin:         "https://www.codebuddy.cn",
		AcceptLanguage: "zh-CN",
		PlatformLabel:  "WorkBuddy",
	}
	endpointsGlobal = Endpoints{
		ChatBase:       "https://www.workbuddy.ai",
		BillingBase:    "https://www.workbuddy.ai",
		WebBase:        "https://www.workbuddy.ai",
		Origin:         "https://www.workbuddy.ai",
		AcceptLanguage: "en-US",
		PlatformLabel:  "WorkBuddy AI",
	}
)

// GetEndpoints 返回该域对应的端点表。
func GetEndpoints(region Region) Endpoints {
	if region.IsGlobal() {
		return endpointsGlobal
	}
	return endpointsCN
}

// 对话域路径。
const (
	// chatCompletionsPath 是唯一的对话出口。
	//
	// 历史：global 曾先打 /console/chat/completions，因 /console 挂腾讯云 WAF
	// 的 body 内容规则（反引号 + printf/whoami 等命令执行特征确定性 403）而改为固定 /v2。
	chatCompletionsPath = "/v2/chat/completions"
	// tokenRefreshPath 是凭证刷新端点（X-Refresh-Token 只允许出现在这里）。
	tokenRefreshPath = "/v2/plugin/auth/token/refresh"
	// v3ConfigPath 是官方 IDE 配置目录（模型能力的权威来源，UA 敏感）。
	v3ConfigPath = "/v3/config"
	// enterpriseModelsPathCN 是企业模型目录（CN 补缺路）。
	enterpriseModelsPathCN = "/console/enterprises/personal/models"
	// enterpriseModelsPathGlobal 是国际版企业模型目录首选路径。
	enterpriseModelsPathGlobal = "/v2/enterprises/personal/models"
)

// 计费域路径。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource"
	dailyCheckinPath   = "/billing/meter/daily-checkin"
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
	claimGiftPath      = "/billing/meter/claim-gift"
	claimCompPath      = "/billing/meter/claim-compensation"
	reportPath         = "/v2/report"
	trialPath          = "/billing/ide/trial"
)

// 成长 / 任务域路径（挂在 ChatBase 之下）。
const (
	growthTasksPath        = "/v2/activity/growth/tasks"
	growthTasksAcceptPath  = "/v2/activity/growth/tasks/accept"
	growthTasksClaimBase   = "/activity/growth/tasks"
	growthStreakPath       = "/activity/growth/streak"
	growthRedeemPath       = "/activity/growth/redeem"
	growthLotterySummary   = "/activity/growth/lottery/summary"
	growthLotteryDraw      = "/activity/growth/lottery/draw"
	growthHeatmapPath      = "/activity/growth/heatmap"
	growthMakeupCardsPath  = "/activity/growth/makeup-cards/use"
	buddyInfoPath          = "/activity/growth/buddy/info"
	buddyFirstPath         = "/activity/growth/buddy/first"
	buddyAgreementPath     = "/activity/growth/buddy/agreement"
	buddyTravelStatusPath  = "/activity/growth/buddy/travel/status"
	buddyTravelDepartPath  = "/activity/growth/buddy/travel/depart"
	buddyTravelClaimPath   = "/activity/growth/buddy/travel/claim"
	appearanceSetPath      = "/v2/user-asset/appearance/set"
	expertMarketListPath   = "/portal/operation-platform/market/expert/list"
	schoolBasePath         = "/portal/activity/school"
	globalCountryCodePath  = "/billing/area/get-country-code"
	globalSubmitRegionPath = "/console/login/account"
	globalRegisterPath     = "/auth/realms/copilot/overseas/user/register"
)

// 出站标识内置值。都允许被配置覆盖。
const (
	defaultClientVersion = "5.5.4"
	defaultCLIVersion    = "2.137.1"
	defaultClientName    = "WorkBuddy"
	// desktopClientVersion 是桌面指纹族使用的版本（与 CLI 族刻意不同）。
	desktopClientVersion = "5.5.6"
	// codeBuddyIDEUA / codeBuddyCLIUA 是模型目录探测专用 UA。
	//
	// /v3/config 对 UA 敏感：过旧会拿到精简目录或 400 code=12403。
	// 实测两路各有独有模型（IDE 含 o4-mini/enhance-1.0，CLI 含 deepseek 系），缺一不可。
	codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"
	codeBuddyCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"
	// chromeUA 是 web 指纹族使用的浏览器 UA。
	chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
)

// 以下常量对外导出，供 package main 的配置默认值使用。
const (
	// DefaultClientVersion 是 UA 中 WorkBuddy 段的默认版本。
	DefaultClientVersion = defaultClientVersion
	// DefaultCLIVersion 是 UA 中 CLI 段的默认版本。
	DefaultCLIVersion = defaultCLIVersion
	// DefaultClientName 是归属头的默认取值。
	DefaultClientName = defaultClientName
)
