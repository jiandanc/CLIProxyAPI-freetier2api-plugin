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
	"net/http"
	"strings"
	"sync"
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
}

// Instances 构造 Qoder 的两个区域实例。
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{newInstance(env, RegionCN), newInstance(env, RegionGlobal)}
}

func newInstance(env core.Env, region Region) core.Vendor {
	return &adapter{
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
// 建不起来时退回内置兜底清单——没有模型注册比注册一份静态清单更糟。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := v.fetchModels(ctx)
	if errModels != nil {
		logger.Debug("qoder: list models failed for %s, using bundled list: %v", v.ID(), errModels)
		models = bundledModels()
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
		// 建桥失败同样退回兜底清单：账号本身可能是好的，只是暂时连不上。
		logger.Debug("qoder: bridge for model list failed: %v", errBridge)
		return v.modelsToPluginAPI(bundledModels()), nil
	}
	models, errModels := b.ListAvailableModels(ctx)
	if errModels != nil {
		return v.modelsToPluginAPI(bundledModels()), nil
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
	result, errCheckin := Checkin(ctx, native)
	if errCheckin != nil {
		return nil, errCheckin
	}
	return &core.CheckinResult{
		Already: result.Status == CheckinAlreadyClaimed,
		Credit:  int64(result.Amount),
		Message: result.Message,
	}, nil
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
	b, errBridge := NewBridge(createCtx, BridgeSecret(cred), v.region, templateBase)
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
// 模型 ID 用规范化后的 slug（如 qwen3.8-flash / deepseek-v4-pro），
// 并注册到双向映射表中，以便出站时精确还原为上游加密网关所需的官方 Key；
// DisplayName 原样保留作为展示名。
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		rawKey := strings.TrimSpace(model.Key)
		if rawKey == "" {
			return core.ModelDescriptor{}, false
		}
		id := CanonicalSlug(model.DisplayName, rawKey)
		name := strings.TrimSpace(model.DisplayName)
		if name == "" {
			name = id
		}
		RegisterModelSlug(id, rawKey)
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
	if model.PriceFactor > 0 {
		description += fmt.Sprintf("，计费倍率 x%.2f", model.PriceFactor)
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
