package main

// 本文件把 Qoder 协议适配成 core.Vendor。
//
// 分界与 WorkBuddy 一致：
//   - internal/vendors/qoder 是**纯协议层**（cosy 签名、bridge 协议转换、
//     端点表、签到、额度、登录），不知道宿存在；
//   - 本文件是**适配层**，把根层的配置与宿主能力接上去。
//
// 与 WorkBuddy 的关键差异：Qoder 的每次上游调用都需要一个 cosy 会话
// （含 RSA 加密的 cosyKey 与设备指纹），建会话要一次 jobToken 交换，
// 因此这里必须缓存 Bridge——不能每个请求重建。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/qoder"
	"freetier2api-plugin/internal/vendors/qoder/bridge"
	"freetier2api-plugin/internal/vendors/qoder/qoderapi"
)

const (
	// qoderBridgeTTL 决定 Bridge（含 cosy 会话）最长复用时间。
	//
	// 上游一旦改签名规则，最长这么多时间后会自动重建，不必重启插件。
	qoderBridgeTTL = 30 * time.Minute
	// qoderBridgeTimeout 限制单次建连时间（含 jobToken 交换）。
	qoderBridgeTimeout = 30 * time.Second
)

// qoderVendor 是 core.Vendor 在 Qoder 上的实现。
type qoderVendor struct {
	region qoderapi.Region
	// bridges 按凭证指纹缓存 cosy 会话（建会话要一次 jobToken 交换，很贵）。
	bridges sync.Map
}

// newQoderVendors 构造 Qoder 的两个区域实例。
func newQoderVendors() []core.Vendor {
	return []core.Vendor{
		&qoderVendor{region: qoderapi.RegionCN},
		&qoderVendor{region: qoderapi.RegionGlobal},
	}
}

func (v *qoderVendor) ID() string     { return qoder.VendorIDFor(v.region) }
func (v *qoderVendor) Name() string   { return qoder.VendorNameFor(v.region) }
func (v *qoderVendor) Region() string { return string(v.region) }

// ModelPrefix 返回空：本供应商用裸模型名注册。
//
// Qoder 的模型 ID 用**人类可读名**（GLM-5.3 / Kimi-K3）而不是上游 SKU
// （gmodel / kmodel_latest）：CPA 的 /v1/models 与客户端下拉只显示模型 ID，
// 用 SKU 会让用户看到内部代号。代价是老客户端要改 ID，用 extra_models 兼容。
func (v *qoderVendor) ModelPrefix() string { return "" }

// ---- 凭证 ----

// Match 判断一份凭证是否属于本区域。
//
// 与 WorkBuddy 同样要求区域一致，但 Qoder 的凭证可能压根没有 region 字段
// （手写或旧格式），此时回落到配置兜底——否则这类凭证会两边都不认领。
func (v *qoderVendor) Match(fileName, provider string, raw map[string]any) bool {
	if !qoder.LooksLikeCredential(raw, fileName, provider) {
		return false
	}
	if vendorID, okVendor := raw[core.VendorKey].(string); okVendor {
		return strings.EqualFold(strings.TrimSpace(vendorID), v.ID())
	}
	if region := qoder.RegionForCredential(raw); region != "" {
		return region == v.region
	}
	return defaultQoderRegionForParse() == v.region
}

// defaultQoderRegionForParse 决定解析无区域声明的 Qoder 凭证时的兜底区域。
//
// Qoder 国际版是主站，因此默认 global（与 WorkBuddy 默认 cn 相反——
// 各家的主站不同，兜底必须跟着自己的实际情况走）。
func defaultQoderRegionForParse() qoderapi.Region {
	cfg := loadedConfig()
	for _, vendor := range core.Vendors() {
		if vendor.Region() == string(qoderapi.RegionGlobal) && realmEnabled(cfg, vendor.Region()) {
			return qoderapi.RegionGlobal
		}
	}
	return qoderapi.RegionCN
}

