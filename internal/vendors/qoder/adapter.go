package qoder

// 本文件把 Qoder 协议适配成 core.Vendor。
//
// 分界：
//   - 本包其余文件是**纯协议层**（cosy 签名、bridge 协议转换、端点表、
//     签到、额度、登录），不知道宿存在；
//   - 本文件是**适配层**，把全局配置与宿主端口接上去。
//
// 与 WorkBuddy 的关键差异：Qoder 的每次上游调用都需要一个 cosy 会话
// （含 RSA 加密的 cosyKey 与设备指纹），建会话要一次 jobToken 交换，
// 因此这里必须缓存 Bridge——不能每个请求重建。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

const (
	// qoderBridgeTTL 决定 Bridge（含 cosy 会话）最长复用时间。
	//
	// 上游一旦改签名规则，最长这么多时间后会自动重建，不必重启插件。
	qoderBridgeTTL = 30 * time.Minute
	// qoderBridgeTimeout 限制单次建连时间（含 jobToken 交换）。
	qoderBridgeTimeout = 30 * time.Second
)

// adapter 是 core.Vendor 在 Qoder 上的实现。
type adapter struct {
	*core.NativeAdapter[*Credential]
	region Region
	// bridges 按凭证指纹缓存 cosy 会话（建会话要一次 jobToken 交换，很贵）。
	bridges sync.Map
	// models 是「对外模型 ID → 上游 key」索引，随每次模型清单刷新整体替换。
	//
	// 必须挂在实例上：国内版与国际版的同名上游 key 可能对应不同模型。
	models modelIndex
	// credits 是「对外模型 ID → 计费倍率文案」，与 models 同源同生命周期。
	//
	// 控制台要展示倍率，但宿主契约（pluginapi.ModelInfo）没有这个字段，
	// 只能由本实例在转换模型时顺手记下，供 ConsoleModels 回读。
	credits core.CreditTable
	// settings 是原始配置键（设备身份需要读 qoder_desktop_version）。
	settings atomic.Value // map[string]string
}

// Instances 构造 Qoder 的两个区域实例。
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{newInstance(env, RegionCN), newInstance(env, RegionGlobal)}
}

func newInstance(env core.Env, region Region) core.Vendor {
	instance := &adapter{
		NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
			Meta: core.Meta{VendorID: VendorIDFor(region), VendorName: VendorNameFor(region), VendorRegion: string(region)},
			Env:  env,
			Extract: func(c *core.Credential) (*Credential, bool) {
				native, ok := c.Native.(*Credential)
				return native, ok
			},
			ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
				native, errParse := ParseCredential(raw, region)
				if errParse != nil {
					return core.CredentialSeed{}, nil, errParse
				}
				if native.Region != region {
					return core.CredentialSeed{}, nil, fmt.Errorf("credential region %q does not match vendor %q", native.Region, VendorIDFor(region))
				}
				label := native.Label
				if label == "" {
					label = native.Email
				}
				if label == "" {
					label = fileName
				}
				return core.CredentialSeed{
					VendorID: VendorIDFor(region), Region: string(native.Region), Label: label,
					FileID: core.FileIDFromFileName(fileName), Token: native.Token,
					RefreshToken: native.RefreshToken, AuthMode: "oauth",
				}, native, nil
			},
			RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
				updated, refreshed, errRefresh := RefreshCredential(ctx, native)
				if errRefresh != nil {
					return core.NativeRefresh[*Credential]{}, errRefresh
				}
				return core.NativeRefresh[*Credential]{
					Native: updated, Token: updated.Token, RefreshToken: updated.RefreshToken, Changed: refreshed,
				}, nil
			},
		}),
		region: region,
	}
	instance.settings.Store(map[string]string{})
	return instance
}

// Configure 保存 Qoder 专属配置（设备身份的桌面端版本覆盖）。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	snapshot := make(map[string]string, len(raw))
	for key, value := range raw {
		snapshot[key] = value
	}
	v.settings.Store(snapshot)
}

// Match 判断一份凭证是否属于本区域。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	if !LooksLikeCredential(raw, fileName, provider) {
		return false
	}
	if vendorID, okVendor := raw[core.VendorKey].(string); okVendor {
		return strings.EqualFold(strings.TrimSpace(vendorID), v.ID())
	}
	if region := RegionForCredential(raw); region != "" {
		return region == v.region
	}
	return v.defaultRegionForParse() == v.region
}

