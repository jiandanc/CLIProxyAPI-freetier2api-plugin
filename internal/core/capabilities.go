package core

// 本文件定义「可选能力接口」与共享助手。
//
// 设计原则（Go 版的接口隔离）：
//   - Vendor 只保留每家都必然具备的能力；
//   - 登录/续期/额度/签到/任务/配置/存储合并等能力各自是一个小接口；
//   - 调用方用类型断言按需取用，不支持的供应商**什么都不用写**；
//   - 禁止用「空实现 + 恒 false」模拟 Java 的 default 方法——那会让
//     "不支持"与"忘了实现"无法区分。

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// Meta 是供应商适配器的身份信息（组合进 adapter，替代旧的 BaseVendor）。
//
// 字段名与接口方法名不同（VendorID/ID、Prefix/ModelPrefix）：Go 不允许
// 同名字段与方法共存。
type Meta struct {
	VendorID     string
	VendorName   string
	VendorRegion string
	// Prefix 是注册模型时的 ID 前缀，空表示裸名。
	Prefix string
}

// ID 返回供应商实例标识（同时是凭证文件名前缀）。
func (m *Meta) ID() string { return m.VendorID }

// Name 返回管理端展示名。
func (m *Meta) Name() string { return m.VendorName }

// Region 返回区域标识（cn / global；无区域概念时为空）。
func (m *Meta) Region() string { return m.VendorRegion }

// ModelPrefix 返回模型注册前缀。
func (m *Meta) ModelPrefix() string { return m.Prefix }

// ModelsProvider 由「按凭证返回可用模型」与静态清单不同的供应商实现。
//
// 未实现时调用方回落到 StaticModels（绝大多数供应商两者同源）。
type ModelsProvider interface {
	ModelsForAuth(ctx context.Context, cred *Credential) ([]pluginapi.ModelInfo, error)
}

// LoginProvider 由提供多步登录（设备码 / OAuth）的供应商实现。
//
// **实现它即表示支持登录**：控制台据此显示「添加账号」入口。
// 纯 API key 的供应商不实现即可，不再需要返回「不支持」的空方法。
type LoginProvider interface {
	LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error)
	LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error)
}

// LoginSessionOwner 由「会话表自持、不登记到公共 LoginSessions」的供应商实现。
//
// 根层按 state 路由登录请求时先查公共登记表；查不到就逐个问供应商「这个会话
// 是不是你的」。Trae 家族把会话存在自己的 SessionStore（回调监听与实例隔离
// 要求如此），没有登记进公共表；缺了这层自证，它的轮询会被路由层当成
// 「会话不存在」拒绝——登录走到一半就报「登录会话不存在或已过期」。
type LoginSessionOwner interface {
	OwnsLoginSession(sessionID string) bool
}

// CallbackSubmitResult 是「粘贴 localhost 回调链接」的处理结果。
//
// 刻意不复用 pluginapi.AuthLoginPollResponse：那个结构是对宿主契约的响应，
// 里面没有位置放「用户还得去点开的那个上游登录页」（CodeArts 的回调里带着它）。
type CallbackSubmitResult struct {
	// Status 是本次提交后的登录状态（成功 / 仍在等待 / 失败）。
	Status pluginapi.AuthLoginStatus
	// Message 是给用户看的进度或错误说明。
	Message string
	// Auth 是登录成功时产出的凭证记录。
	Auth pluginapi.AuthData
	// RedirectURL 是用户还需要在新标签页打开的上游登录页（CodeArts 专用）。
	RedirectURL string
}

// CallbackSubmitter 由「上游只认 localhost 回调」的供应商实现。
//
// 上游授权成功后浏览器会跳向 127.0.0.1，插件与浏览器不在同一台机器时（远程
// 主机、Docker、WSL）这条回调永远到不了，登录会一直卡在等待。这类供应商把
// 回调链接原样交给用户：控制台展示本机回调地址，用户把浏览器停住的那条链接
// 粘回来，插件在本地解析完成登录，而不是干等一个不会到达的 HTTP 请求。
type CallbackSubmitter interface {
	// LocalCallbackHint 返回页面应展示的 localhost 回调地址（供用户辨认）。
	LocalCallbackHint() string
	// SubmitCallback 解析用户粘贴的回调链接并推进当前登录会话。
	SubmitCallback(ctx context.Context, state, rawURL string) (*CallbackSubmitResult, error)
}

