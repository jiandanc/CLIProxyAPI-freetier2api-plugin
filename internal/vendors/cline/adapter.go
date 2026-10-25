package cline

// 本文件把 Cline 适配成 core.Vendor。
//
// 与其它供应商的关键差异：
//   - **有完整登录流程**：WorkOS OAuth 2.0 设备码（RFC 8628），三步换到
//     Cline 自己的令牌；会话状态存在共享的 auth.SessionRegistry 里；
//   - **没有区域**：只有一个部署（api.cline.bot），Region() 返回空串；
//   - **没有额度接口**：上游不提供余额查询，Quota 返回「不支持」；
//   - **令牌需要 workos: 前缀**：由协议层统一补，适配层不必记。

import (
	"context"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// adapter 是 core.Vendor 在 Cline 上的实现。
type adapter struct {
	*core.NativeAdapter[*Credential]
	baseURL string
	// creditsMu 保护 credits：最近一次模型清单里的计费档。
	credits core.CreditTable
}

// pendingLogin 是一次进行中的 Cline 登录（会话数据）。
type pendingLogin struct {
	deviceCode string
}

// Instances 构造 Cline 实例（只有一个，没有区域之分）。
func Instances(env core.Env) []core.Vendor {
	inst := &adapter{}
	inst.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorID, VendorName: VendorName},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := ParseCredential(raw)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			label := native.Label
			if label == "" {
				label = native.Email
			}
			if label == "" {
				label = fileName
			}
			return core.CredentialSeed{
				VendorID: VendorID, Label: label, FileID: core.FileIDFromFileName(fileName),
				Token: native.AccessToken, RefreshToken: native.RefreshToken,
				ExpiresAt: native.ExpiresAt, AuthMode: "oauth",
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			updated, refreshed, errRefresh := RefreshCredential(ctx, inst.baseURL, native)
			if errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			return core.NativeRefresh[*Credential]{
				Native: updated, Token: updated.AccessToken, RefreshToken: updated.RefreshToken,
				ExpiresAt: updated.ExpiresAt, Changed: refreshed,
			}, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeStorageJSON(original, native)
		},
	})
	return []core.Vendor{inst}
}

// Configure 解析供应商专属配置。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	v.baseURL = strings.TrimSpace(raw["cline_base_url"])
}

// Match 判断一份凭证是否属于本供应商。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// ---- 模型 ----

// StaticModels 返回 Cline 的模型清单（上游接口无需认证）。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx, v.baseURL)
	if errModels != nil {
		// 探测失败退回内置兜底清单：没有模型注册比注册一份可能过时的
		// 清单更糟（客户端会看不到任何模型）。
		logger.Debug("cline: list models failed, using bundled list: %v", errModels)
		models = bundledModels()
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI 把上游模型条目转成宿主契约类型。
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	core.FillCredits(&v.credits, models,
		func(model Model) string { return model.ID },
		func(model Model) string { return costLabel(model.Cost) })
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return core.ModelDescriptor{}, false
		}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = id
		}
		contextWindow := model.ContextWindow
		if contextWindow == 0 {
			contextWindow = core.DefaultContextWindow(id)
		}
		maxOutput := model.MaxOutputTokens
		if maxOutput == 0 {
			maxOutput = core.DefaultMaxOutputTokens
		}
		return core.ModelDescriptor{
			ID:                 id,
			Name:               name,
			Description:        describeModel(model),
			ContextLength:      contextWindow,
			MaxOutputTokens:    maxOutput,
			SupportsTools:      model.SupportsTools,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// describeModel 拼接模型说明（标出计费档）。
//
// 计费档是用户最关心的信息：免费档可以随便用，订阅档要占套餐额度。
func describeModel(model Model) string {
	description := strings.TrimSpace(model.Description)
	switch model.Cost {
	case CostFree:
		description = "[免费] " + description
	case CostPass:
		description = "[订阅] " + description
	}
	if model.IsReasoning {
		description += "（推理）"
	}
	return description
}

// ModelCredits 实现 core.CreditsProvider：返回「官方模型 ID → 计费档」。
//
// 控制台的「费用倍率」列直接展示这个文案；免费/订阅是 Cline 能给出的
// 最精确的成本信息（上游不提供数值倍率）。
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// costLabel 把计费档转成展示文案；未知档返回空串。
func costLabel(cost string) string {
	switch cost {
	case CostFree:
		return "免费"
	case CostPass:
		return "订阅"
	}
	return ""
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, v.baseURL, ChatRequest{
		BearerToken: native.BearerToken(),
		Body:        req.Payload,
		Stream:      false,
	})
	if errChat != nil {
		return nil, core.ToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	resp, errStream := ChatStream(ctx, v.baseURL, ChatRequest{
		BearerToken: native.BearerToken(),
		Body:        req.Payload,
		Stream:      true,
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// ---- 登录与续期 ----

// LoginStart 发起一次 WorkOS 设备码授权。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	device, errStart := LoginStart(ctx)
	if errStart != nil {
		return nil, core.ToPluginError(errStart)
	}
	sessionID := "cline_" + core.RandomHex(16)
	v.Env.Logins.Register(VendorID, sessionID, time.Until(device.ExpiresAt), device.Interval, &pendingLogin{deviceCode: device.DeviceCode})
	logger.Info("started cline login (user_code=%s)", device.UserCode)
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       device.VerificationURI,
		State:     sessionID,
		ExpiresAt: device.ExpiresAt,
		Metadata: map[string]any{
			core.VendorKey: VendorID,
			// user_code 必须展示给用户：授权页要求手动输入这串短码
			// （verification_uri_complete 有时不自动填）。
			"user_code": device.UserCode,
		},
	}, nil
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	session, pollState := v.Env.Logins.Poll(state)
	if pollState == core.PollMissing {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	if pollState == core.PollThrottled {
		// 节流期间：继续保持等待状态，不打上游也不报错。
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}
	pending, okPending := session.Data.(*pendingLogin)
	if !okPending {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话数据已损坏，请重新发起登录",
		}, nil
	}
	credential, errPoll := LoginPoll(ctx, v.baseURL, pending.deviceCode)
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

	v.Env.Logins.Delete(strings.TrimSpace(state))
	storageJSON, errStorage := credential.StorageJSON()
	if errStorage != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "构建凭证失败：" + errStorage.Error(),
		}, nil
	}
	label := core.FirstNonEmpty(credential.Label, credential.Email, credential.AccountID)
	logger.Info("completed cline login for %s", core.FirstNonEmpty(credential.Email, credential.AccountID))
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(VendorID, core.FirstNonEmpty(credential.Email, credential.AccountID, core.RandomHex(16))),
			Label:       label,
			StorageJSON: storageJSON,
		},
	}, nil
}

// ConfigFields 声明 cline 专属配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{{Name: "cline_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "上游基地址覆盖。"}}
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.AuthModeReporter     = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.CreditsProvider      = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
)
