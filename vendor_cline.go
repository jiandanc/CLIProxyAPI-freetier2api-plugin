package main

// 本文件把 Cline 适配成 core.Vendor。
//
// 与其它供应商的关键差异：
//   - **有完整登录流程**：WorkOS OAuth 2.0 设备码（RFC 8628），三步换到
//     Cline 自己的令牌。会话状态存在本文件的 loginSessions 里；
//   - **没有区域**：只有一个部署（api.cline.bot），Region() 返回空串；
//   - **没有额度接口**：上游不提供余额查询，Quota 返回「不支持」；
//   - **令牌需要 workos: 前缀**：由协议层统一补，适配层不必记。

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/cline"
)

// clineSessionTTL 是登录会话的有效期。
const clineSessionTTL = 15 * time.Minute

// clinePendingLogin 是一次进行中的 Cline 登录。
type clinePendingLogin struct {
	deviceCode string
	// interval 是上游建议的轮询间隔（含下限修正）。
	interval  time.Duration
	expiresAt time.Time
	// lastPoll 记录上次向上游轮询的时刻（节流）。
	lastPoll time.Time
}

// clineVendor 是 core.Vendor 在 Cline 上的实现。
type clineVendor struct {
	// sessions 存进行中的登录会话（设备码 → 会话状态）。
	sessions sync.Map
}

// newClineVendor 构造 Cline 实例（只有一个，没有区域之分）。
func newClineVendor() core.Vendor { return &clineVendor{} }

func (v *clineVendor) ID() string     { return cline.VendorID }
func (v *clineVendor) Name() string   { return cline.VendorName }
func (v *clineVendor) Region() string { return "" }

// ModelPrefix 返回空：本供应商用裸模型名注册。
func (v *clineVendor) ModelPrefix() string { return "" }

// AuthMode 报告本供应商的凭证形态。
func (v *clineVendor) AuthMode() string { return "oauth" }

// ---- 凭证 ----

// Match 判断一份凭证是否属于本供应商。
func (v *clineVendor) Match(fileName, provider string, raw map[string]any) bool {
	return cline.LooksLikeCredential(raw, fileName, provider)
}

// Parse 把凭证 JSON 解析成 core.Credential。
func (v *clineVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := cline.ParseCredential(raw)
	if errParse != nil {
		return nil, errParse
	}
	return newClineCoreCredential(native, fileName), nil
}

// newClineCoreCredential 把协议层凭证包装成 core.Credential。
func newClineCoreCredential(native *cline.Credential, fileName string) *core.Credential {
	label := native.Label
	if label == "" {
		label = native.Email
	}
	if label == "" {
		label = fileName
	}
	return &core.Credential{
		VendorID:     cline.VendorID,
		Label:        label,
		UID:          firstNonEmptyString(native.AccountID, native.Email),
		Token:        native.AccessToken,
		RefreshToken: native.RefreshToken,
		ExpiresAt:    native.ExpiresAt,
		AuthMode:     "oauth",
		Native:       native,
	}
}

// nativeCredential 取回协议层凭证对象。
func (v *clineVendor) nativeCredential(cred *core.Credential) (*cline.Credential, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", 401)
	}
	if native, okNative := cred.Native.(*cline.Credential); okNative && native != nil {
		return native, nil
	}
	return nil, core.NewPluginError("credential_missing",
		"credential was not parsed by the cline vendor", 401)
}

// ---- 模型 ----

// StaticModels 返回 Cline 的模型清单（上游接口无需认证）。
func (v *clineVendor) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := cline.FetchModels(ctx, baseURLOverride(cline.VendorID))
	if errModels != nil {
		// 探测失败退回内置兜底清单：没有模型注册比注册一份可能过时的
		// 清单更糟（客户端会看不到任何模型）。
		logger.Debug("cline: list models failed, using bundled list: %v", errModels)
		models = cline.BundledClineModels()
	}
	return clineModelsToPluginAPI(models), nil
}