// SMSLoginProvider 由支持「手机号 + 短信验证码」登录的供应商实现。
//
// 两步交互与 LoginProvider 的「发起/轮询」模型不同：验证码要用户从手机上
// 读出来回填，控制台据此渲染专用的两步表单（发码 → 输码登录）。
// 实现它即表示控制台「添加账号」入口显示手机号表单。
type SMSLoginProvider interface {
	// SendLoginCode 发送短信验证码，返回本次登录会话的关联键
	// （后续 VerifyLoginCode 必须带回它；AutoClaw 是 deviceId）。
	SendLoginCode(ctx context.Context, phone string) (sessionKey string, err error)
	// VerifyLoginCode 用验证码完成登录，返回落盘凭证 JSON 与展示标签。
	VerifyLoginCode(ctx context.Context, sessionKey, phone, code string) (storageJSON []byte, label string, err error)
}

// Refresher 由需要校验/续期凭证的供应商实现。
//
// 未实现时调用方视为「已校验且无需写盘」（refreshed=false）。
type Refresher interface {
	Refresh(ctx context.Context, cred *Credential) (updated *Credential, refreshed bool, err error)
}

// QuotaProvider 由上游提供额度查询的供应商实现。
type QuotaProvider interface {
	Quota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error)
}

// CheckinProvider 由提供签到的供应商实现。
//
// SupportsCheckin 仍然保留：同一供应商的不同区域可能只有部分区域有活动
// （WorkBuddy 国内版有、国际版没有）。
type CheckinProvider interface {
	SupportsCheckin() bool
	Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error)
}

// CheckinCapability 由「能按账号判定签到」的供应商实现。
//
// 供应商级的 SupportsCheckin 只能表达「这个供应商/区域有没有签到活动」，
// 表达不了「同类凭证里有一部分账号天生不支持签到」——WorkBuddy 企业账号
// 打 daily-checkin 会被上游以 400 code 10001 拒绝。实现该接口后，控制台
// 按钮、自动任务与额度重置都按账号判定，避免出现必然失败的入口。
type CheckinCapability interface {
	// SupportsCheckinFor 报告该凭证是否支持签到。
	SupportsCheckinFor(cred *Credential) bool
	// CheckinUnsupportedReason 返回不可签到的可读原因（可签到或未知时为空串）。
	CheckinUnsupportedReason(cred *Credential) string
}

// AuthModeReporter 由「凭证本身就是敏感材料」的供应商实现。
//
// 目前只有 apikey 型供应商需要它：页面据此脱敏展示。
type AuthModeReporter interface {
	AuthMode() string
}

// StorageMerger 由「刷新后需要把新凭证合并回原文件」的供应商实现。
type StorageMerger interface {
	MergeStorageJSON(original []byte, credential *Credential) ([]byte, error)
}

// Configurable 由需要读取供应商专属配置的实例实现。
//
// 根层只解析全局键，其余原始键原样交给各供应商自己解释；
// raw 是 key → 原始字符串，实现必须容忍缺失键与 nil map。
type Configurable interface {
	Configure(settings Settings, raw map[string]string)
}

// CreditsProvider 由能给出模型计费倍率的供应商实现。
//
// 返回「官方模型 ID → 展示文案」（如 "0.08X"、"免费"）。宿主契约里没有倍率
// 字段，控制台只能通过这个能力拿到；不实现即视为「未知」，页面显示「—」。
//
// 单独成一个窄接口，而不是塞进 ConsoleModelProvider：倍率与「模型清单长什么样」
// 是两件事，供应商只需给出自己独有的那一维信息，其余明细由 Catalog 推导。
type CreditsProvider interface {
	ModelCredits() map[string]string
}

