package workbuddy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"

	_ "embed"
	"regexp"
	"sync"
)

// 本文件是 CodeBuddy 协议的客户端核心：请求发送、统一信封处理、凭证刷新、
// 设备指纹派生与出站 HTTP 客户端构造。
//
// 所有出站请求都经宿主 HTTP 桥（internal/hostbridge），因此宿主的代理、
// 账号级代理与请求日志对这些请求同样生效。

const (
	// shortTimeout 是短 RPC（刷新、签到、余额、模型清单、任务上报）的总时长上限。
	shortTimeout = 120 * time.Second
	// chatHeaderTimeout 是对话首字节前的等待上限。
	chatHeaderTimeout = 120 * time.Second
	// refreshTimeout 是凭证刷新请求的上限。
	refreshTimeout = 30 * time.Second
	// apiBodyLimit 是短 RPC 响应体的读取上限。
	apiBodyLimit = 1 << 20
	// configBodyLimit 是 /v3/config 的读取上限（响应体明显更大）。
	configBodyLimit = 2 << 20

	// deriveSaltPrefix 是设备指纹派生的固定盐前缀。
	//
	// 固定前缀保证指纹跨重启稳定（换了就是新设备）；真正的部署隔离
	// 由 installSalt（机器盐）提供：不同部署的指纹空间互不相同。
	deriveSaltPrefix = "wb2a:"

	// authRefreshSource 是刷新请求的 X-Auth-Refresh-Source 取值。
	authRefreshSource = "plugin"

	// maxRefreshTokenExpiresIn 是刷新响应里 expiresIn 的合理上限。
	// 超过它视为脏数据，保留原有的 expiresAt 而不是写成一个离谱的未来时刻。
	maxRefreshTokenExpiresIn = int64(10 * 365 * 24 * 60 * 60)

	// deviceTokenFileTTL 是 device token 文件的读取缓存时长。
	deviceTokenFileTTL = 5 * time.Minute
	// deviceTokenFileMaxLen 是 device token 文件的长度上限（超过视为异常）。
	deviceTokenFileMaxLen = 1024
)

// Options 是构造 Client 所需的全部输入。
//
// 刻意用一个结构体而不是长参数列表：字段会随上游变化持续增加，
// 位置参数很快就会不可读。
type Options struct {
	// Context 绑定宿主回调 ID，使出站请求进宿主的请求日志。
	Context context.Context
	// EnabledRealms 是启用的域（cn / global 子集）。
	EnabledRealms []string
	// PromptMode / PromptText 控制系统提示词处理。
	PromptMode string
	PromptText string
	// OnContentBlocked 是内容审核拦截时的回调（触发提示词降级）。
	OnContentBlocked func()
	// SanitizeFingerprints 开启出站请求体黑名单脱敏。
	SanitizeFingerprints bool
	// PassthroughIP 决定是否把客户端 IP 透传给上游。
	PassthroughIP bool
	// UserAgent 显式覆盖出站 UA。
	UserAgent string
	// ClientVersion / CLIVersion 覆盖 UA 的两段版本号。
	ClientVersion string
	CLIVersion    string
	// ClientName 覆盖归属头取值。
	ClientName string
	// DeviceToken / DeviceTokenFile 是设备令牌的两级兜底。
	DeviceToken     string
	DeviceTokenFile string
	// StateDir 用于存放模型清单缓存。
	StateDir string
}

// Client 是 CodeBuddy 上游客户端。
//
// 每个请求构造一个实例：它很轻（只是配置快照 + 少许缓存），
// 但必须绑定请求级 context 才能让出站流量进宿主的请求日志。
type Client struct {
	rt   *runtime
	opts Options
}

// NewClient 构造一个上游客户端。
func NewClient(rt *runtime, opts Options) *Client {
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	return &Client{rt: rt, opts: opts}
}

// enabledRealm 报告某个域是否被配置启用。
//
// 关闭的域上的账号一律不发起上游请求：判断放在客户端层而不是散落各处，
// 保证「关掉 global」是真的一个请求都不发。
func (c *Client) enabledRealm(region Region) bool {
	if len(c.opts.EnabledRealms) == 0 {
		return true
	}
	for _, item := range c.opts.EnabledRealms {
		if item == string(region) {
			return true
		}
	}
	return false
}

// 三档出站 HTTP 客户端。

// shortClient 返回短 RPC 客户端。
func (c *Client) shortClient() *http.Client {
	return hostbridge.Client(c.opts.Context, shortTimeout)
}

// refreshClient 返回凭证刷新客户端。
func (c *Client) refreshClient() *http.Client {
	return hostbridge.Client(c.opts.Context, refreshTimeout)
}

// chatClient 返回对话客户端。
//
// 总时长设为 0：流式对话的时长不可预期，首字节与空闲分别由
// headerTimeout 与流内空闲监控负责。用总超时会误杀长回答。
func (c *Client) chatClient(headerTimeout time.Duration) *http.Client {
	if headerTimeout <= 0 {
		headerTimeout = chatHeaderTimeout
	}
	// 宿主桥不支持 ResponseHeaderTimeout（由宿主侧决定），
	// 这里用 context 超时表达首字节上限：httpx 的 transport 会把
	// 请求 context 传给 host.http.do_stream。
	return hostbridge.StreamClient(c.opts.Context, 0)
}

// apiEnvelope 是上游的统一响应信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发起一次短 RPC 并解开统一信封。
//
// 错误处理：HTTP ≥400 走 Classify；HTTP 成功但业务 code 非 0 也走 Classify
// （把 msg 当正文）。返回的是信封里的 data 部分。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request %s: %w", req.URL.Path, errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
	if errRead != nil {
		// 半截 body 不进 Classify：网络问题不该被当成账号问题罚号。
		return nil, fmt.Errorf("read %s response: %w", req.URL.Path, errRead)
	}
	return unwrapEnvelope(resp.StatusCode, string(body), resp.Header)
}

// doJSONWithLimit 与 doJSON 相同，但使用更大的读取上限（/v3/config 需要）。
func (c *Client) doJSONWithLimit(req *http.Request, limit int64) (json.RawMessage, error) {
	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request %s: %w", req.URL.Path, errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, limit))
	if errRead != nil {
		return nil, fmt.Errorf("read %s response: %w", req.URL.Path, errRead)
	}
	return unwrapEnvelope(resp.StatusCode, string(body), resp.Header)
}

// unwrapEnvelope 把「状态码 + 正文」解成 data 或分类错误。
func unwrapEnvelope(status int, body string, header http.Header) (json.RawMessage, error) {
	if status >= 400 {
		return nil, enrichClassifyError(status, body, header)
	}
	var envelope apiEnvelope
	if errUnmarshal := json.Unmarshal([]byte(body), &envelope); errUnmarshal != nil {
		// 响应不是 JSON：说明上游返回了非预期内容（HTML 错误页等）。
		return nil, &core.Failure{Kind: core.FailureKindClient, Status: status, Msg: core.Truncate(body, 512)}
	}
	if envelope.Code != 0 {
		return nil, enrichClassifyError(status, body, header)
	}
	return envelope.Data, nil
}

// enrichClassifyError 在分类结果上补齐上游给出的等待信息。
//
// 上游有两种表达等待的方式：响应头（Retry-After 系列）与正文文案（"将在 X 重置"）。
// 两者都解析出来交给调用方，由它决定冷却到哪个时刻。
func enrichClassifyError(status int, body string, header http.Header) *core.Failure {
	classified := Classify(status, body)
	classified.RetryAfter = ParseRetryAfter(header, time.Now())
	classified.ResetAt = ParseRateReset(body)
	if classified.Kind == core.FailureKindChannelRejected {
		// 渠道校验是上游新增的策略拦截，插件侧没有（也不该有）伪造官方客户端证明的手段。
		// 返回给用户的消息已折叠成人话，日志里保留全量正文：requestId 是反馈上游、
		// 以及日后比对协议差异时唯一的证据，折叠后就只剩这一份。
		logger.Error("upstream channel verification rejected the request (status=%d): %s",
			status, core.Truncate(body, 1024))
	}
	return classified
}

// refreshTokenResponse 是刷新端点的响应。
type refreshTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// DeviceToken 按「账号凭证 > 插件配置 > 文件」的优先级解析设备令牌。
func (c *Client) DeviceToken(cred *Credential) string {
	if token := cred.DeviceTokenValue(); token != "" {
		return token
	}
	if token := strings.TrimSpace(c.opts.DeviceToken); token != "" {
		return token
	}
	return c.rt.readDeviceTokenFile(c.opts.DeviceTokenFile)
}

// deriveIDWithSalt 派生一个跨重启稳定、账号间互异的标识。
func deriveIDWithSalt(uid, purpose, salt string) string {
	seed := deriveSaltPrefix + purpose + ":" + uid + ":" + salt
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:18])
}

