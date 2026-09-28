package main

// 本文件把 CodeArts 适配成 core.Vendor。

import (
	"context"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/vendors/codearts"
)

type codeartsVendor struct{}

func newCodeartsVendor() core.Vendor { return &codeartsVendor{} }

func (v *codeartsVendor) ID() string          { return codearts.VendorID }
func (v *codeartsVendor) Name() string        { return codearts.VendorName }
func (v *codeartsVendor) Region() string      { return "cn" }
func (v *codeartsVendor) ModelPrefix() string { return "" }

func (v *codeartsVendor) Match(fileName, provider string, raw map[string]any) bool {
	return codearts.LooksLikeCredential(raw, fileName, provider)
}

func (v *codeartsVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := codearts.ParseCredential(raw)
	if errParse != nil {
		return nil, errParse
	}

	label := native.Label
	if label == "" {
		if native.UserName != "" {
			label = "CodeArts-" + native.UserName
		} else if native.UserID != "" {
			label = "CodeArts-" + native.UserID
		} else {
			label = "CodeArts-Account"
		}
	}

	return &core.Credential{
		VendorID:  codearts.VendorID,
		Region:    "cn",
		Label:     label,
		UID:       native.UserID,
		Token:     native.AccessKeyID,
		ExpiresAt: native.ExpiresAt,
		AuthMode:  "oauth",
		Native:    native,
	}, nil
}

func (v *codeartsVendor) nativeCredential(cred *core.Credential) (*codearts.Credential, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", 401)
	}
	if native, ok := cred.Native.(*codearts.Credential); ok && native != nil {
		return native, nil
	}
	return nil, core.NewPluginError("credential_missing", "credential was not parsed by codearts vendor", 401)
}

func (v *codeartsVendor) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, err := codearts.FetchModels(ctx, nil)
	if err != nil {
		return nil, err
	}
	return codeartsModelsToPluginAPI(models), nil
}

func (v *codeartsVendor) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, _ := v.nativeCredential(cred)
	models, err := codearts.FetchModels(ctx, native)
	if err != nil {
		return nil, err
	}
	return codeartsModelsToPluginAPI(models), nil
}

func codeartsModelsToPluginAPI(models []codearts.Model) []pluginapi.ModelInfo {
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

func (v *codeartsVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	payload, errChat := codearts.Chat(ctx, baseURLOverride(codearts.VendorID), codearts.ChatRequest{
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

func (v *codeartsVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return err
	}
	resp, errStream := codearts.ChatStream(ctx, baseURLOverride(codearts.VendorID), codearts.ChatRequest{
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

func (v *codeartsVendor) CountTokens(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (int, error) {
	if len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

func (v *codeartsVendor) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	return codearts.FetchQuota(ctx, native)
}

func (v *codeartsVendor) SupportsCheckin() bool {
	return codearts.SupportsCheckin()
}

func (v *codeartsVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	res, errClaim := codearts.Checkin(ctx, native)
	if errClaim != nil {
		return nil, errorToPluginError(errClaim)
	}
	return &core.CheckinResult{
		Already: res.Already,
		Message: res.Message,
	}, nil
}

func (v *codeartsVendor) SupportsLogin() bool {
	return true
}

func (v *codeartsVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return codearts.LoginStart(ctx)
}

func (v *codeartsVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return codearts.LoginPoll(ctx, state)
}

func (v *codeartsVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, false, err
	}
	updated, refreshed, errRefresh := codearts.RefreshCredential(ctx, native)
	if errRefresh != nil {
		return nil, false, errRefresh
	}
	if refreshed && updated != nil {
		cred.ApplyTokenRefresh(updated.AccessKeyID, updated.RefreshToken, updated.ExpiresAt)
		cred.Native = updated
	}
	return cred, refreshed, nil
}

func (v *codeartsVendor) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, err := v.nativeCredential(credential)
	if err != nil {
		return original, err
	}
	return codearts.MergeStorageJSON(original, native)
}

func (v *codeartsVendor) Tasks() []core.Task { return nil }

func init() {
	core.RegisterVendor(newCodeartsVendor())
}