// FormatRate 把上游各种原始倍率串（如 "x0.71 credits"、"x0.05"、"0.5"）
// 归一化为统一的始终保留两位小数的 "0.50X" 样式；非数值文案（如 "免费"、"订阅"）原样保留；
// 负数与 NaN/Inf 视为「未知」返回空串（上游常用它们表示缺数据）。
func FormatRate(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// 去掉多余的 credits / credit 等单位
	lower := strings.ToLower(s)
	clean := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(lower, "credits"), "credit"))
	clean = strings.TrimPrefix(clean, "x")
	clean = strings.TrimSuffix(clean, "x")
	clean = strings.TrimSpace(clean)
	if num, err := strconv.ParseFloat(clean, 64); err == nil {
		// 负数倍率不存在；NaN/Inf 是上游把缺数据写成占位符，展示出来会变成
		// 一个看得见的假倍率。这两种情况一律当「未知」。
		if math.IsNaN(num) || math.IsInf(num, 0) || num < 0 {
			return ""
		}
		return fmt.Sprintf("%.2fX", num)
	}
	return s
}

// ConsoleModel 是供应商提供的模型展示明细（控制台模型页用）。
type ConsoleModel struct {
	ID              string
	Name            string
	Description     string
	ContextLength   int64
	MaxOutputTokens int64
	Efforts         []string
	DefaultEffort   string
	SupportsImages  bool
	SupportsTools   bool
	Credits         string
}

// ConsoleModelsFromInfo 把注册清单转成控制台展示条目，倍率按官方模型 ID 补充。
//
// 只给倍率表的供应商用它就能完成接入：明细字段全部来自注册清单，不另存
// 一份模型元数据，也就不会出现两处慢慢对不上。
func ConsoleModelsFromInfo(models []pluginapi.ModelInfo, credits map[string]string) []ConsoleModel {
	out := make([]ConsoleModel, 0, len(models))
	for _, model := range models {
		entry := ConsoleModel{
			ID:              model.ID,
			Name:            FirstNonEmpty(model.DisplayName, model.Name),
			Description:     model.Description,
			ContextLength:   model.ContextLength,
			MaxOutputTokens: model.MaxCompletionTokens,
			SupportsImages:  ModelSupportsImages(model),
			SupportsTools:   ModelSupportsTools(model),
			Credits:         credits[model.ID],
		}
		if model.Thinking != nil {
			entry.Efforts = model.Thinking.Levels
			if len(model.Thinking.Levels) > 0 {
				entry.DefaultEffort = model.Thinking.Levels[0]
			}
		}
		out = append(out, entry)
	}
	return out
}

// ModelCacheLoader 由需要预热自身模型目录的供应商实现。
//
// state.json 中的缓存按 VendorID 区分；Manager 在启动时把对应条目交给
// 该供应商，避免根层直接依赖某个具体供应商包。
type ModelCacheLoader interface {
	LoadModelCache(cached []CachedModel)
}

// ConsoleModelProvider 由需要向控制台补充展示明细的供应商实现。
//
// 未实现时控制台直接用 Catalog 缓存的注册清单（模型 ID、上下文、输出上限、
// 能力等都能从 pluginapi.ModelInfo 推导），只是没有倍率这类宿主契约之外的字段。
type ConsoleModelProvider interface {
	ConsoleModels() []ConsoleModel
}

// As 把供应商断言为某个可选能力接口。
//
// 用法：if quota, ok := core.As[core.QuotaProvider](vendor); ok { ... }
func As[T any](vendor Vendor) (T, bool) {
	value, okCapability := any(vendor).(T)
	return value, okCapability
}

// SupportsCheckin 报告供应商是否提供签到。
func SupportsCheckin(vendor Vendor) bool {
	provider, okProvider := As[CheckinProvider](vendor)
	return okProvider && provider.SupportsCheckin()
}

// SupportsCheckinFor 报告某个凭证是否支持签到。
//
// 未实现 CheckinCapability 的供应商回落到供应商级判定，保持既有行为。
func SupportsCheckinFor(vendor Vendor, cred *Credential) bool {
	if !SupportsCheckin(vendor) {
		return false
	}
	capability, okCapability := As[CheckinCapability](vendor)
	if !okCapability {
		return true
	}
	return capability.SupportsCheckinFor(cred)
}