// Parse 把凭证 JSON 解析成 core.Credential。
func (v *qoderVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := qoder.ParseCredential(raw, v.region)
	if errParse != nil {
		return nil, errParse
	}
	if native.Region != v.region {
		return nil, fmt.Errorf("credential region %q does not match vendor %q", native.Region, v.ID())
	}
	return newQoderCoreCredential(native, v.ID(), fileName), nil
}

// newQoderCoreCredential 把协议层凭证包装成 core.Credential。
func newQoderCoreCredential(native *qoder.Credential, vendorID, fileName string) *core.Credential {
	label := native.Label
	if label == "" {
		label = native.Email
	}
	if label == "" {
		label = fileName
	}
	// Qoder 的 PAT 是长期凭证，没有过期时间概念；设备令牌同理。
	// 因此 ExpiresAt 留 0（表示未知），刷新由宿主按 NextRefreshAfter 驱动。
	return &core.Credential{
		VendorID:     vendorID,
		Region:       string(native.Region),
		Label:        label,
		UID:          native.Email,
		Token:        native.Token,
		RefreshToken: native.RefreshToken,
		AuthMode:     "oauth",
		Native:       native,
	}
}

// nativeCredential 取回协议层凭证对象。
func (v *qoderVendor) nativeCredential(cred *core.Credential) (*qoder.Credential, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", 401)
	}
	if native, okNative := cred.Native.(*qoder.Credential); okNative && native != nil {
		return native, nil
	}
	return nil, core.NewPluginError("credential_missing",
		"credential was not parsed by the qoder vendor", 401)
}

// ---- 模型 ----

// StaticModels 返回本区域的模型清单。
//
// 需要一次上游调用（列可用模型），因此借一个同区域的账号建 Bridge。
// 建不起来时退回内置兜底清单——没有模型注册比注册一份静态清单更糟。
func (v *qoderVendor) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := v.fetchModels(ctx)
	if errModels != nil {
		logger.Debug("qoder: list models failed for %s, using bundled list: %v", v.ID(), errModels)
		models = qoder.BundledQoderModels()
	}
	return qoderModelsToPluginAPI(models), nil
}

// fetchModels 借探测凭证拉取上游模型清单。
func (v *qoderVendor) fetchModels(ctx context.Context) ([]bridge.QoderModel, error) {
	cred := probeCredentialForVendor(v.ID())
	if cred == nil {
		return nil, fmt.Errorf("no credential available for model probe")
	}
	b, errBridge := v.bridgeFor(ctx, cred)
	if errBridge != nil {
		return nil, errBridge
	}
	return b.ListAvailableModels(ctx)
}