func (v *adapter) defaultRegionForParse() Region {
	settings := v.Env.Settings()
	for _, vendor := range v.Env.Registry.Vendors() {
		if vendor.Region() == string(RegionGlobal) && settings.RealmEnabled(vendor.Region()) {
			return RegionGlobal
		}
	}
	return RegionCN
}

// ---- 模型 ----

// StaticModels 返回本区域的模型清单。
//
// 需要一次上游调用（列可用模型），因此借一个同区域的账号建 Bridge。
// 失败时返回错误而不是编造清单：注册一份写死的模型目录只会随上游过时，
// 让客户端看到并不存在的模型。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := v.fetchModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// fetchModels 借探测凭证拉取上游模型清单。
func (v *adapter) fetchModels(ctx context.Context) ([]Model, error) {
	cred, okCred := core.ProbeNativeCredential(v.Env, v.ID(), func(c *core.Credential) (*Credential, bool) {
		native, okNative := c.Native.(*Credential)
		return native, okNative
	})
	if !okCred || cred == nil {
		return nil, fmt.Errorf("no credential available for model probe")
	}
	b, errBridge := v.bridgeFor(ctx, cred)
	if errBridge != nil {
		return nil, errBridge
	}
	return b.ListAvailableModels(ctx)
}

// ModelsForAuth 返回该凭证可用的模型。
//
// 失败时返回错误而不是兜底清单：宿主会跳过该凭证，比让客户端看到一个
// 写死的、可能已不存在的模型目录更诚实。
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	if cred == nil || cred.RegionValue() != string(v.region) {
		// 区域不符时返回空清单而不是报错：宿主会因此跳过该凭证，
		// 这正是我们想要的结果（避免用错域的凭证打上游）。
		return nil, nil
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	b, errBridge := v.bridgeFor(ctx, native)
	if errBridge != nil {
		return nil, errBridge
	}
	models, errModels := b.ListAvailableModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	b, errBridge := v.bridgeFor(ctx, native)
	if errBridge != nil {
		return nil, errBridge
	}
	// 复用 bridge 的 chat-completions 组装逻辑，但把结果写进内存而不是
	// http.ResponseWriter——插件侧不做 HTTP 响应，只产出载荷。
	recorder := newPayloadRecorder()
	if errRun := RunChatCompletions(b, req.Payload, recorder); errRun != nil {
		return nil, errRun
	}
	return &pluginapi.ExecutorResponse{
		Payload: recorder.Payload(),
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	b, errBridge := v.bridgeFor(ctx, native)
	if errBridge != nil {
		return errBridge
	}
	return StreamChatCompletions(b, req.Payload, sink.Emit)
}

// ---- 额度与签到 ----

// Quota 查询账号额度。
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	quota, errQuota := FetchQuota(ctx, native)
	if errQuota != nil {
		return nil, errQuota
	}
	response := BuildQuotaResponse(quota)
	return &response, nil
}

// Checkin 执行一次签到。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	// 设备身份只取自「用户提供的真身份」（凭证字段或全局配置）。插件不会
	// 自行派生：上游对机器头做精确校验，派生值会让可领取活动被整条过滤，
	// 详见 identity.go。没有真身份时签到时一个机器头都不发。
	identity := ResolveIdentity(native, v.settingsSnapshot())
	result, errCheckin := Checkin(ctx, native, identity)
	if errCheckin != nil {
		return nil, errCheckin
	}
	return &core.CheckinResult{
		// 「今日已领取」由协议层判定：它只在**今天 10:00 开启的那一轮**
		// 已经领取时才成立。9 点那一轮看到的是上一轮已领，协议层会返回
		// CheckinWindowNotOpen（窗口未开启），不会走到这里。
		Already:     result.Status == CheckinAlreadyClaimed,
		Credit:      int64(result.Amount),
		Message:     result.Message,
		NotRecorded: result.NotRecorded,
	}, nil
}

// settingsSnapshot 返回配置键快照。
func (v *adapter) settingsSnapshot() map[string]string {
	if snapshot, okSnapshot := v.settings.Load().(map[string]string); okSnapshot {
		return snapshot
	}
	return nil
}