// buildCacheKey 构造 prompt_cache_key。
//
// 这是单项最大的费用优化：同一段长前缀命中缓存后费用可降一个数量级。
// uid 前 8 位是**硬隔离因子**——跨账号复用同一 cache key 会让上游
// 命中错账号的前缀缓存，泄露对方对话内容。
func buildCacheKey(uid, conversation string) string {
	uidPrefix := uid
	if len(uidPrefix) > 8 {
		uidPrefix = uidPrefix[:8]
	}
	if uidPrefix == "" {
		uidPrefix = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	return "wb2a-" + uidPrefix + "-" + hex.EncodeToString(sum[:16])
}

// closeBody 关闭响应体并忽略错误（响应体关闭失败不影响业务结果）。
func closeBody(body io.Closer) {
	if body != nil {
		_ = body.Close()
	}
}

// 本文件实现对话调用：请求构造、出站、错误分类与流式读取。

// ChatRequest 是一次对话调用的输入。
type ChatRequest struct {
	// Credential 是被选中的账号凭证。
	Credential *Credential
	// Body 是客户端原始请求体（OpenAI 形态）。
	Body []byte
	// Model 是上游模型名（已剥离 realm 前缀）。
	Model string
	// ClientIP 是客户端 IP（仅在 PassthroughIP 开启时才会上送）。
	ClientIP string
	// Meta 是会话头族。
	Meta ChatMeta
}

// ChatResult 是一次对话调用的结果。
type ChatResult struct {
	// Response 是非流式聚合后的响应体（流式调用时为空）。
	Response []byte
	// Stats 是流式统计（非流式调用时为零值）。
	Stats StreamStats
	// Credential 是实际使用的凭证（可能被刷新过）。
	Credential *Credential
}

// ChatStream 发起一次流式对话，返回可供读取的响应体。
//
// 调用方负责关闭返回的 ReadCloser。上游强制流式（请求体里的 stream 恒为 true），
// 因此这是唯一的对话出口：非流式需求由 Aggregate 在本侧聚合。
func (c *Client) ChatStream(req ChatRequest) (io.ReadCloser, error) {
	if req.Credential == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := req.Credential.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	// 改写管线：加 stream、归一字段、注入思维链、脱敏。
	conversationKey := ConversationKey(req.Body)
	cacheKey := buildCacheKey(req.Credential.UIDValue(), core.FirstNonEmpty(conversationKey, req.Meta.ConversationRequestID))
	defaultEfforts, supportedEfforts := c.rt.effortTables()
	prepared := PrepareBody(req.Body, PrepareOptions{
		Model:            model,
		PromptMode:       c.opts.PromptMode,
		PromptText:       c.opts.PromptText,
		Sanitize:         c.opts.SanitizeFingerprints,
		DefaultEfforts:   defaultEfforts,
		SupportedEfforts: supportedEfforts,
		CacheKey:         cacheKey,
		Global:           region.IsGlobal(),
	})

	endpoints := GetEndpoints(region)
	httpReq, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost,
		endpoints.ChatBase+chatCompletionsPath, strings.NewReader(string(prepared)))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	c.applyChatHeaders(httpReq, req.Credential, req.Meta, req.ClientIP)

	resp, errDo := c.chatClient(chatHeaderTimeout).Do(httpReq)
	if errDo != nil {
		// 传输层错误：不是账号问题（不产生分类副作用），但换号重试有意义。
		return nil, transientError(errDo)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
		closeBody(resp.Body)
		errClassified := enrichClassifyError(resp.StatusCode, string(body), resp.Header)
		if errClassified != nil && errClassified.Kind == core.FailureKindContentBlocked && c.opts.OnContentBlocked != nil {
			c.opts.OnContentBlocked()
		}
		return nil, errClassified
	}
	return resp.Body, nil
}

// transientError 把传输层错误包装成一个「可重试但不罚号」的错误。
//
// 与分类错误的区别：Status 为 0，宿主按 502 处理并换号重试，
// 但不会因它把凭证标记为坏（半截响应不该让账号背锅）。
func transientError(err error) *core.Failure {
	if err == nil {
		return nil
	}
	return &core.Failure{Kind: core.FailureKindServer, Status: 0, Msg: err.Error()}
}

// Chat 发起一次对话并聚合为非流式响应。
func (c *Client) Chat(req ChatRequest) (*ChatResult, error) {
	reader, errStream := c.ChatStream(req)
	if errStream != nil {
		return nil, errStream
	}
	defer closeBody(reader)

	aggregated, errAggregate := Aggregate(reader)
	if errAggregate != nil {
		if IsEmptyStreamError(errAggregate) {
			return nil, &core.Failure{Kind: core.FailureKindServer, Status: http.StatusBadGateway,
				Msg: "upstream stream contained no valid data events (upstream_parse)"}
		}
		return nil, errAggregate
	}
	return &ChatResult{Response: aggregated, Credential: req.Credential}, nil
}

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

// 供应商实例标识。同时是凭证文件名前缀（workbuddycn-<uid>.json）。
//
// CN 与 GLOBAL 在本插件里是两个独立供应商：域名、模型清单、reasoning 档位表、
// 签到活动都不同，且凭证不通用。因此各自有独立的 ID 与文件名前缀，
// 让账号、模型、认证文件都能按供应商区分开。
const (
	// VendorIDCN 是国内版供应商标识。
	VendorIDCN = "workbuddycn"
	// VendorIDGlobal 是国际版供应商标识。
	VendorIDGlobal = "workbuddyglobal"
)