// ModelsForAuth 返回该凭证可用的模型。
func (v *qoderVendor) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	if cred == nil || cred.RegionValue() != string(v.region) {
		// 区域不符时返回空清单而不是报错：宿主会因此跳过该凭证，
		// 这正是我们想要的结果（避免用错域的凭证打上游）。
		return nil, nil
	}
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	b, errBridge := v.bridgeFor(ctx, native)
	if errBridge != nil {
		// 建桥失败同样退回兜底清单：账号本身可能是好的，只是暂时连不上。
		logger.Debug("qoder: bridge for model list failed: %v", errBridge)
		return qoderModelsToPluginAPI(qoder.BundledQoderModels()), nil
	}
	models, errModels := b.ListAvailableModels(ctx)
	if errModels != nil {
		return qoderModelsToPluginAPI(qoder.BundledQoderModels()), nil
	}
	return qoderModelsToPluginAPI(models), nil
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *qoderVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.nativeCredential(cred)
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
	if errRun := bridge.RunChatCompletions(b, req.Payload, recorder); errRun != nil {
		return nil, errRun
	}
	return &pluginapi.ExecutorResponse{
		Payload: recorder.Payload(),
		Headers: map[string][]string{"Content-Type": {jsonContentType}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
func (v *qoderVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return errNative
	}
	b, errBridge := v.bridgeFor(ctx, native)
	if errBridge != nil {
		return errBridge
	}
	return bridge.StreamChatCompletions(b, req.Payload, sink.Emit)
}

// CountTokens 估算 token 数（按字节数除以 3，不调上游）。
func (v *qoderVendor) CountTokens(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (int, error) {
	if len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

// ---- 额度与签到 ----

// Quota 查询账号额度。
func (v *qoderVendor) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	quota, errQuota := qoder.FetchQuota(ctx, native)
	if errQuota != nil {
		return nil, errQuota
	}
	response := qoder.BuildQuotaResponse(quota)
	return &response, nil
}

// SupportsCheckin 报告本区域是否提供签到。
//
// 两个区域都返回 true：国内版确实有签到；国际版**没有签到计划**，
// 但 Checkin 会返回明确的「该区域无每日签到活动」，这比在 UI 层
// 静默隐藏入口更有信息量（用户会知道不是自己配错了）。
func (v *qoderVendor) SupportsCheckin() bool { return true }

// Checkin 执行一次签到。
func (v *qoderVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	result, errCheckin := qoder.Checkin(ctx, native)
	if errCheckin != nil {
		return nil, errCheckin
	}
	return &core.CheckinResult{
		Already: result.Status == qoder.CheckinAlreadyClaimed,
		Credit:  int64(result.Amount),
		Message: result.Message,
	}, nil
}

// ---- 登录与续期 ----

// LoginStart 发起一次 PKCE 设备授权登录。
func (v *qoderVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	loginURL, state, expiresAt, errStart := qoder.LoginStart(ctx, v.region)
	if errStart != nil {
		return nil, errStart
	}
	return &pluginapi.AuthLoginStartResponse{
		Provider:  providerKey,
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
func (v *qoderVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	result, errPoll := qoder.LoginPoll(ctx, state)
	if errPoll != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errPoll.Error(),
		}, nil
	}
	if result.Pending {
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	plan := qoder.FetchPlanName(ctx, result.Token, result.Region)
	storage, cred := qoder.BuildLoginStorage(result.Region, result.Token, result.RefreshToken, result.Profile, plan)
	authID := qoder.LoginAuthID(result.Profile, state)
	fileName := core.FileNameFor(v.ID(), strings.TrimPrefix(authID, "qoder-"))
	logger.Info("completed qoder login (vendor=%s email=%s)", v.ID(), cred.Email)
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth: pluginapi.AuthData{
			Provider:    providerKey,
			ID:          strings.TrimSuffix(fileName, ".json"),
			FileName:    fileName,
			Label:       cred.Label,
			StorageJSON: storage,
		},
	}, nil
}

// OwnsLoginSession 报告该会话是否由本供应商创建。
//
// 会话 id 只在 qoder 包的状态表里，因此根层要判断归属只能问它。
func (v *qoderVendor) OwnsLoginSession(sessionID string) bool {
	return qoder.OwnsLoginSession(sessionID)
}

// Refresh 校验并刷新凭证。
//
// Qoder 的续期是 jobToken 交换（PAT → 新的 securityOauthToken/refreshToken）；
// 设备令牌则用 userinfo 保活（内容不变则不写盘）。
func (v *qoderVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	updated, refreshed, errRefresh := qoder.RefreshCredential(ctx, native)
	if errRefresh != nil {
		return nil, false, errRefresh
	}
	if refreshed && updated != nil {
		cred.ApplyTokenRefresh(updated.Token, updated.RefreshToken, 0)
		cred.Native = updated
	}
	return cred, refreshed, nil
}

// MergeStorageJSON 实现 core.StorageMerger：把续期后的令牌合并回原凭证 JSON。
//
// Qoder 的凭证是扁平字段（device_token / refresh_token / region / label），
// 只更新会变的那两个令牌字段，其余字段（用户手写的 label、secret 等）原样保留。
func (v *qoderVendor) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
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

// Tasks 返回本实例支持的任务动作。
//
// Qoder 没有 WorkBuddy 那套成长任务体系，只有签到。
func (v *qoderVendor) Tasks() []core.Task { return nil }

// ---- Bridge 缓存 ----

// bridgeEntry 是缓存的一条 Bridge。
type bridgeEntry struct {
	bridge    *bridge.Bridge
	createdAt time.Time
}

// bridgeFor 取（或建立）该凭证的 cosy 会话。
//
// 缓存是必需的：建会话要一次 jobToken 交换 + RSA 加密，每个请求重建
// 会让上游看到大量重复的令牌交换。
func (v *qoderVendor) bridgeFor(ctx context.Context, cred *qoder.Credential) (*bridge.Bridge, error) {
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
	templateBase, errTemplate := qoder.TemplateBase()
	if errTemplate != nil {
		return nil, errTemplate
	}
	b, errBridge := bridge.NewBridge(createCtx, qoder.BridgeSecret(cred), v.region, templateBase)
	if errBridge != nil {
		return nil, qoder.ClassifyCredentialError(errBridge)
	}
	v.bridges.Store(key, bridgeEntry{bridge: b, createdAt: time.Now()})
	return b, nil
}

// bridgeCacheKey 生成 Bridge 的缓存键。
//
// 用「凭证指纹 + 区域」：凭证轮换后必须重建会话（旧的签名密钥已失效）。
// 不直接存 token 明文——缓存键会进日志与内存 dump。
func (v *qoderVendor) bridgeCacheKey(cred *qoder.Credential) string {
	return v.ID() + "|" + qoder.CredentialFingerprint(cred)
}

// ---- 内部工具 ----

// qoderModelsToPluginAPI 把上游模型条目转成宿主契约类型。
func qoderModelsToPluginAPI(models []bridge.QoderModel) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.Key)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(model.DisplayName)
		if name == "" {
			name = id
		}
		out = append(out, core.ModelInfoToPluginAPI("", core.ModelDescriptor{
			ID:                 id,
			Name:               name,
			Description:        describeQoderModel(model),
			ContextLength:      int64(model.ContextWindow),
			MaxOutputTokens:    int64(model.MaxOutputTokens),
			SupportsTools:      true,
			CanDisableThinking: !model.IsReasoning,
		}))
	}
	return out
}

