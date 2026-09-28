package main

// 本文件把 Trae（国内版/国际版）适配成 core.Vendor。

import (
	"context"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/vendors/trae"
)

type traeVendor struct {
	region trae.Region
}

func newTraeVendor(r trae.Region) core.Vendor {
	return &traeVendor{region: r}
}

func (v *traeVendor) ID() string {
	return trae.VendorIDFor(v.region)
}

func (v *traeVendor) Name() string {
	if v.region == trae.RegionGlobal {
		return trae.VendorNameGlobal
	}
	return trae.VendorNameCN
}

func (v *traeVendor) Region() string {
	return string(v.region)
}

func (v *traeVendor) ModelPrefix() string {
	return ""
}

func (v *traeVendor) Match(fileName, provider string, raw map[string]any) bool {
	return trae.LooksLikeCredential(raw, fileName, provider, v.region)
}

func (v *traeVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := trae.ParseCredential(raw, v.region)
	if errParse != nil {
		return nil, errParse
	}

	label := native.Label
	if label == "" {
		prefix := "TraeCN-"
		if v.region == trae.RegionGlobal {
			prefix = "TraeGlobal-"
		}
		if native.Nickname != "" {
			label = prefix + native.Nickname
		} else if native.UID != "" {
			label = prefix + native.UID
		} else {
			label = prefix + "Account"
		}
	}

	return &core.Credential{
		VendorID:  trae.VendorIDFor(v.region),
		Region:    string(v.region),
		Label:     label,
		UID:       native.UID,
		Token:     native.AccessToken,
		ExpiresAt: native.ExpiresAt,
		AuthMode:  "oauth",
		Native:    native,
	}, nil
}

func (v *traeVendor) nativeCredential(cred *core.Credential) (*trae.Credential, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", 401)
	}
	if native, ok := cred.Native.(*trae.Credential); ok && native != nil {
		return native, nil
	}
	return nil, core.NewPluginError("credential_missing", "credential was not parsed by trae vendor", 401)
}

func (v *traeVendor) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, err := trae.FetchModels(ctx, v.region)
	if err != nil {
		return nil, err
	}
	return traeModelsToPluginAPI(models), nil
}

func (v *traeVendor) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	return v.StaticModels(ctx)
}

func traeModelsToPluginAPI(models []trae.Model) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, core.ModelInfoToPluginAPI("", core.ModelDescriptor{
			ID:                 m.ID,
			Name:               m.Name,
			Description:        m.Description,
			ContextLength:      int64(m.ContextWindow),
			MaxOutputTokens:    int64(m.MaxTokens),
			SupportsTools:      m.SupportsTools,
			CanDisableThinking: !m.IsReasoning,
		}))
	}
	return out
}

func (v *traeVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	payload, errChat := trae.Chat(ctx, baseURLOverride(v.ID()), trae.ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     false,
	})
	if errChat != nil {
		return nil, errorToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {jsonContentType}},
	}, nil
}

func (v *traeVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return err
	}
	return trae.ChatStream(ctx, baseURLOverride(v.ID()), trae.ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     true,
	}, func(chunk []byte) error {
		return sink.Emit(chunk)
	})
}

func (v *traeVendor) CountTokens(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (int, error) {
	if len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

func (v *traeVendor) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	resp, errQuota := trae.FetchQuota(ctx, native)
	if errQuota != nil {
		return nil, errorToPluginError(errQuota)
	}
	return resp, nil
}

func (v *traeVendor) SupportsCheckin() bool {
	return trae.SupportsCheckin(v.region)
}

func (v *traeVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	res, errCheckin := trae.Checkin(ctx, native)
	if errCheckin != nil {
		return nil, errorToPluginError(errCheckin)
	}
	return &core.CheckinResult{
		Already: res.Already,
		Credit:  res.Credit,
		Message: res.Message,
	}, nil
}

func (v *traeVendor) SupportsLogin() bool {
	return true
}

func (v *traeVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return trae.LoginStart(ctx, v.region)
}

func (v *traeVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return trae.LoginPoll(ctx, state)
}

func (v *traeVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, false, err
	}
	updated, refreshed, errRefresh := trae.RefreshCredential(ctx, native)
	if errRefresh != nil {
		return nil, false, errRefresh
	}
	if refreshed && updated != nil {
		cred.ApplyTokenRefresh(updated.AccessToken, updated.RefreshToken, updated.ExpiresAt)
		cred.Native = updated
	}
	return cred, refreshed, nil
}

func (v *traeVendor) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, err := v.nativeCredential(credential)
	if err != nil {
		return original, err
	}
	return trae.MergeStorageJSON(original, native)
}

func (v *traeVendor) Tasks() []core.Task { return nil }

func init() {
	core.RegisterVendor(newTraeVendor(trae.RegionCN))
	core.RegisterVendor(newTraeVendor(trae.RegionGlobal))
}