// ModelsForAuth 返回该凭证可用的模型。
//
// Cline 的清单是全局的（不按账号区分），因此与 StaticModels 同源。
func (v *clineVendor) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	if cred == nil {
		return nil, nil
	}
	return v.StaticModels(ctx)
}

// clineModelsToPluginAPI 把上游模型条目转成宿主契约类型。
func clineModelsToPluginAPI(models []cline.Model) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = id
		}
		contextWindow := model.ContextWindow
		if contextWindow == 0 {
			contextWindow = defaultContextWindowFor(id)
		}
		maxOutput := model.MaxOutputTokens
		if maxOutput == 0 {
			maxOutput = defaultMaxOutputFor(id)
		}
		out = append(out, core.ModelInfoToPluginAPI("", core.ModelDescriptor{
			ID:                 id,
			Name:               name,
			Description:        describeClineModel(model),
			ContextLength:      contextWindow,
			MaxOutputTokens:    maxOutput,
			SupportsTools:      model.SupportsTools,
			CanDisableThinking: !model.IsReasoning,
		}))
	}
	return out
}

// describeClineModel 拼接模型说明（标出计费档）。
//
// 计费档是用户最关心的信息：免费档可以随便用，订阅档要占套餐额度。
func describeClineModel(model cline.Model) string {
	description := strings.TrimSpace(model.Description)
	switch model.Cost {
	case cline.CostFree:
		description = "[免费] " + description
	case cline.CostPass:
		description = "[订阅] " + description
	}
	if model.IsReasoning {
		description += "（推理）"
	}
	return description
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *clineVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := cline.Chat(ctx, baseURLOverride(cline.VendorID), cline.ChatRequest{
		BearerToken: native.BearerToken(),
		Body:        req.Payload,
		Stream:      false,
	})
	if errChat != nil {
		return nil, errorToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {jsonContentType}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
//
// 只投递上游的原生 SSE 数据载荷，不做 `data: ` 包装与分帧——包装由宿主
// 按声明格式完成。插件侧重复分帧会与宿主产生不一致的行为。
func (v *clineVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return errNative
	}
	resp, errStream := cline.ChatStream(ctx, baseURLOverride(cline.VendorID), cline.ChatRequest{
		BearerToken: native.BearerToken(),
		Body:        req.Payload,
		Stream:      true,
	})
	if errStream != nil {
		return errorToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return forwardRawSSE(resp.Body, sink)
}

// CountTokens 估算 token 数（按字节数除以 3，不调上游）。
func (v *clineVendor) CountTokens(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (int, error) {
	if len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

// ---- 额度与签到 ----

// Quota 报告本供应商没有额度查询接口。
func (v *clineVendor) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	return nil, core.NewPluginError("quota_unsupported",
		"Cline 上游不提供额度查询接口（额度以订阅页为准）", http.StatusNotImplemented)
}

// SupportsCheckin 报告本供应商是否提供签到（Cline 没有）。
func (v *clineVendor) SupportsCheckin() bool { return false }

// Checkin 报告本供应商不支持签到。
func (v *clineVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	return nil, nil
}

// ---- 登录与续期 ----

// LoginStart 发起一次 WorkOS 设备码授权。
func (v *clineVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	device, errStart := cline.LoginStart(ctx)
	if errStart != nil {
		return nil, errorToPluginError(errStart)
	}
	sessionID := "cline_" + newLoginSessionID()
	v.sessions.Store(sessionID, &clinePendingLogin{
		deviceCode: device.DeviceCode,
		interval:   device.Interval,
		expiresAt:  device.ExpiresAt,
	})
	logger.Info("started cline login (user_code=%s)", device.UserCode)
	return &pluginapi.AuthLoginStartResponse{
		Provider:  providerKey,
		URL:       device.VerificationURI,
		State:     sessionID,
		ExpiresAt: device.ExpiresAt,
		Metadata: map[string]any{
			core.VendorKey: v.ID(),
			// user_code 必须展示给用户：授权页要求手动输入这串短码
			// （verification_uri_complete 有时不自动填）。
			"user_code": device.UserCode,
		},
	}, nil
}

// LoginPoll 轮询一次登录状态。
func (v *clineVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	session, exists, shouldPoll := v.takeSession(state)
	if !exists {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	if !shouldPoll {
		// 节流期间：继续保持等待状态，不打上游也不报错
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}
	credential, errPoll := cline.LoginPoll(ctx, baseURLOverride(cline.VendorID), session.deviceCode)
	if errPoll != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errPoll.Error(),
		}, nil
	}
	if credential == nil {
		// 用户还没在浏览器里完成授权，继续等待。
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}

	v.sessions.Delete(strings.TrimSpace(state))
	storageJSON, errStorage := credential.StorageJSON()
	if errStorage != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "构建凭证失败：" + errStorage.Error(),
		}, nil
	}
	coreCredential := newClineCoreCredential(credential, "")
	logger.Info("completed cline login for %s", firstNonEmptyString(credential.Email, credential.AccountID))
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth: pluginapi.AuthData{
			Provider:    providerKey,
			FileName:    core.FileNameFor(v.ID(), firstNonEmptyString(credential.Email, credential.AccountID, newLoginSessionID())),
			Label:       coreCredential.LabelValue(),
			StorageJSON: storageJSON,
		},
	}, nil
}