// ConfigFields 声明 Qoder 专属 YAML 配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "qoder_desktop_version", Type: pluginapi.ConfigFieldTypeString, Description: "cosy-version 桌面端版本覆盖。"},
		{Name: "qoder_machine_token", Type: pluginapi.ConfigFieldTypeString, Description: "官方组件签发的 machineToken，用于解锁国际版每日领取活动。"},
		{Name: "qoder_machine_type", Type: pluginapi.ConfigFieldTypeString, Description: "与 qoder_machine_token 配对的 machineType，必须成对填写。"},
		{Name: "qoder_machine_code", Type: pluginapi.ConfigFieldTypeString, Description: "可选的 machineCode（不填不影响活动可见性）。"},
	}
}

// ---- 登录与续期 ----

// LoginStart 发起一次 PKCE 设备授权登录。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	loginURL, state, expiresAt, errStart := LoginStart(ctx, v.Env.Logins, v.region)
	if errStart != nil {
		return nil, errStart
	}
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       loginURL,
		State:     state,
		ExpiresAt: expiresAt,
		Metadata: map[string]any{
			core.VendorKey: v.ID(),
			"region":       string(v.region),
		},
	}, nil
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	result, errPoll := LoginPoll(ctx, v.Env.Logins, state)
	if errPoll != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errPoll.Error(),
		}, nil
	}
	if result.Pending {
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	plan := FetchPlanName(ctx, result.Token, result.Region)
	storage, cred := BuildLoginStorage(result.Region, result.Token, result.RefreshToken, result.Profile, plan)
	authID := LoginAuthID(result.Profile, state)
	fileName := core.FileNameFor(v.ID(), strings.TrimPrefix(authID, "qoder-"))
	logger.Info("completed qoder login (vendor=%s email=%s)", v.ID(), cred.Email)
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			ID:          strings.TrimSuffix(fileName, ".json"),
			FileName:    fileName,
			Label:       cred.Label,
			StorageJSON: storage,
		},
	}, nil
}

// OwnsLoginSession 报告该会话是否由本供应商创建。
func (v *adapter) OwnsLoginSession(sessionID string) bool {
	return OwnsLoginSession(v.Env.Logins, sessionID)
}

// MergeStorageJSON 实现 core.StorageMerger：把续期后的令牌合并回原凭证 JSON。
//
// Qoder 的凭证是扁平字段（device_token / refresh_token / region / label），
// 只更新会变的那两个令牌字段，其余字段（用户手写的 label、secret 等）原样保留。
func (v *adapter) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	if len(original) == 0 {
		return original, nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(original, &payload); errUnmarshal != nil {
		return original, nil
	}
	token := credential.TokenValue()
	refreshToken := credential.RefreshTokenValue()
	if token != "" {
		// 只改已有的键名，不新增：用户可能用的是七个候选名之一
		// （token / device_token / access_token …），凭空加一个
		// device_token 会让文件里出现两份含义相同的令牌。
		if _, okDevice := payload["device_token"]; okDevice {
			payload["device_token"] = token
		} else if _, okToken := payload["token"]; okToken {
			payload["token"] = token
		} else {
			payload["device_token"] = token
		}
	}
	if refreshToken != "" {
		payload["refresh_token"] = refreshToken
	}
	merged, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return original, errMarshal
	}
	return merged, nil
}

// ---- Bridge 缓存 ----

// bridgeEntry 是缓存的一条 Bridge。
type bridgeEntry struct {
	bridge    *Bridge
	createdAt time.Time
}

