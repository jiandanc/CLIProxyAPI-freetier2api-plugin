package main

// 本文件把 MiniMax Code（国内版/国际版）适配成 core.Vendor。

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/vendors/minimaxcode"
)

type minimaxCodeVendor struct {
	region minimaxcode.Region
}

func newMiniMaxCodeVendor(r minimaxcode.Region) core.Vendor {
	return &minimaxCodeVendor{region: r}
}

func (v *minimaxCodeVendor) ID() string {
	return minimaxcode.VendorIDFor(v.region)
}

func (v *minimaxCodeVendor) Name() string {
	return minimaxcode.VendorNameFor(v.region)
}

func (v *minimaxCodeVendor) Region() string {
	return string(v.region)
}

func (v *minimaxCodeVendor) ModelPrefix() string {
	return ""
}

func (v *minimaxCodeVendor) Match(fileName, provider string, raw map[string]any) bool {
	return minimaxcode.LooksLikeCredential(raw, fileName, provider, v.region)
}

func (v *minimaxCodeVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := minimaxcode.ParseCredential(raw, v.region)
	if errParse != nil {
		return nil, errParse
	}

	label := native.Label
	if label == "" {
		if native.Email != "" {
			label = native.Email
		} else if native.UserID != "" {
			label = v.Name() + "-" + native.UserID
		} else {
			label = fileName
		}
	}

	uid := native.UserID
	if uid == "" {
		uid = firstNonEmptyString(native.Email, fileName)
	}

	return &core.Credential{
		VendorID:     minimaxcode.VendorIDFor(v.region),
		Region:       string(v.region),
		Label:        label,
		FileID:       strings.TrimSuffix(strings.TrimSpace(fileName), ".json"),
		Token:        native.AccessToken,
		RefreshToken: native.RefreshToken,
		ExpiresAt:    native.ExpiresAt,
		AuthMode:     native.AuthMode,
		Native:       native,
	}, nil
}

func (v *minimaxCodeVendor) nativeCredential(cred *core.Credential) (*minimaxcode.Credential, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", 401)
	}
	if native, ok := cred.Native.(*minimaxcode.Credential); ok && native != nil {
		return native, nil
	}
	return nil, core.NewPluginError("credential_missing", "credential was not parsed by minimaxcode vendor", 401)
}

func (v *minimaxCodeVendor) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, err := minimaxcode.FetchModels(ctx, v.region)
	if err != nil {
		return nil, err
	}
	return minimaxModelsToPluginAPI(models), nil
}

func (v *minimaxCodeVendor) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	return v.StaticModels(ctx)
}

func minimaxModelsToPluginAPI(models []minimaxcode.Model) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, core.ModelInfoToPluginAPI("", core.ModelDescriptor{
			ID:                 m.ID,
			Name:               m.Name,
			Description:        m.Description,
			ContextLength:      m.ContextWindow,
			MaxOutputTokens:    m.MaxOutput,
			SupportsTools:      m.SupportsTools,
			CanDisableThinking: !m.IsReasoning,
		}))
	}
	return out
}

func (v *minimaxCodeVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := minimaxcode.Chat(ctx, native, req, baseURLOverride(v.ID()))
	if errChat != nil {
		return nil, errorToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {jsonContentType}},
	}, nil
}

func (v *minimaxCodeVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return errNative
	}
	errStream := minimaxcode.ChatStream(ctx, native, req, baseURLOverride(v.ID()), func(chunk []byte) error {
		return sink.Emit(chunk)
	})
	if errStream != nil {
		return errorToPluginError(errStream)
	}
	return nil
}

func (v *minimaxCodeVendor) CountTokens(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (int, error) {
	if len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

func (v *minimaxCodeVendor) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	return minimaxcode.FetchQuota(ctx, native, baseURLOverride(v.ID()))
}

func (v *minimaxCodeVendor) SupportsCheckin() bool {
	// 仅国际版支持签到，国内版上游无签到接口
	return v.region == minimaxcode.RegionGlobal
}

func (v *minimaxCodeVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	return minimaxcode.Checkin(ctx, native, baseURLOverride(v.ID()))
}

func (v *minimaxCodeVendor) SupportsLogin() bool {
	return true
}

func (v *minimaxCodeVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return minimaxcode.LoginStart(ctx, v.region)
}

func (v *minimaxCodeVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return minimaxcode.LoginPoll(ctx, state, baseURLOverride(v.ID()))
}

func (v *minimaxCodeVendor) OwnsLoginSession(sessionID string) bool {
	return minimaxcode.OwnsLoginSession(sessionID, v.region)
}

func (v *minimaxCodeVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	updated, refreshed, errRefresh := minimaxcode.RefreshCredential(ctx, native, baseURLOverride(v.ID()))
	if errRefresh != nil {
		return nil, false, errRefresh
	}
	if refreshed && updated != nil {
		cred.ApplyTokenRefresh(updated.AccessToken, updated.RefreshToken, updated.ExpiresAt)
		cred.Native = updated
	}
	return cred, refreshed, nil
}

func (v *minimaxCodeVendor) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, errNative := v.nativeCredential(credential)
	if errNative != nil {
		return original, errNative
	}
	return minimaxcode.MergeStorageJSON(original, native)
}

func (v *minimaxCodeVendor) Tasks() []core.Task {
	return nil
}

func init() {
	core.RegisterVendor(newMiniMaxCodeVendor(minimaxcode.RegionCN))
	core.RegisterVendor(newMiniMaxCodeVendor(minimaxcode.RegionGlobal))
}