// OwnsLoginSession 报告该会话是否由本供应商创建。
func (v *clineVendor) OwnsLoginSession(sessionID string) bool {
	trimmed := strings.TrimSpace(sessionID)
	if strings.HasPrefix(strings.ToLower(trimmed), "cline_") {
		return true
	}
	_, okSession := v.sessions.Load(trimmed)
	return okSession
}

// takeSession 取出登录会话（含过期 GC 与轮询节流）。
//
// 返回 (session, exists, shouldPoll):
//   - exists: 会话是否存在且在 TTL 内；
//   - shouldPoll: 是否已过节流间隔，可以向上游发起 poll 请求。
func (v *clineVendor) takeSession(sessionID string) (*clinePendingLogin, bool, bool) {
	trimmed := strings.TrimSpace(sessionID)
	value, okLoad := v.sessions.Load(trimmed)
	if !okLoad {
		return nil, false, false
	}
	session, okSession := value.(*clinePendingLogin)
	if !okSession {
		return nil, false, false
	}
	if time.Now().After(session.expiresAt) {
		v.sessions.Delete(trimmed)
		return nil, false, false
	}
	// 节流：控制台页轮询很频繁，但不该每次都打上游——上游对过快轮询
	// 会回 slow_down，反而拖慢登录。
	if time.Since(session.lastPoll) < session.interval {
		return session, true, false
	}
	session.lastPoll = time.Now()
	return session, true, true
}

// Refresh 校验并刷新凭证。
//
// Cline 的续期是用 refresh token 换一组新令牌；上游没轮换时 refreshed=false，
// 调用方据此决定是否写盘（避免 mtime 无意义地变动）。
func (v *clineVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	updated, refreshed, errRefresh := cline.RefreshCredential(ctx, baseURLOverride(cline.VendorID), native)
	if errRefresh != nil {
		return nil, false, errRefresh
	}
	if refreshed && updated != nil {
		cred.ApplyTokenRefresh(updated.AccessToken, updated.RefreshToken, updated.ExpiresAt)
		cred.Native = updated
	}
	return cred, refreshed, nil
}

// MergeStorageJSON 实现 core.StorageMerger：把续期后的令牌合并回原凭证 JSON。
func (v *clineVendor) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, errNative := v.nativeCredential(credential)
	if errNative != nil {
		return original, errNative
	}
	return cline.MergeStorageJSON(original, native)
}

// Tasks 返回本实例支持的任务动作（Cline 没有任务体系）。
func (v *clineVendor) Tasks() []core.Task { return nil }

// init 注册 Cline 实例。
func init() {
	core.RegisterVendor(newClineVendor())
}