// VendorIDFor 返回该区域对应的供应商标识。
func VendorIDFor(region Region) string {
	if region.IsGlobal() {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// VendorNameFor 返回该区域的展示名（供管理端与页面使用）。
func VendorNameFor(region Region) string {
	if region.IsGlobal() {
		return "WorkBuddy 国际版"
	}
	return "WorkBuddy 国内版"
}

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
		// ChatBase 与官方 5.8.x 的 product.json endpoint 对齐：模型请求
		// 自 2026-10 起全部走 www.workbuddy.cn（copilot.tencent.com 只是
		// 桌面网关的兜底常量）。计费/成长域保持不变（两域目前都在线）。
		ChatBase:       "https://www.workbuddy.cn",
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
	// 企业用量接口：个人资源接口对企业账号返回空 Accounts，企业额度只能从这里取。
	enterpriseUsagePath   = "/billing/meter/get-enterprise-user-usage"
	enterpriseUsagePathV2 = "/v2/billing/meter/get-enterprise-user-usage"
	claimGiftPath         = "/billing/meter/claim-gift"
	claimCompPath         = "/billing/meter/claim-compensation"
	reportPath            = "/v2/report"
	trialPath             = "/billing/ide/trial"
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
//
// 版本号与正版 5.8.3 对齐（UA 三段式取自 daemon.log 头 dump：
// WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0）。
const (
	defaultClientVersion = "5.8.3"
	defaultCLIVersion    = "2.161.0"
	defaultClientName    = "WorkBuddy"
	// DesktopClientVersion 是桌面指纹族使用的版本（与 CLI 族刻意不同）。
	// 导出供 tasks 子包构造桌面指纹事件使用。
	DesktopClientVersion = "5.8.3"
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

// 本文件构造四类出站请求头。
//
// 为什么值得单独成文件：上游风控对请求头极其敏感——UA 的平台段写错、
// 少了 X-CodeBuddy-Request、会话头族不全会得到 403 code=11140 "request illegal"，
// 且报错信息完全不指向真正的原因。集中管理是唯一可靠的做法。

// ChatMeta 是与单次对话关联的会话头族。
//
// 由调用方在**轮转循环之外**生成一次，循环内复用：上游后台按
// X-Conversation-Request-ID 聚合同一轮请求，每次重试都换 ID 会被拆成多次会话。
type ChatMeta struct {
	// ConversationID 是多轮会话标识（可为空，为空时不发该头）。
	ConversationID string
	// ConversationRequestID 是轮级聚合主键（必发）。
	ConversationRequestID string
	// MessageID 是单条消息标识（每条独立）。
	MessageID string
}

// applyCommonHeaders 写入所有 API 共享的头。
func applyCommonHeaders(req *http.Request, c *Client, cred *Credential) {
	endpoints := GetEndpoints(cred.Realm())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", endpoints.Origin)
	req.Header.Set("Referer", endpoints.Origin+"/")
	req.Header.Set("User-Agent", c.userAgent(cred.Realm()))
	req.Header.Set("Accept-Language", endpoints.AcceptLanguage)
	// 这个头是上游识别"来自官方客户端"的基础判据之一，缺失会被风控拦。
	req.Header.Set("X-CodeBuddy-Request", "1")

	if uid := cred.UIDValue(); uid != "" {
		req.Header.Set("X-Machine-ID", c.rt.deriveID(uid, "machine"))
		req.Header.Set("X-Session-ID", c.rt.deriveID(uid, "session"))
	}
}

// applyAuthHeaders 写入凭证相关的头。
//
// 凭证缺失时不发空 Bearer（那会被当成"提供了无效凭证"），
// 而是发上游约定的 X-No-* 标记，让它给出更准确的错误。
func applyAuthHeaders(req *http.Request, cred *Credential) {
	if token := cred.AccessTokenValue(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if uid := cred.UIDValue(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
}

// userAgent 构造三段式 UA。
//
// 平台段在 global 域**必须**是 "WorkBuddy AI"：送成 "WorkBuddy" 会触发
// 上游 403 code=11140 "request illegal"。官方客户端不做任何 UA 随机化，
// 因此这里也保持确定性（随机化反而更容易被风控标记）。
func (c *Client) userAgent(region Region) string {
	if override := strings.TrimSpace(c.opts.UserAgent); override != "" {
		return override
	}
	clientVersion := c.opts.ClientVersion
	if clientVersion == "" {
		clientVersion = defaultClientVersion
	}
	cliVersion := c.opts.CLIVersion
	if cliVersion == "" {
		cliVersion = defaultCLIVersion
	}
	platform := GetEndpoints(region).PlatformLabel
	return "WorkBuddy/" + clientVersion + " " + platform + "/" + clientVersion + " CLI/" + cliVersion
}

// applyChatHeaders 写入对话请求专有的头。
func (c *Client) applyChatHeaders(req *http.Request, cred *Credential, meta ChatMeta, clientIP string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	// 正版对话请求（CLI 直发路径）不带 Origin/Referer/Accept-Language，
	// 也不带设备指纹三头（X-Device-Token 仅用于签到与 auth/state）。
	for _, key := range []string{
		"Origin", "Referer", "Accept-Language",
		"X-Device-Token", "X-Machine-ID", "X-Session-ID",
	} {
		req.Header.Del(key)
	}

	req.Header.Set("Accept", "application/json, text/event-stream")

	// 企业与域信息：CN 与 global 的形态不同，写错会被拒。
	// X-Domain 与正版一致取当前对话域主机名（个人账号也恒发）。
	if cred.Realm().IsGlobal() {
		req.Header.Set("X-No-Enterprise-Id", "1")
		req.Header.Set("X-Domain", endpointHost(GetEndpoints(RegionGlobal).ChatBase))
	} else {
		if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", enterpriseID)
			req.Header.Set("X-Tenant-Id", enterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		req.Header.Set("X-Domain", endpointHost(GetEndpoints(cred.Realm()).ChatBase))
	}

	applyAttributionHeaders(req, c)
	applyConversationHeaders(req, meta)

	if c.opts.PassthroughIP && strings.TrimSpace(clientIP) != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
		req.Header.Set("X-Real-IP", clientIP)
		req.Header.Set("X-Client-IP", clientIP)
	}
}

// applyAttributionHeaders 写入客户端归属头。
//
// 正版恒定发送：X-Product=SaaS（deploymentType）、X-IDE-Type/X-IDE-Name=WorkBuddy、
// X-IDE-Version=应用版本、X-Agent-Intent=craft、X-Agent-Type=main、X-Private-Data=false。
// client_name 显式配成 "SaaS" 时还原旧行为（只发 X-Product），保留这个开关
// 是为了在归属头引起风控变化时能快速回退。
func applyAttributionHeaders(req *http.Request, c *Client) {
	clientVersion := c.opts.ClientVersion
	if clientVersion == "" {
		clientVersion = defaultClientVersion
	}
	name := strings.TrimSpace(c.opts.ClientName)
	if name == "" {
		name = defaultClientName
	}
	if strings.EqualFold(name, "SaaS") {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", clientVersion)
	req.Header.Set("X-Private-Data", "false")
	req.Header.Set("X-Product", "SaaS")
}

// endpointHost 从基址 URL 提取主机名（含端口），供 X-Domain 使用。
// 基址是包内常量且恒带 scheme；解析失败只会出现在误改常量时，返回空串
// 让问题显式暴露，而不是静默回退到错误的域。
func endpointHost(base string) string {
	parsed, errParse := url.Parse(strings.TrimSpace(base))
	if errParse != nil {
		return ""
	}
	return parsed.Host
}

// applyConversationHeaders 写入会话头族。
//
// 正版形态（CLI 直发路径）：
//   - X-Conversation-Request-ID：每轮稳定（UUIDv7-32hex）；
//   - X-Conversation-Message-ID / X-Request-ID：每步新生（UUIDv7-32hex，两者同值）；
//   - X-Root-Request-ID / X-Parent-Conversation-ID：仅子会话才有，主对话不发；
//   - trace 族每请求随机：traceId 32hex、spanId 16hex、parentSpanId 16hex，
//     traceparent/b3/X-B3-*/X-Trace-ID 同源。
//
// 这一族头看似冗余（多个头取同一个值），但上游后台按它们做请求聚合与链路追踪，
// 缺任意一个都会让会话在后台被拆散。
func applyConversationHeaders(req *http.Request, meta ChatMeta) {
	messageID := strings.TrimSpace(meta.MessageID)
	if messageID == "" {
		messageID = NewUUIDv7Hex()
	}
	conversationRequestID := strings.TrimSpace(meta.ConversationRequestID)
	if conversationRequestID == "" {
		conversationRequestID = messageID
	}
	if conversationID := strings.TrimSpace(meta.ConversationID); conversationID != "" {
		req.Header.Set("X-Conversation-ID", conversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", conversationRequestID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)

	// trace 族：全随机且互相同源（正版由 otel 注入，不复用会话 ID）。
	traceID := core.RandomHex(16)
	spanID := core.RandomHex(8)
	parentSpanID := core.RandomHex(8)
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("b3", traceID+"-"+spanID+"-1-"+parentSpanID)
	req.Header.Set("X-B3-TraceId", traceID)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	req.Header.Set("X-B3-Sampled", "1")
	req.Header.Set("X-Trace-ID", traceID)
}

// applyBillingHeaders 写入计费域与成长域请求的头。
//
// 与对话头的差别：没有会话头族与归属四头，UA 是单段 "WorkBuddy/<version>"。
func (c *Client) applyBillingHeaders(req *http.Request, cred *Credential) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	// 计费域用单段 UA；显式配 SaaS 时不设 UA（旧行为）。
	if name := strings.TrimSpace(c.opts.ClientName); !strings.EqualFold(name, "SaaS") && strings.TrimSpace(c.opts.UserAgent) == "" {
		clientVersion := c.opts.ClientVersion
		if clientVersion == "" {
			clientVersion = defaultClientVersion
		}
		req.Header.Set("User-Agent", "WorkBuddy/"+clientVersion)
	}

	if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	}
	if domain := cred.DomainValue(); domain != "" {
		req.Header.Set("X-Domain", domain)
	}
	if token := c.DeviceToken(cred); token != "" {
		req.Header.Set("X-Device-Token", token)
	}
}

// applyMPHeaders 在计费头之上叠加小程序指纹头。
//
// 小程序口径的任务列表 / accept / claim 全链路都必须带这组头，
// 缺头时上游会把请求当成默认口径：列表不下发 mp 任务、accept 报 task not found。
func applyMPHeaders(req *http.Request) {
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
}

// applyDesktopHeaders 写入桌面指纹族请求的头。
//
// 桌面族用于成长任务的埋点上报：上游按 UA 与 X-Domain 把事件归属到
// "桌面客户端"，与 CLI 族的事件分开计分。
func (c *Client) applyDesktopHeaders(req *http.Request, cred *Credential) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUA())
	req.Header.Set("X-Domain", strings.TrimSuffix(GetEndpoints(cred.Realm()).ChatBase, "/"))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", requestIDFor(cred, c.rt))
}

// desktopUA 返回桌面指纹族的 UA。
func desktopUA() string {
	return "WorkBuddy/" + DesktopClientVersion + " WorkBuddy/" + DesktopClientVersion + " CLI/" + defaultCLIVersion
}

// requestIDFor 生成一个带纳秒后缀的请求 ID（桌面族要求每次不同）。
func requestIDFor(cred *Credential, rt *runtime) string {
	return rt.deriveID(cred.UIDValue(), "req") + newHexID()[:6]
}

// applyWebHeaders 写入 web 指纹族请求的头（浏览器形态）。
func applyWebHeaders(req *http.Request, cred *Credential, pageURL string) {
	endpoints := GetEndpoints(RegionCN)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", endpoints.WebBase)
	req.Header.Set("Referer", pageURL)
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("x-client-platform", "web")
	applyAuthHeaders(req, cred)
	if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	}
	if domain := cred.DomainValue(); domain != "" {
		req.Header.Set("X-Domain", domain)
	}
}

// applyV3ConfigHeaders 写入 /v3/config 专有的头。
//
// UA 是这里的风控闸门：过旧会拿到精简目录，或直接 400 code=12403。
func (c *Client) applyV3ConfigHeaders(req *http.Request, cred *Credential, ua string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Domain", v3ConfigDomain(cred))
}

// v3ConfigDomain 决定 /v3/config 的 X-Domain 取值。
func v3ConfigDomain(cred *Credential) string {
	domain := strings.TrimSpace(cred.DomainValue())
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimSuffix(domain, "/")
	if domain != "" {
		return domain
	}
	base := GetEndpoints(cred.Realm()).ChatBase
	base = strings.TrimPrefix(base, "https://")
	base = strings.TrimPrefix(base, "http://")
	return strings.TrimSuffix(base, "/")
}

// 本文件实现上游 SSE 流的处理：流式透传与非流式聚合。
//
// 上游返回的是 OpenAI 形态的 SSE（`data: {...}` 帧 + 结尾 `data: [DONE]`），
// 但直接原样转发给客户端是不安全的：
//   - 上游会在 `choices[].message` 里放整条消息（而非 delta），累加型客户端会重复拼接；
//   - 上游的 tool_call 分片每片都带 function.name，累加型客户端会拼成 "BashBash"；
//   - 上游可能中途漏掉 id/model 字段，客户端会把它当成新消息。
//
// 因此采用**白名单重建**：只保留已知字段，其余丢弃。

// errEmptyStream 表示上游流里没有任何有效数据帧。
//
// 单独定义是为了让调用方区分「上游空流」（上游缺陷，该报错）
// 与「客户端断连导致的写失败」（正常现象，不该报错）。
var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError 报告错误是否为上游空流。
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// StreamStats 是一次流式转发的统计。
type StreamStats struct {
	// Frames 是转发的有效帧数。
	Frames int
	// Usage 是上游给出的 usage（可能为空）。
	Usage map[string]any
	// Model 是上游回答里的模型名。
	Model string
	// Credit 是上游给出的本次真实扣费积分（可能为 nil）。
	Credit *float64
	// FinishReason 是最后一个 finish_reason。
	FinishReason string
	// SawDone 表示是否收到了 [DONE]。
	SawDone bool
}

// StreamOptions 是流式转发的选项。
type StreamOptions struct {
	// Emit 收到一帧可下发的载荷时调用。载荷是**裸 JSON**（不含 `data: ` 前缀），
	// 因为宿主会为 chat-completions 补上前缀与结尾的 [DONE]。
	Emit func(payload []byte) error
	// Hint 为错误帧附加值（可为 nil）。
	Hint func(code string) string
}

// Stream 读取上游 SSE 并把规范化后的帧交给 emit。
//
// 行为要点：
//   - 收到 `[DONE]` 后停止读取，之后的数据一律忽略；
//   - 含 error 键的帧原样透传（错误帧不该被白名单改写）；
//   - 白名单重建其余帧；
//   - 全程零有效数据帧 → 返回 errEmptyStream。
func Stream(body io.Reader, opts StreamOptions) (StreamStats, error) {
	var stats StreamStats
	if body == nil {
		return stats, errEmptyStream
	}

	scanner := bufio.NewScanner(body)
	// 上游的单帧可能很长（工具参数、长文本），默认 64KB 会截断。
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	firstID := ""
	seenToolName := map[int]bool{}
	sawAny := false

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// 非 data: 行（注释、event: 等）原样下发，保持上游的帧结构。
		if !strings.HasPrefix(trimmed, "data:") {
			if errEmit := opts.emit([]byte(line)); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			stats.SawDone = true
			break
		}
		if payload == "" {
			continue
		}

		// 错误帧：原样透传，可选附加提示。上游的错误信息比我们重建的更有价值。
		if isErrorFrame(payload) {
			stats.Frames++
			sawAny = true
			if errEmit := opts.emit([]byte(attachHint(payload, opts.Hint))); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		var frame map[string]any
		if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
			// 无法解析的帧原样透传（可能是上游新增的帧类型）。
			stats.Frames++
			sawAny = true
			if errEmit := opts.emit([]byte(payload)); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		// tool_call 的 name 只保留在首片。
		StripToolCallNames(frame, seenToolName)

		// id 续传：上游偶发在后续帧漏 id，客户端会把它当成新消息。
		if id, _ := frame["id"].(string); strings.TrimSpace(id) != "" {
			if firstID == "" {
				firstID = id
			}
		} else if firstID != "" {
			frame["id"] = firstID
		}

		collectStats(frame, &stats)
		normalized := normalizeFrame(frame)

		encoded, errEncode := json.Marshal(normalized)
		if errEncode != nil {
			continue
		}
		stats.Frames++
		sawAny = true
		if errEmit := opts.emit(encoded); errEmit != nil {
			return stats, errEmit
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		// 扫描错误（含上游断流）：已转发的帧仍然有效，把错误交给调用方判断。
		return stats, fmt.Errorf("read upstream stream: %w", errScan)
	}
	if !sawAny {
		return stats, errEmptyStream
	}
	return stats, nil
}

func (o StreamOptions) emit(payload []byte) error {
	if o.Emit == nil {
		return nil
	}
	return o.Emit(payload)
}

// isErrorFrame 判断帧是否为错误帧。
func isErrorFrame(payload string) bool {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(payload), &probe); errUnmarshal != nil {
		return false
	}
	return len(probe.Error) > 0 && string(probe.Error) != "null"
}

// attachHint 给错误帧附加提示文案（只加不改）。
func attachHint(payload string, hint func(code string) string) string {
	if hint == nil {
		return payload
	}
	var frame map[string]any
	if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
		return payload
	}
	errorField, okError := frame["error"].(map[string]any)
	if !okError {
		return payload
	}
	if _, okHint := errorField["gateway_hint"]; okHint {
		return payload
	}
	code, _ := errorField["code"].(string)
	text := hint(code)
	if strings.TrimSpace(text) == "" {
		return payload
	}
	errorField["gateway_hint"] = text
	encoded, errEncode := json.Marshal(frame)
	if errEncode != nil {
		return payload
	}
	return string(encoded)
}

// collectStats 从帧里提取统计信息。
func collectStats(frame map[string]any, stats *StreamStats) {
	if usage, okUsage := frame["usage"].(map[string]any); okUsage && len(usage) > 0 {
		stats.Usage = usage
		if credit, okCredit := toFloat64(usage["credit"]); okCredit {
			stats.Credit = &credit
		}
	}
	if model, okModel := frame["model"].(string); okModel && strings.TrimSpace(model) != "" {
		stats.Model = model
	}
	choices, okChoices := frame["choices"].([]any)
	if !okChoices || len(choices) == 0 {
		return
	}
	choice, okChoice := choices[0].(map[string]any)
	if !okChoice {
		return
	}
	if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
		stats.FinishReason = reason
	}
}

func toFloat64(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int64:
		return float64(typed), true
	case int:
		return float64(typed), true
	case json.Number:
		parsed, errParse := typed.Float64()
		return parsed, errParse == nil
	}
	return 0, false
}

