package main

// 本文件把 TRAE SOLO 适配成 core.Vendor。

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/vendors/traesolo"
)

type traeSoloVendor struct{}

func newTraeSoloVendor() core.Vendor { return &traeSoloVendor{} }

func (v *traeSoloVendor) ID() string          { return traesolo.VendorID }
func (v *traeSoloVendor) Name() string        { return traesolo.VendorName }
func (v *traeSoloVendor) Region() string      { return "cn" }
func (v *traeSoloVendor) ModelPrefix() string { return "" }

func (v *traeSoloVendor) Match(fileName, provider string, raw map[string]any) bool {
	return traesolo.LooksLikeCredential(raw, fileName, provider)
}

func (v *traeSoloVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := traesolo.ParseCredential(raw)
	if errParse != nil {
		return nil, errParse
	}

	label := native.Label
	if label == "" {
		if native.Nickname != "" {
			label = "TRAESOLO-" + native.Nickname
		} else if native.UID != "" {
			label = "TRAESOLO-" + native.UID
		} else {
			label = "TRAESOLO-Account"
		}
	}

	return &core.Credential{
		VendorID:  traesolo.VendorID,
		Region:    "cn",
		Label:     label,
		FileID:    strings.TrimSuffix(strings.TrimSpace(fileName), ".json"),
		Token:     native.AccessToken,
		ExpiresAt: native.ExpiresAt,
		AuthMode:  "oauth",
		Native:    native,
	}, nil
}

func (v *traeSoloVendor) nativeCredential(cred *core.Credential) (*traesolo.Credential, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", 401)
	}
	if native, ok := cred.Native.(*traesolo.Credential); ok && native != nil {
		return native, nil
	}
	return nil, core.NewPluginError("credential_missing", "credential was not parsed by traesolo vendor", 401)
}

func (v *traeSoloVendor) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, err := traesolo.FetchModels(ctx, nil)
	if err != nil {
		return nil, err
	}
	return traeSoloModelsToPluginAPI(models), nil
}

func (v *traeSoloVendor) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, _ := v.nativeCredential(cred)
	models, err := traesolo.FetchModels(ctx, native)
	if err != nil {
		return nil, err
	}
	return traeSoloModelsToPluginAPI(models), nil
}

func traeSoloModelsToPluginAPI(models []traesolo.Model) []pluginapi.ModelInfo {
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

func (v *traeSoloVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	payload, errChat := traesolo.Chat(ctx, baseURLOverride(traesolo.VendorID), traesolo.ChatRequest{
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

func (v *traeSoloVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return err
	}
	return traesolo.ChatStream(ctx, baseURLOverride(traesolo.VendorID), traesolo.ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     true,
	}, func(chunk []byte) error {
		return sink.Emit(chunk)
	})
}

func (v *traeSoloVendor) CountTokens(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (int, error) {
	if len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

func (v *traeSoloVendor) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	resp, errQuota := traesolo.FetchQuota(ctx, native)
	if errQuota != nil {
		return nil, errorToPluginError(errQuota)
	}
	return resp, nil
}

func (v *traeSoloVendor) SupportsCheckin() bool {
	return traesolo.SupportsCheckin()
}

func (v *traeSoloVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, err
	}
	res, errCheckin := traesolo.Checkin(ctx, native)
	if errCheckin != nil {
		return nil, errorToPluginError(errCheckin)
	}
	return &core.CheckinResult{
		Already: res.Already,
		Credit:  res.Credit,
		Message: res.Message,
	}, nil
}

func (v *traeSoloVendor) SupportsLogin() bool {
	return true
}

func (v *traeSoloVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return traesolo.LoginStart(ctx)
}

func (v *traeSoloVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return traesolo.LoginPoll(ctx, state)
}

func (v *traeSoloVendor) OwnsLoginSession(sessionID string) bool {
	return traesolo.OwnsLoginSession(sessionID)
}

func (v *traeSoloVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, err := v.nativeCredential(cred)
	if err != nil {
		return nil, false, err
	}
	updated, refreshed, errRefresh := traesolo.RefreshCredential(ctx, native)
	if errRefresh != nil {
		return nil, false, errRefresh
	}
	if refreshed && updated != nil {
		cred.ApplyTokenRefresh(updated.AccessToken, updated.RefreshToken, updated.ExpiresAt)
		cred.Native = updated
	}
	return cred, refreshed, nil
}

func (v *traeSoloVendor) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, err := v.nativeCredential(credential)
	if err != nil {
		return original, err
	}
	return traesolo.MergeStorageJSON(original, native)
}

func (v *traeSoloVendor) Tasks() []core.Task { return nil }

func init() {
	core.RegisterVendor(newTraeSoloVendor())
}