// bridgeFor 取（或建立）该凭证的 cosy 会话。
//
// 缓存是必需的：建会话要一次 jobToken 交换 + RSA 加密，每个请求重建
// 会让上游看到大量重复的令牌交换。
func (v *adapter) bridgeFor(ctx context.Context, cred *Credential) (*Bridge, error) {
	key := v.bridgeCacheKey(cred)
	if cached, okCached := v.bridges.Load(key); okCached {
		entry, okEntry := cached.(bridgeEntry)
		if okEntry && time.Since(entry.createdAt) < qoderBridgeTTL {
			return entry.bridge, nil
		}
		v.bridges.Delete(key)
	}

	createCtx, cancel := context.WithTimeout(ctx, qoderBridgeTimeout)
	defer cancel()
	templateBase, errTemplate := TemplateBase()
	if errTemplate != nil {
		return nil, errTemplate
	}
	b, errBridge := NewBridge(createCtx, BridgeSecret(cred), v.region, templateBase, &v.models)
	if errBridge != nil {
		return nil, ClassifyCredentialError(errBridge)
	}
	v.bridges.Store(key, bridgeEntry{bridge: b, createdAt: time.Now()})
	return b, nil
}

// bridgeCacheKey 生成 Bridge 的缓存键。
//
// 用「凭证指纹 + 区域」：凭证轮换后必须重建会话（旧的签名密钥已失效）。
// 不直接存 token 明文——缓存键会进日志与内存 dump。
func (v *adapter) bridgeCacheKey(cred *Credential) string {
	return v.ID() + "|" + CredentialFingerprint(cred)
}

// ---- 内部工具 ----

// modelsToPluginAPI 把上游模型条目转成宿主契约类型。
//
// 对外模型 ID 用 display_name 规范化后的 slug（DeepSeek-V4-Pro → deepseek-v4-pro），
// 同时重建本实例的 slug → 上游 key 索引，出站时由 Bridge 精确还原成
// Qoder 加密网关要求的 model_config.key / x-model-key。
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	slugByKey := v.models.replace(models)
	credits := make(map[string]string, len(models))
	registered := core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		key := strings.TrimSpace(model.Key)
		if key == "" {
			return core.ModelDescriptor{}, false
		}
		id, okSlug := slugByKey[key]
		if !okSlug {
			return core.ModelDescriptor{}, false
		}
		name := strings.TrimSpace(model.DisplayName)
		if name == "" {
			name = key
		}
		if model.PriceFactor != nil {
			if rate := rateLabel(*model.PriceFactor); rate != "" {
				credits[id] = rate
			}
		}
		return core.ModelDescriptor{
			ID:                 id,
			Name:               name,
			Description:        describeModel(model),
			ContextLength:      int64(model.ContextWindow),
			MaxOutputTokens:    int64(model.MaxOutputTokens),
			SupportsTools:      true,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
	v.credits.Replace(credits)
	return registered
}

// ModelCredits 实现 core.CreditsProvider：返回「对外模型 ID → 计费倍率」。
//
// 只报倍率这一维信息，模型清单本身由目录层从注册结果推导。
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// rateLabel 把上游的 price_factor 转成形如 0.08X 的展示文案。
func rateLabel(factor float64) string {
	// 0 是合法倍率（限免），保留；负数与非有限值才算未提供。
	if factor < 0 || math.IsInf(factor, 0) || math.IsNaN(factor) {
		return ""
	}
	return fmt.Sprintf("%.2fX", factor)
}

// describeModel 拼接模型说明（带价格倍率）。
func describeModel(model Model) string {
	description := "Qoder 上游模型"
	if model.IsDefault {
		description += "（默认）"
	}
	if model.IsReasoning {
		description += "（推理）"
	}
	if model.PriceFactor != nil {
		description += fmt.Sprintf("，计费倍率 x%.2f", *model.PriceFactor)
	}
	return description
}

// payloadRecorder 把 HTTP 响应写进内存（插件侧不做真实 HTTP 响应）。
type payloadRecorder struct {
	status  int
	headers http.Header
	body    []byte
}

func newPayloadRecorder() *payloadRecorder {
	return &payloadRecorder{status: http.StatusOK, headers: http.Header{}}
}

func (r *payloadRecorder) Header() http.Header { return r.headers }

func (r *payloadRecorder) Write(data []byte) (int, error) {
	r.body = append(r.body, data...)
	return len(data), nil
}

func (r *payloadRecorder) WriteHeader(statusCode int) { r.status = statusCode }

// Payload 返回记录的响应体。
func (r *payloadRecorder) Payload() []byte { return r.body }

func (v *adapter) SupportsCheckin() bool { return true }

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.CheckinProvider      = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.CreditsProvider      = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.LoginSessionOwner    = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.StorageMerger        = (*adapter)(nil)
)