// normalizeFrame 用白名单重建帧。
//
// 顶层保留：id / object / created / model / system_fingerprint / service_tier / choices / usage。
// delta 只保留：role / content / reasoning_content / refusal / tool_calls /
// function_call（空占位剔除）。
//
// 丢弃上游的私有字段（session_id、trace 等）：它们对客户端无用，
// 而且会把上游内部的会话标识暴露给下游。
func normalizeFrame(frame map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if value, okValue := frame[key]; okValue {
			out[key] = value
		}
	}
	if _, okObject := out["object"]; !okObject {
		out["object"] = "chat.completion.chunk"
	}
	if _, okID := out["id"]; !okID {
		out["id"] = "chatcmpl-wb2api"
	}

	if choices, okChoices := frame["choices"].([]any); okChoices {
		out["choices"] = normalizeChoices(choices)
	} else {
		out["choices"] = []any{}
	}

	// usage 缺失时显式给 null：客户端普遍会读这个键判断是否结束。
	if usage, okUsage := frame["usage"]; okUsage {
		out["usage"] = usage
	} else {
		out["usage"] = nil
	}
	return out
}

// normalizeChoices 重建 choices。
func normalizeChoices(choices []any) []any {
	out := make([]any, 0, len(choices))
	for _, item := range choices {
		choice, okChoice := item.(map[string]any)
		if !okChoice {
			continue
		}
		normalized := map[string]any{}
		if index, okIndex := choice["index"]; okIndex {
			normalized["index"] = index
		}
		if delta, okDelta := choice["delta"].(map[string]any); okDelta {
			normalized["delta"] = normalizeDelta(delta)
		}
		// finish_reason 空串归一为 null（客户端靠它判断结束）。
		if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
			normalized["finish_reason"] = reason
		} else {
			normalized["finish_reason"] = nil
		}
		out = append(out, normalized)
	}
	return out
}

// normalizeDelta 用白名单重建 delta。
func normalizeDelta(delta map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"role", "content", "reasoning_content", "refusal"} {
		if value, okValue := delta[key]; okValue {
			out[key] = value
		}
	}
	if calls, okCalls := delta["tool_calls"].([]any); okCalls && len(calls) > 0 {
		out["tool_calls"] = calls
	}
	// function_call 是旧字段：空对象占位会被客户端当成一次调用，必须剔除。
	if functionCall, okFunctionCall := delta["function_call"].(map[string]any); okFunctionCall && len(functionCall) > 0 {
		out["function_call"] = functionCall
	}
	return out
}

// Aggregate 把上游 SSE 流聚合成一个非流式响应。
//
// 上游强制流式，因此非流式客户端必须由我们在网关侧聚合。
func Aggregate(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, errEmptyStream
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var (
		id           string
		model        string
		created      int64
		content      strings.Builder
		reasoning    strings.Builder
		toolCalls    []any
		usage        map[string]any
		finishReason string
		sawDone      bool
		validEvents  int
		gotContent   bool
	)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		if payload == "" {
			continue
		}
		var frame map[string]any
		if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
			continue
		}
		if isErrorFrame(payload) {
			return nil, fmt.Errorf("upstream error: %s", core.Truncate(payload, 512))
		}
		validEvents++

		if got := core.StringField(frame, "id"); got != "" && id == "" {
			id = got
		}
		if got := core.StringField(frame, "model"); got != "" {
			model = got
		}
		if value, okValue := toFloat64(frame["created"]); okValue {
			created = int64(value)
		}
		if payloadUsage, okUsage := frame["usage"].(map[string]any); okUsage && len(payloadUsage) > 0 {
			usage = payloadUsage
		}

		choices, okChoices := frame["choices"].([]any)
		if !okChoices || len(choices) == 0 {
			continue
		}
		choice, okChoice := choices[0].(map[string]any)
		if !okChoice {
			continue
		}
		if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
			finishReason = reason
		}

		// 部分上游帧把内容放在 message（而非 delta）里，两种都要处理。
		// gotContent 闩锁防止同一份内容被 delta 与 message 各算一次。
		if delta, okDelta := choice["delta"].(map[string]any); okDelta {
			appendContent(&content, delta["content"])
			appendContent(&reasoning, delta["reasoning_content"])
			if calls, okCalls := delta["tool_calls"].([]any); okCalls && len(calls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, calls)
				gotContent = true
			}
		}
		if message, okMessage := choice["message"].(map[string]any); okMessage && !gotContent {
			if text, okText := message["content"].(string); okText && text != "" {
				content.WriteString(text)
			}
			if text, okText := message["reasoning_content"].(string); okText && text != "" {
				reasoning.WriteString(text)
			}
		}
		if content.Len() > 0 {
			gotContent = true
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, fmt.Errorf("read upstream stream: %w", errScan)
	}
	if validEvents == 0 {
		return nil, errEmptyStream
	}

	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		// 输出被截断或流没正常结束时，工具参数必然不完整，丢弃它们。
		if finishReason == "length" || !sawDone {
			toolCalls = DropTruncatedToolCalls(toolCalls)
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = sortToolCalls(toolCalls)
		}
	}
	if strings.TrimSpace(finishReason) == "" {
		finishReason = "stop"
	}

	response := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		response["usage"] = ensureUsageTotal(usage)
	}
	return json.Marshal(response)
}