// CheckinUnsupportedReason 返回某凭证不可签到的可读原因；无原因时返回空串。
func CheckinUnsupportedReason(vendor Vendor, cred *Credential) string {
	capability, okCapability := As[CheckinCapability](vendor)
	if !okCapability {
		return ""
	}
	return strings.TrimSpace(capability.CheckinUnsupportedReason(cred))
}

// ModelsFor 返回某凭证可用的模型：可选实现优先，否则回落 StaticModels。
//
// cred 为空时返回空清单（宿主不会为空凭证注册模型）。
func ModelsFor(vendor Vendor, ctx context.Context, cred *Credential) ([]pluginapi.ModelInfo, error) {
	if provider, okProvider := As[ModelsProvider](vendor); okProvider {
		return provider.ModelsForAuth(ctx, cred)
	}
	if cred == nil {
		return nil, nil
	}
	return vendor.StaticModels(ctx)
}

// RefreshVendor 校验/续期凭证：无 Refresher 的供应商视为已校验。
func RefreshVendor(vendor Vendor, ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if refresher, okRefresher := As[Refresher](vendor); okRefresher {
		return refresher.Refresh(ctx, cred)
	}
	return cred, false, nil
}

// EstimateTokens 按字节数估算 token（偏保守，倾向高估）。
func EstimateTokens(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	tokens := len(payload) / 3
	if tokens < 1 {
		return 1
	}
	return tokens
}

// QuotaUnsupported 是「上游没有额度接口」的统一错误。
func QuotaUnsupported(vendor Vendor) error {
	return NewPluginError("quota_unsupported",
		vendor.Name()+" 上游不提供额度查询接口（额度以订阅页为准）", http.StatusNotImplemented)
}

// LoginUnsupported 是「上游没有登录流程」的统一错误。
func LoginUnsupported(vendor Vendor) error {
	return NewPluginError("login_unsupported",
		vendor.Name()+" 不支持登录：请用「添加 API Key」方式添加", http.StatusNotImplemented)
}

// ConfigFieldsProvider 由需要声明 YAML 配置字段的供应商实现。
type ConfigFieldsProvider interface {
	ConfigFields() []pluginapi.ConfigField
}

// ── 套餐领取 ────────────────────────────────────────────────────────────────
//
// 有些供应商的「领取套餐」需要上游发放的活动套餐，并强制要求人机验证令牌
// （ZCode 的阿里云无痕验证）。求解器不适合塞进插件进程，因此契约只要求供应
// 商给出 **SDK 初始化参数**，由管理页在用户浏览器里渲染控件，用户通过后把
// 令牌回传给插件转发。
//
// 三个方法合成一个接口：它们描述的是同一条人工领取链路，总是一起被实现、
// 一起被调用（预览页要 SDK 参数才能渲染控件，领取要用预览结果选套餐）。
// 拆成三个小接口只会让调用点多写两次类型断言。

// LoginVariant 是「同一供应商有多个 OAuth 身份源」时控制台要渲染的一个入口。
//
// ID 会放进 LoginStart 的 metadata["oauth_variant"]，由供应商自己解释
// （AutoClaw 是 zai / google）；CallbackHint 是该变体在浏览器里会停住的
// localhost 回调地址，页面据此提示用户把哪条链接粘回来。
type LoginVariant struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	CallbackHint string `json:"callback_hint,omitempty"`
}

// LoginVariantProvider 由「登录有多个变体」的供应商实现。
//
// 实现即表示控制台要把这些变体渲染成按钮；不实现则仍按单一登录入口处理。
type LoginVariantProvider interface {
	LoginVariants() []LoginVariant
}

