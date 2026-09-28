package main

// 本文件把 ZCode 适配成 core.Vendor。

import (
	"context"

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

func (v *zcodeVendor) AuthMode() string { return "apikey" }

func (v *zcodeVendor) Match(fileName, provider string, raw map[string]any) bool {
	return zcode.LooksLikeCredential(raw, fileName, provider)
}

func (v *zcodeVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := zcode.ParseCredential(raw)
	if errParse != nil {
		return nil, errParse
	}

	label := native.Label
	if label == "" {
		if native.APIKey != "" {
			label = "ZCode-" + zcode.MaskedKey(native.APIKey)
		} else {
			label = "ZCode-" + zcode.MaskedKey(native.JWTToken)
		}
	}

	token := native.APIKey
	if token == "" {
		token = native.JWTToken
	}

	return &core.Credential{
		VendorID: zcode.VendorID,
		Region:   "global",
		Label:    label,
		UID:      zcode.MaskedKey(token),
		Token:    token,
		AuthMode: "apikey",
		Native:   native,
	}, nil
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
	models, err := zcode.FetchModels(ctx)
	if err != nil {
		return nil, err
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
			CanDisableThinking: !m.IsReasoning,
		}))
	}
	return out
}

func (v *zcodeVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	payload, errChat := zcode.Chat(ctx, baseURLOverride(zcode.VendorID), zcode.ChatRequest{
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

func (v *zcodeVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return err
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
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	resp, errQuota := zcode.FetchQuota(ctx, native)
	if errQuota != nil {
		return nil, errorToPluginError(errQuota)
	}
	return resp, nil
}

func (v *zcodeVendor) SupportsCheckin() bool {
	return zcode.SupportsCheckin()
}

func (v *zcodeVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	res, errCheckin := zcode.Checkin(ctx, native)
	if errCheckin != nil {
		return nil, errorToPluginError(errCheckin)
	}
	return &core.CheckinResult{
		Already: res.Already,
		Credit:  res.Credit,
		Message: res.Message,
	}, nil
}

func (v *zcodeVendor) SupportsLogin() bool {
	return true
}

func (v *zcodeVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return zcode.LoginStart(ctx, baseURLOverride(zcode.VendorID))
}

func (v *zcodeVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return zcode.LoginPoll(ctx, baseURLOverride(zcode.VendorID), state)
}

func (v *zcodeVendor) OwnsLoginSession(sessionID string) bool {
	return zcode.OwnsLoginSession(sessionID)
}

func (v *zcodeVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, false, err
	}
	if errVal := zcode.ValidateCredential(ctx, native); errVal != nil {
		return nil, false, errVal
	}
	return cred, false, nil
}

func (v *zcodeVendor) Tasks() []core.Task { return nil }

func init() {
	core.RegisterVendor(newZCodeVendor())
}