func appendContent(builder *strings.Builder, value any) {
	if text, okText := value.(string); okText && text != "" {
		builder.WriteString(text)
	}
}

// mergeToolCalls 按 index 合并工具调用分片。
//
// 上游的分片形态：首片带 id/name/arguments 开头，后续片只带 arguments 增量。
// 缺 index 的分片需要容错（id 已见过则延续，否则开新序号）。
func mergeToolCalls(existing []any, incoming []any) []any {
	byIndex := make(map[int]map[string]any, len(existing))
	order := make([]int, 0, len(existing)+len(incoming))
	for _, item := range existing {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		index := core.IntFromAny(call["index"])
		byIndex[index] = call
		order = append(order, index)
	}

	nextIndex := len(order)
	for _, item := range incoming {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		index, okIndex := call["index"]
		var key int
		if okIndex {
			key = core.IntFromAny(index)
		} else {
			// 缺 index：延续最近一次调用（上游偶发行为）。
			if len(order) > 0 {
				key = order[len(order)-1]
			} else {
				key = nextIndex
				nextIndex++
			}
		}
		target, okTarget := byIndex[key]
		if !okTarget {
			target = map[string]any{"index": key}
			byIndex[key] = target
			order = append(order, key)
		}
		mergeToolCallFields(target, call)
	}

	out := make([]any, 0, len(order))
	for _, key := range order {
		if call, okCall := byIndex[key]; okCall {
			out = append(out, call)
		}
	}
	return out
}

// mergeToolCallFields 把分片字段合并进目标调用。
func mergeToolCallFields(target, incoming map[string]any) {
	if id, okID := incoming["id"].(string); okID && strings.TrimSpace(id) != "" {
		target["id"] = id
	}
	if callType, okType := incoming["type"].(string); okType && strings.TrimSpace(callType) != "" {
		target["type"] = callType
	}
	incomingFunction, okFunction := incoming["function"].(map[string]any)
	if !okFunction {
		return
	}
	targetFunction, okTargetFunction := target["function"].(map[string]any)
	if !okTargetFunction {
		targetFunction = map[string]any{}
		target["function"] = targetFunction
	}
	if name, okName := incomingFunction["name"].(string); okName && strings.TrimSpace(name) != "" {
		targetFunction["name"] = name
	}
	if arguments, okArgs := incomingFunction["arguments"].(string); okArgs && arguments != "" {
		previous, _ := targetFunction["arguments"].(string)
		targetFunction["arguments"] = previous + arguments
	}
}

// sortToolCalls 按 index 排序输出（客户端依赖顺序）。
func sortToolCalls(calls []any) []any {
	out := append([]any(nil), calls...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			left, _ := out[j-1].(map[string]any)
			right, _ := out[j].(map[string]any)
			if core.IntFromAny(left["index"]) <= core.IntFromAny(right["index"]) {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// ensureUsageTotal 补齐缺失的 total_tokens。
//
// 不修改原 map：上游的 usage 可能被其他路径复用，就地改会有隐性耦合。
func ensureUsageTotal(usage map[string]any) map[string]any {
	if _, okTotal := usage["total_tokens"]; okTotal {
		return usage
	}
	prompt, okPrompt := toFloat64(usage["prompt_tokens"])
	completion, okCompletion := toFloat64(usage["completion_tokens"])
	if !okPrompt || !okCompletion {
		return usage
	}
	out := make(map[string]any, len(usage)+1)
	for key, value := range usage {
		out[key] = value
	}
	out["total_tokens"] = int64(prompt + completion)
	return out
}

// ChatMetaFromBody 从请求体派生会话头族。
//
// conversationRequestID 按正版语义**每轮生成一次**的 UUIDv7（同一次调用的
// 重试循环共用，靠调用方在轮转循环之外构造一次来保证）：内容派生值不是
// 合法 v7 形态，且同一轮跨会话重放会复用同一 ID。
func ChatMetaFromBody(body []byte) ChatMeta {
	return ChatMeta{
		ConversationID:        ConversationKey(body),
		ConversationRequestID: NewUUIDv7Hex(),
	}
}

// ==================== 请求体改写与清洗管线 ====================

// 本文件实现「OpenAI 请求体 → CodeBuddy 请求体」的改写管线。
//
// **改写顺序有语义，不可调换**。每一步都对应一个曾经真实踩过的坑：
//
//	1. stream 强制 true          —— 上游拒绝非流式请求
//	2. max_completion_tokens 归一 —— 上游只认 max_tokens，别名会被静默忽略
//	3. stream_options 补默认      —— 不补就拿不到 usage（含真实扣费）
//	4. tool_choice 归一           —— 上游该字段是 string，对象形态直接 400
//	5. developer → system         —— 上游 role 白名单不含 developer，命中即 400
//	6. image_url 归一             —— 上游只接受对象形态，字符串会 400
//	7. tool 结果重排              —— 插队消息会让上游判定工具配对失败
//	8. 孤儿 tool_call 裁剪        —— 配对不全上游会顶死整条会话
//	9. 思维链注入                 —— deepseek 系不显式开思考就不返回思维链
//	10. 档位归一                  —— 非法档位 400
//	11. reasoning_content 回填    —— 部分租户要求 assistant 消息带非空 reasoning
//	12. 指纹脱敏                  —— 上游内容审核是逐字匹配

// PrepareOptions 是改写管线的输入。
type PrepareOptions struct {
	// Model 是目标模型裸名。非空时直接改写 payload["model"]。
	Model string
	// PromptMode 提示词模式：passthrough / custom / append。
	PromptMode string
	// PromptText 自定义或追加的提示词内容。
	PromptText string
	// Sanitize 开启指纹脱敏（第 12 步）。
	Sanitize bool
	// DefaultEfforts 是模型名 → 默认档位的表（来自模型目录）。
	DefaultEfforts map[string]string
	// SupportedEfforts 是模型名 → 支持档位列表的表（来自模型目录）。
	SupportedEfforts map[string][]string
	// CacheKey 是 prompt_cache_key；为空时跳过注入。
	CacheKey string
	// Global 表示这是 global 域请求（额外需要 system 兜底）。
	Global bool
}

// defaultDeepSeekEffort 是 deepseek 系模型缺失档位时的兜底默认档。
const defaultDeepSeekEffort = "high"

// effortRank 是档位的强度排序，用于「取不高于请求档的最高支持档」。
var effortRank = map[string]int{
	"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// PrepareBody 执行完整改写管线，返回可直接发给上游的请求体。
//
// 解析失败时原样返回输入：宁可不改写也不要让一次对话因为改写失败而中断，
// 上游的真实报错比本地的解析错误更有诊断价值。
func PrepareBody(body []byte, opts PrepareOptions) []byte {
	payload := decodeBodyMap(body)
	if payload == nil {
		return body
	}

	// 0) 出站 model 设为裸名（单 pass 改写，无需二次序列化）。
	if trimmedModel := strings.TrimSpace(opts.Model); trimmedModel != "" {
		payload["model"] = trimmedModel
	}

	// 1) 上游拒绝非流式。
	payload["stream"] = true

	// 2) max_completion_tokens → max_tokens。
	translateMaxCompletionTokens(payload)

	// 3) stream_options 缺省补 include_usage（不补就拿不到 usage）。
	if _, okOptions := payload["stream_options"]; !okOptions {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	// 4) tool_choice 归一。
	normalizeToolChoice(payload)

	// 5) developer → system。
	normalizeRoles(payload)

	// 5.5) 系统提示词处理（透传 / 替换 / 追加）。
	applyPromptMode(payload, opts.PromptMode, opts.PromptText)

	// 6) image_url 归一。
	normalizeImageURL(payload)

	if messages, okMessages := payload["messages"].([]any); okMessages {
		// 7) 工具结果重排。
		messages = repackToolResultBlocks(messages)
		// 8) 孤儿 tool_call 裁剪。
		messages = cleanupOrphanToolCalls(messages)
		payload["messages"] = messages
	}

	model, _ := payload["model"].(string)
	model = strings.TrimSpace(model)

	// 9) 思维链注入。
	injectThinking(payload, defaultEffortFor(opts.DefaultEfforts, model))

	// 10) 档位归一。
	normalizeReasoningEffort(payload, opts.SupportedEfforts[model])

	// 10.5) reasoning_summary：正版在开思考（thinking enabled 或带档位）
	// 且调用方未指定时注入 "auto"，补齐协议形态。
	injectReasoningSummary(payload)

	// 11) reasoning_content 回填。
	backfillReasoningContent(payload)

	// global 域的兜底 system：上游某些路径要求首条消息必须是 system。
	if opts.Global {
		ensureConsoleSystem(payload)
	}

	// prompt_cache_key：单项最大的费用优化，必须在最后注入（前面的步骤不依赖它）。
	if strings.TrimSpace(opts.CacheKey) != "" {
		if _, okKey := payload["prompt_cache_key"]; !okKey {
			payload["prompt_cache_key"] = opts.CacheKey
		}
	}

	// 12) 指纹脱敏（放最后：前面步骤可能引入需要净化的文本）。
	if opts.Sanitize {
		if messages, okMessages := payload["messages"].([]any); okMessages {
			sanitizeMessages(messages)
		}
	}

	encoded, errEncode := encodeBodyMap(payload)
	if errEncode != nil {
		return body
	}
	return encoded
}

// translateMaxCompletionTokens 把 max_completion_tokens 翻译成 max_tokens。
//
// 背景：OpenAI 规范里 max_tokens 已废弃、max_completion_tokens 是新字段，
// 但上游只认 max_tokens——透传别名会被忽略，然后回落到默认输出上限，
// 长任务被静默截断。
//
// 规则：
//   - 别名**无论是否翻译都删除**（减少体积与排障噪音）；
//   - 显式 max_tokens 存在时只删不译（显式优先）；
//   - 别名是正整数值才翻译，写成 int64 避免科学计数法（1.28e5）；
//   - 0/null/负数/非数值不翻译（0/null 语义是「未设置」，负数是非法值）。
func translateMaxCompletionTokens(payload map[string]any) {
	alias, hasAlias := payload["max_completion_tokens"]
	if !hasAlias {
		return
	}
	delete(payload, "max_completion_tokens")

	if _, hasExplicit := payload["max_tokens"]; hasExplicit {
		return
	}
	if value, okValue := positiveInt64(alias); okValue {
		payload["max_tokens"] = value
	}
}

// positiveInt64 把 JSON 数值转成正整数。
func positiveInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed <= 0 || typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case int64:
		if typed <= 0 {
			return 0, false
		}
		return typed, true
	case int:
		if typed <= 0 {
			return 0, false
		}
		return int64(typed), true
	}
	return 0, false
}

// normalizeToolChoice 把 tool_choice 归一成上游接受的形态。
//
// 上游该字段是 Go string，对象形态会 400 code=11101。
func normalizeToolChoice(payload map[string]any) {
	raw, hasChoice := payload["tool_choice"]
	if !hasChoice {
		return
	}
	if text, okText := raw.(string); okText {
		if strings.EqualFold(strings.TrimSpace(text), "none") {
			// "none" 表示不使用工具：删掉选择器与工具定义本身。
			delete(payload, "tool_choice")
			delete(payload, "tools")
			delete(payload, "functions")
			return
		}
		return
	}
	object, okObject := raw.(map[string]any)
	if !okObject {
		delete(payload, "tool_choice")
		return
	}
	choiceType, _ := object["type"].(string)
	switch strings.ToLower(strings.TrimSpace(choiceType)) {
	case "none":
		delete(payload, "tool_choice")
		delete(payload, "tools")
		delete(payload, "functions")
	case "auto", "required":
		payload["tool_choice"] = strings.ToLower(strings.TrimSpace(choiceType))
	case "function":
		// 两种形态：{"function":{"name":"x"}} 与 {"name":"x"}。
		name := ""
		if function, okFunction := object["function"].(map[string]any); okFunction {
			name, _ = function["name"].(string)
		}
		if strings.TrimSpace(name) == "" {
			name, _ = object["name"].(string)
		}
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			payload["tool_choice"] = trimmed
			return
		}
		delete(payload, "tool_choice")
	default:
		delete(payload, "tool_choice")
	}
}

// normalizeRoles 把 developer 归一为 system。
//
// 上游的 role 白名单不含 developer，命中即 HTTP 400 code=11-128。
// 只归一这一个值，其余 role 原样保留（不合并、不重排、不删除）。
func normalizeRoles(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, okRole := message["role"].(string)
		if !okRole {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			message["role"] = "system"
		}
	}
}

// normalizeImageURL 把字符串形态的 image_url 转成对象形态。
//
// 上游只接受对象，字符串会 400 code=11101 cannot unmarshal string into ImageContent。
// 已是对象的一律原样保留（不补 detail/mime_type 默认值：让上游返回真实错误，
// 而不是被我们补的默认值掩盖）。
func normalizeImageURL(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		parts, okParts := message["content"].([]any)
		if !okParts {
			continue
		}
		for _, part := range parts {
			partMap, okPartMap := part.(map[string]any)
			if !okPartMap {
				continue
			}
			if partType, okType := partMap["type"].(string); !okType || partType != "image_url" {
				continue
			}
			if url, okURL := partMap["image_url"].(string); okURL {
				partMap["image_url"] = map[string]any{"url": url}
			}
		}
	}
}