// LoginCaptchaProvider 由「登录前必须在浏览器里过一道人机验证」的供应商实现。
//
// 登录与套餐领取的差别：套餐领取拿到令牌就去领取；登录拿到令牌只是换授权地址的
// 前置条件，后续还有「浏览器登录 → 回调链接粘贴 → 换码」。因此单独成一个能力，
// 而不是复用 PlanClaimer（那会让供应商被迫实现不相干的 PreviewPlans/ClaimPlan）。
type LoginCaptchaProvider interface {
	// LoginCaptchaConfig 返回控件初始化参数（当前是阿里云场景 ID/region/prefix）。
	LoginCaptchaConfig(ctx context.Context) (CaptchaConfig, error)
}

// CaptchaConfig 是人机验证 SDK 的初始化参数。
//
// 前端据此渲染控件，拿到令牌后原样回传 VerifyParam；Region 为空表示上游
// 未下发地域，此时不带 Verify-Region 头。
type CaptchaConfig struct {
	Enabled bool   `json:"enabled"`
	SceneID string `json:"scene_id"`
	Region  string `json:"region"`
	Prefix  string `json:"prefix"`
}

// PlanGrant 是套餐授予的一项额度（通常是某模型的 token 额度）。
type PlanGrant struct {
	Model  string  `json:"model"`
	Units  float64 `json:"units"`
	Period string  `json:"period,omitempty"`
}

// PlanInfo 是一个可领取的套餐。
type PlanInfo struct {
	PlanID      string      `json:"plan_id"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Priority    int         `json:"priority,omitempty"`
	Grants      []PlanGrant `json:"grants,omitempty"`
	StartsAt    int64       `json:"starts_at,omitempty"`
	EndsAt      int64       `json:"ends_at,omitempty"`
}

// ClaimPlanRequest 是一次套餐领取请求。
//
// PlanID 为空表示由供应商自行挑选优先级最高的未领套餐；VerifyParam 是用户在
// 浏览器里完成人机验证后拿到的令牌。
type ClaimPlanRequest struct {
	PlanID       string `json:"plan_id"`
	VerifyParam  string `json:"verify_param"`
	VerifyRegion string `json:"verify_region,omitempty"`
}

// ClaimPlanResponse 是领取成功的结果。
type ClaimPlanResponse struct {
	PlanID   string `json:"plan_id"`
	PlanName string `json:"plan_name,omitempty"`
	StartsAt int64  `json:"starts_at,omitempty"`
	EndsAt   int64  `json:"ends_at,omitempty"`
}

// PlanClaimer 由投放活动套餐的供应商实现（当前只有 ZCode 的 Coding Plan）。
//
// 未实现时管理页不显示领取入口——不允许对没有套餐概念的供应商发起领取。
type PlanClaimer interface {
	// CaptchaConfig 给出人机验证控件的初始化参数。
	CaptchaConfig(ctx context.Context) (CaptchaConfig, error)
	// PreviewPlans 返回账号当前可领的套餐（已剔除已持有的）。
	// 空切片表示没有可领的，页面据此把按钮置灰成「已领取」。
	PreviewPlans(ctx context.Context, cred *Credential) ([]PlanInfo, error)
	// ClaimPlan 领取指定套餐；req.PlanID 为空时由供应商自行挑选。
	ClaimPlan(ctx context.Context, cred *Credential, req ClaimPlanRequest) (ClaimPlanResponse, error)
}

// ActivityProbeResult 是一次只读活动探测的结果。
//
// Activities 复用 PlanInfo：上游投放的活动与套餐同构（同一个 plan_id、同一份
// 授予明细），另立一套字段只会多一次逐字段搬运。它**不代表可领**——是否已持有、
// 是否在领取窗口内由供应商判断，这里如实返回上游当前投放。
// Warning 说明本次结果可能不完整的原因（例如活跃上报失败），空串表示链路完整；
// 有 Activities 时 Warning 也可能非空。
type ActivityProbeResult struct {
	Activities []PlanInfo `json:"activities"`
	Warning    string     `json:"warning,omitempty"`
}

// ActivityProbe 是只读的活动探测能力，与套餐领取能力分离。
// 实现可以在探测前执行必要的无副作用资格同步（例如日活事件），但不得领取。
type ActivityProbe interface {
	ProbeActivity(ctx context.Context, cred *Credential) (ActivityProbeResult, error)
}
