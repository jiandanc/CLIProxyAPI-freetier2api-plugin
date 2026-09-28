package main

// 本文件把 Tabbit 适配成 core.Vendor。

import (
	"context"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/vendors/tabbit"
)

type tabbitVendor struct{}

func newTabbitVendor() core.Vendor { return &tabbitVendor{} }

func (v *tabbitVendor) ID() string          { return tabbit.VendorID }
func (v *tabbitVendor) Name() string        { return tabbit.VendorName }
func (v *tabbitVendor) Region() string      { return "global" }
func (v *tabbitVendor) ModelPrefix() string { return "" }

func (v *tabbitVendor) AuthMode() string { return "apikey" }

func (v *tabbitVendor) Match(fileName, provider string, raw map[string]any) bool {
	return tabbit.LooksLikeCredential(raw, fileName, provider)
}

func (v *tabbitVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := tabbit.ParseCredential(raw)
	if errParse != nil {
		return nil, errParse
	}

	label := native.Label
	if label == "" {
		if native.APIKey != "" {
			label = "Tabbit-" + tabbit.MaskedKey(native.APIKey)
		} else {
			label = "Tabbit-" + tabbit.MaskedKey(native.SessionToken)
		}
	}

	token := native.APIKey
	if token == "" {
		token = native.SessionToken
	}

	return &core.Credential{
		VendorID: tabbit.VendorID,
		Region:   "global",
		Label:    label,
		UID:      tabbit.MaskedKey(token),
		Token:    token,
		AuthMode: "apikey",
		Native:   native,
	}, nil
}

func (v *tabbitVendor) nativeCredential(cred *core.Credential) (*tabbit.Credential, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", 401)
	}
	if native, ok := cred.Native.(*tabbit.Credential); ok && native != nil {
		return native, nil
	}
	return nil, core.NewPluginError("credential_missing", "credential was not parsed by tabbit vendor", 401)
}

func (v *tabbitVendor) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, err := tabbit.FetchModels(ctx, nil)
	if err != nil {
		return nil, err
	}
	return tabbitModelsToPluginAPI(models), nil
}

func (v *tabbitVendor) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, _ := v.nativeCredential(cred)
	models, err := tabbit.FetchModels(ctx, native)
	if err != nil {
		return nil, err
	}
	return tabbitModelsToPluginAPI(models), nil
}

func tabbitModelsToPluginAPI(models []tabbit.Model) []pluginapi.ModelInfo {
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

func (v *tabbitVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	payload, errChat := tabbit.Chat(ctx, baseURLOverride(tabbit.VendorID), tabbit.ChatRequest{
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

func (v *tabbitVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return err
	}
	resp, errStream := tabbit.ChatStream(ctx, baseURLOverride(tabbit.VendorID), tabbit.ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     true,
	})
	if errStream != nil {
		return errorToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return forwardRawSSE(resp.Body, sink)
}

func (v *tabbitVendor) CountTokens(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (int, error) {
	if len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

func (v *tabbitVendor) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	return tabbit.FetchQuota(ctx, native)
}

func (v *tabbitVendor) SupportsCheckin() bool {
	return tabbit.SupportsCheckin()
}

func (v *tabbitVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	return nil, nil
}

func (v *tabbitVendor) SupportsLogin() bool {
	return tabbit.SupportsLogin()
}

func (v *tabbitVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return tabbit.LoginStart(ctx)
}

func (v *tabbitVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return tabbit.LoginPoll(ctx, state)
}

func (v *tabbitVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, false, err
	}
	if errVal := tabbit.ValidateCredential(ctx, native); errVal != nil {
		return nil, false, errVal
	}
	return cred, false, nil
}

func (v *tabbitVendor) Tasks() []core.Task { return nil }

func init() {
	core.RegisterVendor(newTabbitVendor())
}