// ensureConsoleSystem 在 global 域缺少首条 system 时补一条兜底。
//
// 上游 global 的某些路径要求 messages[0] 是 system，缺失时行为不确定。
func ensureConsoleSystem(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages || len(messages) == 0 {
		return
	}
	if first, okFirst := messages[0].(map[string]any); okFirst {
		if role, okRole := first["role"].(string); okRole && strings.EqualFold(strings.TrimSpace(role), "system") {
			return
		}
	}
	payload["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}}, messages...)
}

// defaultEffortFor 查模型的默认档位。
func defaultEffortFor(table map[string]string, model string) string {
	if len(table) == 0 {
		return ""
	}
	return strings.TrimSpace(table[strings.TrimSpace(model)])
}

// applyPromptMode 处理系统提示词：custom 替换全部 system/developer；append 在开头连续 system 块后追加。
func applyPromptMode(payload map[string]any, mode, text string) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	text = strings.TrimSpace(text)
	if mode == "" || mode == "passthrough" || text == "" {
		return
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	if mode == "custom" {
		kept := make([]any, 0, len(messages)+1)
		kept = append(kept, map[string]any{"role": "system", "content": text})
		for _, item := range messages {
			msgMap, okMap := item.(map[string]any)
			if !okMap {
				kept = append(kept, item)
				continue
			}
			role, _ := msgMap["role"].(string)
			switch strings.ToLower(strings.TrimSpace(role)) {
			case "system", "developer":
				continue
			}
			kept = append(kept, item)
		}
		payload["messages"] = kept
	} else if mode == "append" {
		insertAt := 0
		for insertAt < len(messages) {
			msgMap, okMap := messages[insertAt].(map[string]any)
			if !okMap {
				break
			}
			role, _ := msgMap["role"].(string)
			switch strings.ToLower(strings.TrimSpace(role)) {
			case "system", "developer":
				insertAt++
				continue
			}
			break
		}
		out := make([]any, 0, len(messages)+1)
		out = append(out, messages[:insertAt]...)
		out = append(out, map[string]any{"role": "system", "content": text})
		out = append(out, messages[insertAt:]...)
		payload["messages"] = out
	}
}

// decodeBodyMap 把请求体解析成 map。解析失败返回 nil（调用方按"无法解析"处理）。
func decodeBodyMap(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil
	}
	return payload
}

// encodeBodyMap 把 map 序列化回请求体。
func encodeBodyMap(payload map[string]any) ([]byte, error) {
	return json.Marshal(payload)
}

// 本文件处理 DeepSeek 系的思维链。
//
// 上游对 deepseek 系模型标记了 thinkingFormat:"deepseek" 与
// requiresReasoningContentOnAssistantMessages，带来两个硬性要求：
//   - 开启思考必须显式带 thinking:{type:"enabled"} **且有档位**，
//     否则上游默认按「不思考」应答（思维链完全不返回）；
//   - assistant 消息必须带非空 reasoning，部分租户校验 len(reasoning) > 0，
//     缺失/null/空串一律 400（空白串可以，说明校验的是长度而非内容）。