// describeQoderModel 拼接模型说明（带价格倍率）。
func describeQoderModel(model bridge.QoderModel) string {
	description := "Qoder 上游模型"
	if model.IsDefault {
		description += "（默认）"
	}
	if model.IsReasoning {
		description += "（推理）"
	}
	if model.PriceFactor > 0 {
		description += fmt.Sprintf("，计费倍率 x%.2f", model.PriceFactor)
	}
	return description
}

// probeCredentialForVendor 为某个供应商取一个可用凭证（供模型探测用）。
func probeCredentialForVendor(vendorID string) *qoder.Credential {
	ctx := context.Background()
	entries, errList := listHostAuths(ctx, "")
	if errList != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		stored, okStored := getAuthJSONByIndex(ctx, "", entry.AuthIndex)
		if !okStored {
			continue
		}
		credential, okParse := parseVendorCredential(stored, entry.Name, nil)
		if !okParse || credential.VendorIDValue() != vendorID {
			continue
		}
		if native, okNative := credential.Native.(*qoder.Credential); okNative {
			return native
		}
	}
	return nil
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

// init 注册 Qoder 的两个区域实例。
//
// 与 WorkBuddy 一样在包初始化期注册：宿主的 plugin.register 之前就绪，
// 因此任何时刻调用 core.ResolveVendor 都能拿到完整清单。
func init() {
	for _, vendor := range newQoderVendors() {
		core.RegisterVendor(vendor)
	}
}
