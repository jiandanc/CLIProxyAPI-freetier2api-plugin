package main

// 本文件把 ZCode 适配成 core.Vendor。
//
// 凭证形态是 oauth：账号由控制台发起 OAuth 登录产生（一次拿到 Coding Plan
// JWT 与兑换出的 API Key）。之所以声明为 oauth 而不是 apikey，是因为页面按
// auth_mode 决定「添加账号」的动作——apikey 会直接弹输入框、不发起登录。
//
// 通道选择：对话只走 API Key 通道（api.z.ai，免人机验证码）。Plan 通道
// （Bearer JWT）被上游强制要求 X-Aliyun-Captcha-Verify-Param，而本插件不内置
// 浏览器验证码求解。JWT 仍保留，供额度查询与套餐领取使用。

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/vendors/zcode"
)

type zcodeVendor struct{}

func newZCodeVendor() core.Vendor { return &zcodeVendor{} }

func (v *zcodeVendor) ID() string          { return zcode.VendorID }
func (v *zcodeVendor) Name() string        { return zcode.VendorName }
func (v *zcodeVendor) Region() string      { return "global" }
func (v *zcodeVendor) ModelPrefix() string { return "" }

// AuthMode 返回凭证形态：oauth。页面据此在「添加账号」时发起登录流程。
func (v *zcodeVendor) AuthMode() string { return "oauth" }

func (v *zcodeVendor) Match(fileName, provider string, raw map[string]any) bool {
	return zcode.LooksLikeCredential(raw, fileName, provider)
}

func (v *zcodeVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := zcode.ParseCredential(raw)
	if errParse != nil {
		return nil, errParse
	}

	// 展示名优先邮箱（可读），其次是脱敏凭证。
	label := native.Label
	if label == "" {
		label = firstNonEmptyString(native.Email, "ZCode-"+zcode.MaskedKey(native.OutboundToken()))
	}

	// FileID 是账号身份（文件名）：签到记录与额度缓存都以它为键。
	// UID（邮箱/user_id）只用于展示与激活上报，不承担身份职责。
	fileID := strings.TrimSuffix(strings.TrimSpace(fileName), ".json")

	// core.Token 是「出站凭证」：对话用 API Key 通道，因此这里放 API Key。
	// JWT 留在 Native 里，供额度查询与套餐领取使用。
	return &core.Credential{
		VendorID: zcode.VendorID,
		Region:   "global",
		Label:    label,
		FileID:   fileID,
		UID:      firstNonEmptyString(native.UserID, native.Email),
		Token:    native.APIKey,
		AuthMode: native.AuthMode,
		Native:   native,
	}, nil
}

// MergeStorageJSON 把刷新后的凭证合并回原文件，保留用户手工维护的其它键。
func (v *zcodeVendor) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, errNative := v.nativeCredential(credential)
	if errNative != nil {
		return original, errNative
	}
	return zcode.MergeStorageJSON(original, native)
}

func (v *zcodeVendor) nativeCredential(cred *core.Credential) (*zcode.Credential, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", 401)
	}
	if native, ok := cred.Native.(*zcode.Credential); ok && native != nil {
		return native, nil
	}
	return nil, core.NewPluginError("credential_missing", "credential was not parsed by zcode vendor", 401)
}

func (v *zcodeVendor) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := zcode.FetchModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return zcodeModelsToPluginAPI(models), nil
}

func (v *zcodeVendor) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	return v.StaticModels(ctx)
}

func zcodeModelsToPluginAPI(models []zcode.Model) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, core.ModelInfoToPluginAPI("", core.ModelDescriptor{
			ID:                 m.ID,
			Name:               m.Name,
			Description:        m.Description,
			ContextLength:      int64(m.ContextWindow),
			MaxOutputTokens:    int64(m.MaxTokens),
			SupportsTools:      m.SupportsTools,
			SupportsImages:     m.SupportsImages,
			CanDisableThinking: !m.IsReasoning,
		}))
	}
	return out
}

func (v *zcodeVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := zcode.Chat(ctx, baseURLOverride(zcode.VendorID), zcode.ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     false,
	})
	if errChat != nil {
		return nil, errChat
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {jsonContentType}},
	}, nil
}

func (v *zcodeVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return errNative
	}
	return zcode.ChatStream(ctx, baseURLOverride(zcode.VendorID), zcode.ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     true,
	}, func(chunk []byte) error {
		return sink.Emit(chunk)
	})
}

func (v *zcodeVendor) CountTokens(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (int, error) {
	if len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

func (v *zcodeVendor) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	response, errQuota := zcode.FetchQuota(ctx, native)
	if errQuota != nil {
		return nil, errQuota
	}
	return response, nil
}

// SupportsCheckin 报告本供应商是否提供签到。
//
// ZCode 没有：其「签到」实质是领取活动套餐，而上游对 billing/claim 强制要求
// 阿里云无痕验证码（实测 HTTP 400 / code=3007），本插件不内置浏览器求解器。
// 控制台据此按「无签到活动」展示，与 Cline / OpenCode ZEN 一致。
func (v *zcodeVendor) SupportsCheckin() bool { return false }

// Checkin 报告本供应商不支持签到。
func (v *zcodeVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	return nil, nil
}

func (v *zcodeVendor) SupportsLogin() bool {
	return true
}

func (v *zcodeVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return zcode.LoginStart(ctx)
}

func (v *zcodeVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return zcode.LoginPoll(ctx, state)
}

func (v *zcodeVendor) OwnsLoginSession(sessionID string) bool {
	return zcode.OwnsLoginSession(sessionID)
}

// Refresh 校验凭证。
//
// ZCode 没有 refresh_token 流程（JWT 与 API Key 都不支持续期），因此这里只做
// 本地校验并返回 refreshed=false——不触发无意义的写盘。
func (v *zcodeVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	if errValidate := zcode.ValidateCredential(ctx, native); errValidate != nil {
		return nil, false, errValidate
	}
	return cred, false, nil
}

func (v *zcodeVendor) Tasks() []core.Task { return nil }

func init() {
	core.RegisterVendor(newZCodeVendor())
}