// isDeepSeekModel 判断是否为 deepseek 系模型。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectThinking 为 deepseek 系模型注入思维链开关。
//
//	输入                           行为
//	thinking.type == "disabled"    尊重，删除档位字段
//	thinking.type == "enabled"     补默认档（若缺）
//	无 thinking / 非对象 / type 空  注入 enabled 并补默认档
func injectThinking(payload map[string]any, defaultEffort string) {
	model, _ := payload["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	thinking, okThinking := payload["thinking"].(map[string]any)
	if !okThinking {
		payload["thinking"] = map[string]any{"type": "enabled"}
		ensureDeepSeekEffort(payload, defaultEffort)
		return
	}
	thinkingType, _ := thinking["type"].(string)
	switch strings.ToLower(strings.TrimSpace(thinkingType)) {
	case "disabled":
		// 用户明确要关：尊重它，同时删掉档位（开着档位却关思考是矛盾组合）。
		delete(payload, "reasoning_effort")
		delete(payload, "reasoningEffort")
	case "enabled":
		ensureDeepSeekEffort(payload, defaultEffort)
	default:
		thinking["type"] = "enabled"
		ensureDeepSeekEffort(payload, defaultEffort)
	}
}

// ensureDeepSeekEffort 保证 deepseek 请求带一个档位。
//
// snake_case 优先、camelCase 兜底；两者都没有才写默认档。
func ensureDeepSeekEffort(payload map[string]any, defaultEffort string) {
	if _, okSnake := payload["reasoning_effort"]; okSnake {
		return
	}
	if _, okCamel := payload["reasoningEffort"]; okCamel {
		return
	}
	effort := strings.TrimSpace(defaultEffort)
	if effort == "" {
		effort = defaultDeepSeekEffort
	}
	payload["reasoning_effort"] = effort
}

// normalizeReasoningEffort 把档位降到模型支持的范围。
//
// 规则（支持表为空时一律透传，不做任何猜测）：
//   - 请求档位在支持列表里 → 原样透传；
//   - 否则取**不高于**请求档的最高支持档；
//   - 若所有支持档都高于请求档 → 取最低支持档（偏离最小）。
//
// 上游收到非法档位直接 400，而降级到最近的合法档位能让请求成功。
func normalizeReasoningEffort(payload map[string]any, supported []string) {
	if len(supported) == 0 {
		return
	}
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		raw, okRaw := payload[key].(string)
		if !okRaw {
			continue
		}
		requested := strings.ToLower(strings.TrimSpace(raw))
		if requested == "" {
			continue
		}
		if containsString(supported, requested) {
			continue
		}
		payload[key] = nearestEffort(supported, effortRank[requested])
	}
}

// nearestEffort 在支持列表里找与请求档位最接近的档位。
func nearestEffort(supported []string, requestedRank int) string {
	lowest := ""
	lowestRank := 99
	nearest := ""
	nearestRank := -1
	for _, item := range supported {
		rank, okRank := effortRank[item]
		if !okRank {
			continue
		}
		if rank < lowestRank {
			lowestRank = rank
			lowest = item
		}
		// 不高于请求档的最高者。
		if rank <= requestedRank && rank > nearestRank {
			nearestRank = rank
			nearest = item
		}
	}
	if nearest != "" {
		return nearest
	}
	// 所有支持档都高于请求档：取最低支持档（偏离最小）。
	if lowest != "" {
		return lowest
	}
	return supported[0]
}

// injectReasoningSummary 在开思考且调用方未指定摘要档位时注入 "auto"。
//
// 与正版 injectReasoningSummaryIfNeeded 对齐：thinking.type=enabled、
// reasoning_effort 或 reasoning.effort 存在即视为开思考；
// body 已带 reasoning_summary（含显式关闭）时不覆盖。
func injectReasoningSummary(payload map[string]any) {
	if _, okSummary := payload["reasoning_summary"]; okSummary {
		return
	}
	if !reasoningGateOpen(payload) {
		return
	}
	payload["reasoning_summary"] = "auto"
}

// backfillReasoningContent 为 assistant 消息补齐 reasoning 字段。
//
// 门控：请求开了思考，或历史里已经有思维链痕迹。
// 只处理 assistant 消息：
//   - reasoning_content 已是字符串 → 不覆盖；
//   - 否则若 reasoning 是字符串 → 复制其值；
//   - 否则补空串。
//
// 同时镜像保证 reasoning 字段非空：缺失/null/空串归一为单空格
// （实测部分租户校验 len(reasoning) > 0，空白串可通过）。
func backfillReasoningContent(payload map[string]any) {
	if !reasoningGateOpen(payload) {
		return
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}

		if _, okString := message["reasoning_content"].(string); !okString {
			if reasoning, okReasoning := message["reasoning"].(string); okReasoning {
				message["reasoning_content"] = reasoning
			} else {
				message["reasoning_content"] = ""
			}
		}

		// 镜像字段：非空优先，皆无补一个空格。
		reasoning, _ := message["reasoning"].(string)
		if strings.TrimSpace(reasoning) == "" {
			if filled, okFilled := message["reasoning_content"].(string); okFilled && strings.TrimSpace(filled) != "" {
				message["reasoning"] = filled
			} else {
				message["reasoning"] = " "
			}
		}
	}
}

// reasoningGateOpen 判断是否需要回填思维链字段。
//
// 开思考时必须回填（上游会把历史里的 assistant 消息一起校验）；
// 历史里已经出现过思维链痕迹时也要回填，否则后续轮次会突然缺失字段。
func reasoningGateOpen(payload map[string]any) bool {
	if thinking, okThinking := payload["thinking"].(map[string]any); okThinking {
		if thinkingType, okType := thinking["type"].(string); okType &&
			strings.EqualFold(strings.TrimSpace(thinkingType), "enabled") {
			return true
		}
	}
	messages, _ := payload["messages"].([]any)
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		if reasoning, okReasoning := message["reasoning"].(string); okReasoning && strings.TrimSpace(reasoning) != "" {
			return true
		}
		if _, okContent := message["reasoning_content"]; okContent {
			return true
		}
	}
	return false
}

// 本文件处理 tool_call 与 tool 结果的配对问题。
//
// 上游对工具配对的校验很严：调用与结果必须**双侧齐全**，否则返回
// 11148 "tool calls and tool results do not match" 并顶死整条会话。
//
// 两个真实来源的破坏：
//   - 客户端（如 Codex）会在并行调用的两份结果之间插入非 tool 消息；
//   - 客户端会发送「调用没有结果」或「结果没有调用」的残缺历史。

// repackToolResultBlocks 把插在 tool_calls 与其结果之间的非 tool 消息挪到整组之后。
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	→ assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// 只调顺序不改内容。无插入消息时返回原 slice（零改动零分配）。
func repackToolResultBlocks(messages []any) []any {
	if len(messages) < 3 {
		return messages
	}
	out := make([]any, 0, len(messages))
	changed := false

	for index := 0; index < len(messages); index++ {
		message, okMessage := messages[index].(map[string]any)
		if !okMessage {
			out = append(out, messages[index])
			continue
		}
		if !hasToolCalls(message) {
			out = append(out, messages[index])
			continue
		}

		// 收集这一组：调用消息本身 + 紧随其后的 tool 结果（中间可能夹着别的消息）。
		group := []any{messages[index]}
		var displaced []any
		cursor := index + 1
		for ; cursor < len(messages); cursor++ {
			next, okNext := messages[cursor].(map[string]any)
			if !okNext {
				break
			}
			role, _ := next["role"].(string)
			role = strings.ToLower(strings.TrimSpace(role))
			if role == "tool" {
				group = append(group, messages[cursor])
				continue
			}
			// 遇到下一组带 tool_calls 的 assistant 必须交还外层循环：
			// 否则它那批结果永远得不到重排（真实会话里正是这样漏掉的）。
			if hasToolCalls(next) {
				break
			}
			displaced = append(displaced, messages[cursor])
		}
		if len(displaced) > 0 {
			changed = true
			out = append(out, group...)
			out = append(out, displaced...)
		} else {
			out = append(out, group...)
		}
		index = cursor - 1
	}
	if !changed {
		return messages
	}
	return out
}

// cleanupOrphanToolCalls 裁剪配对不全的工具调用与结果。
//
// 规则：调用与结果的交集才保留。
//   - assistant.tool_calls 按交集对称裁剪；裁空则删除该键；
//   - role=tool 只在对应调用被保留时保留，否则整条删除。
//
// 历史上的错误做法是「批内全齐才整批保留，否则删掉整个 tool_calls 键」，
// 那会留下「无 tool_calls 的 assistant + 孤儿 tool」，正好命中上游的 11148。
func cleanupOrphanToolCalls(messages []any) []any {
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}

	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "assistant":
			for _, id := range toolCallIDs(message) {
				callIDs[id] = true
			}
		case "tool":
			if id, okID := message["tool_call_id"].(string); okID && strings.TrimSpace(id) != "" {
				resultIDs[strings.TrimSpace(id)] = true
			}
		}
	}
	if len(callIDs) == 0 && len(resultIDs) == 0 {
		return messages
	}

	keep := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keep[id] = true
		}
	}

	out := make([]any, 0, len(messages))
	changed := false
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			out = append(out, item)
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "assistant":
			if !hasToolCalls(message) {
				out = append(out, message)
				continue
			}
			filtered := filterToolCalls(message, keep)
			if len(filtered) == 0 {
				delete(message, "tool_calls")
				changed = true
			} else {
				message["tool_calls"] = filtered
			}
			out = append(out, message)
		case "tool":
			id, _ := message["tool_call_id"].(string)
			if !keep[strings.TrimSpace(id)] {
				changed = true
				continue
			}
			out = append(out, message)
		default:
			out = append(out, message)
		}
	}
	if !changed {
		return messages
	}
	return out
}

// toolCallIDs 取出 assistant 消息里的全部 tool_call id。
func toolCallIDs(message map[string]any) []string {
	calls, okCalls := message["tool_calls"].([]any)
	if !okCalls {
		return nil
	}
	out := make([]string, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		if id, okID := call["id"].(string); okID && strings.TrimSpace(id) != "" {
			out = append(out, strings.TrimSpace(id))
		}
	}
	return out
}

// hasToolCalls 报告消息是否带非空 tool_calls。
func hasToolCalls(message map[string]any) bool {
	calls, okCalls := message["tool_calls"].([]any)
	return okCalls && len(calls) > 0
}

// filterToolCalls 按 keep 集合裁剪 tool_calls。
func filterToolCalls(message map[string]any, keep map[string]bool) []any {
	calls, _ := message["tool_calls"].([]any)
	out := make([]any, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		id, _ := call["id"].(string)
		if keep[strings.TrimSpace(id)] {
			out = append(out, call)
		}
	}
	return out
}

// IsTruncatedArguments 判断工具调用参数是否为残缺 JSON。
//
// 空串是合法的（无参工具）；非空但解析不了说明响应被截断。
func IsTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var value any
	return json.Unmarshal([]byte(trimmed), &value) != nil
}

// DropTruncatedToolCalls 丢弃参数残缺的工具调用。
//
// 在 finish_reason 为 length（输出被截断）或流没有正常结束时调用：
// 那种情况下的工具参数必然是不完整的 JSON，交给客户端只会得到解析错误。
// 能解析但类型不对的（标量/数组）不丢，留给客户端的 schema 校验处理。
func DropTruncatedToolCalls(calls []any) []any {
	out := make([]any, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			out = append(out, item)
			continue
		}
		function, okFunction := call["function"].(map[string]any)
		if !okFunction {
			out = append(out, call)
			continue
		}
		arguments, _ := function["arguments"].(string)
		if IsTruncatedArguments(arguments) {
			continue
		}
		out = append(out, call)
	}
	return out
}

// StripToolCallNames 让每个 tool_call 的 function.name 只出现在首片。
//
// 累加型客户端会做 name += delta.name，若每片都带 name 就会拼成
// "BashBashBash"；覆盖型客户端（name ?? state.name）则不会被空值清空。
// 因此「首片保留、后续删除该键」是同时满足两类客户端的做法。
func StripToolCallNames(frame map[string]any, seen map[int]bool) {
	choices, okChoices := frame["choices"].([]any)
	if !okChoices {
		return
	}
	for _, item := range choices {
		choice, okChoice := item.(map[string]any)
		if !okChoice {
			continue
		}
		delta, okDelta := choice["delta"].(map[string]any)
		if !okDelta {
			continue
		}
		calls, okCalls := delta["tool_calls"].([]any)
		if !okCalls {
			continue
		}
		for _, callItem := range calls {
			call, okCall := callItem.(map[string]any)
			if !okCall {
				continue
			}
			index := core.IntFromAny(call["index"])
			function, okFunction := call["function"].(map[string]any)
			if !okFunction {
				continue
			}
			if seen[index] {
				delete(function, "name")
				continue
			}
			if _, okName := function["name"]; okName {
				seen[index] = true
			}
		}
	}
}

// 本文件实现出站请求体的指纹脱敏。
//
// 背景：客户端（Claude Code 类 CLI）会在 system prompt 里注入若干固定模板句，
// 上游的内容审核是**逐字精确匹配**（不是语义审核），一字改动即可绕过。
//
// 策略分两层：
//   - 键值/header 型指纹：整段剥离（它本身就是注入痕迹，不含用户语义）；
//   - 承载语义的模板句：最小改写（换一个词），语义不变。
//
// 规则表从 catalog 同级的 sanitize_rules.json 读取（内嵌），
// 便于审计「到底改了哪些字面量」，也避免 Go 源码里出现大量易被误改的临界字符串。

//go:embed assets/sanitize_rules.json
var sanitizeRulesRaw []byte

// sanitizeRules 是脱敏规则。
type sanitizeRules struct {
	// Features 是特征预检串：任一命中才进入净化（普通请求零开销）。
	Features []string `json:"features"`
	// Rewrites 是 [原文, 改写] 对。
	Rewrites [][2]string `json:"rewrites"`
	// Regexes 是整段剥离用的正则（按名字取用）。
	Regexes map[string]string `json:"regexes"`
}

var (
	sanitizeOnce    sync.Once
	sanitizeRuleSet sanitizeRules
	sanitizeRegexes map[string]*regexp.Regexp
)

func loadSanitizeRules() (sanitizeRules, map[string]*regexp.Regexp) {
	sanitizeOnce.Do(func() {
		var rules sanitizeRules
		if errUnmarshal := json.Unmarshal(sanitizeRulesRaw, &rules); errUnmarshal != nil {
			// 内嵌资源损坏是构建期问题：没有规则就退化为「不脱敏」，
			// 而不是让插件启动失败（脱敏是加分项，不是必需项）。
			rules = sanitizeRules{}
		}
		compiled := map[string]*regexp.Regexp{}
		for name, pattern := range rules.Regexes {
			if strings.TrimSpace(pattern) == "" {
				continue
			}
			expr, errCompile := regexp.Compile(pattern)
			if errCompile != nil {
				// 规则写坏时跳过该条，而不是 panic。
				continue
			}
			compiled[name] = expr
		}
		sanitizeRuleSet = rules
		sanitizeRegexes = compiled
	})
	return sanitizeRuleSet, sanitizeRegexes
}

// sanitizeMessages 净化请求体里的全部文本。
//
// 净化范围：content（字符串或 parts 里的文本）、reasoning/reasoning_content、
// tool_calls 的参数（它是字符串化 JSON，按文本走）。**图片 part 不动**。
//
// 返回值报告是否发生了改动。
func sanitizeMessages(messages []any) bool {
	rules, regexes := loadSanitizeRules()
	changed := false
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		// 注意：这里**不能**在 content 缺失时 continue。
		// 工具调用消息的 content 通常是 null，早期版本因此在 content 缺失时跳过，
		// 导致 tool_calls 里的参数完全不被净化——写进工具参数（文件名、命令、
		// 写入内容）的被拦字符串会原样漏出。
		if content, okContent := message["content"]; okContent {
			if updated, didChange := sanitizeContent(content, rules, regexes); didChange {
				message["content"] = updated
				changed = true
			}
		}
		for _, key := range []string{"reasoning", "reasoning_content"} {
			if text, okText := message[key].(string); okText {
				if updated := sanitizeText(text, rules, regexes); updated != text {
					message[key] = updated
					changed = true
				}
			}
		}
		if calls, okCalls := message["tool_calls"].([]any); okCalls {
			if sanitizeToolCalls(calls, rules, regexes) {
				changed = true
			}
		}
	}
	return changed
}

// sanitizeContent 净化消息内容（字符串或 parts 数组）。
func sanitizeContent(content any, rules sanitizeRules, regexes map[string]*regexp.Regexp) (any, bool) {
	switch typed := content.(type) {
	case string:
		updated := sanitizeText(typed, rules, regexes)
		return updated, updated != typed
	case []any:
		changed := false
		for _, item := range typed {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			// 图片 part 不动：净化文本字段之外的内容会破坏图片。
			if partType, okType := part["type"].(string); okType && partType == "image_url" {
				continue
			}
			text, okText := part["text"].(string)
			if !okText {
				continue
			}
			if updated := sanitizeText(text, rules, regexes); updated != text {
				part["text"] = updated
				changed = true
			}
		}
		return typed, changed
	}
	return content, false
}

// sanitizeToolCalls 净化工具调用的参数与名称。
func sanitizeToolCalls(calls []any, rules sanitizeRules, regexes map[string]*regexp.Regexp) bool {
	changed := false
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		function, okFunction := call["function"].(map[string]any)
		if !okFunction {
			continue
		}
		if arguments, okArgs := function["arguments"].(string); okArgs {
			if updated := sanitizeText(arguments, rules, regexes); updated != arguments {
				function["arguments"] = updated
				changed = true
			}
		}
	}
	return changed
}

// hasFingerprint 特征预检：快速判断文本是否需要净化。
//
// 用 strings.Contains 走快路径：绝大多数请求全不命中，
// 直接跳过后面所有正则与改写（零分配）。
func hasFingerprint(text string, rules sanitizeRules) bool {
	for _, feature := range rules.Features {
		if strings.Contains(text, feature) {
			return true
		}
	}
	return false
}

// sanitizeText 对一段文本执行完整脱敏。
//
// 执行顺序：
//  1. 特征预检（不命中直接返回原串）；
//  2. 改写表（承载语义的模板句，逐词替换）；
//  3. header 键值段整段剥离；
//  4. cc_ 键值段循环清理（直到稳定）；
//  5. 裸 header 键名缩写。
func sanitizeText(text string, rules sanitizeRules, regexes map[string]*regexp.Regexp) string {
	if text == "" || !hasFingerprint(text, rules) {
		return text
	}
	for _, pair := range rules.Rewrites {
		if len(pair) != 2 || pair[0] == "" {
			continue
		}
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	if expr := regexes["sanitizeHdrRe"]; expr != nil {
		text = expr.ReplaceAllString(text, "")
	}
	if expr := regexes["sanitizeKvRe"]; expr != nil {
		// 循环清理直到稳定：一条规则剥掉前缀后可能让下一条匹配露出来。
		for previous := ""; previous != text; {
			previous = text
			text = expr.ReplaceAllString(text, "")
		}
	}
	if expr := regexes["sanitizeBareHdrRe"]; expr != nil {
		text = expr.ReplaceAllStringFunc(text, func(match string) string {
			// 把裸键名缩写成短形式，保留可读性同时不再是原字面量。
			if len(match) > 4 {
				return match[:len(match)-3] + "hdr"
			}
			return match
		})
	}
	return strings.TrimSpace(text)
}
