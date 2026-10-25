package trae

// 本文件把 Trae 国内版、Trae 国际版与 TRAE SOLO 适配成 core.Vendor。
//
// 三个实例共享同一套协议实现，差异由 Config/profile 描述。

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

type adapter struct {
	*core.NativeAdapter[*Credential]
	// config 是构造期的 profile，只读。
	config Config
	// ideVersion 是 IDE 版本号的配置覆盖；空串表示沿用 config 的编译期默认值。
	// 三个实例的版本线互不相同（CN 3.3.x / global 3.3.x / SOLO 0.1.x），
	// 所以配置键按实例 ID 区分（见 Configure），不能共用族级键。
	ideVersionMu   sync.RWMutex
	ideVersion     string
	ideVersionCode string
	sessions       *SessionStore
	// catalog 是模型目录缓存（按账号），随实例隔离。
	catalog catalogCache
	// credits 缓存最近一轮目录探测的倍率表（官方模型 ID → 展示文案）。
	credits core.CreditTable
}

// profile 返回当前 profile 快照（编译期 profile + 运行期版本覆盖）。
//
// 版本号决定上游下发的模型目录，可在运行期改写，所以所有读取点都必须走这里。
func (v *adapter) profile() Config {
	v.ideVersionMu.RLock()
	defer v.ideVersionMu.RUnlock()
	cfg := v.config
	if v.ideVersion != "" {
		cfg.IDEVersion = v.ideVersion
	}
	if v.ideVersionCode != "" {
		cfg.IDEVersionCode = v.ideVersionCode
	}
	return cfg
}

// Instances 构造 Trae 家族的全部实例。
func Instances(env core.Env) []core.Vendor {
	configs := AllConfigs()
	sessions := NewSessionStore()
	out := make([]core.Vendor, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, newInstance(env, cfg, sessions))
	}
	return out
}

func newInstance(env core.Env, cfg Config, sessions *SessionStore) core.Vendor {
	return &adapter{
		NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
			Meta: core.Meta{VendorID: cfg.VendorID, VendorName: cfg.VendorName, VendorRegion: string(cfg.Region)},
			Env:  env,
			Extract: func(c *core.Credential) (*Credential, bool) {
				native, ok := c.Native.(*Credential)
				return native, ok
			},
			ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
				native, errParse := ParseCredentialFor(raw, cfg)
				if errParse != nil {
					return core.CredentialSeed{}, nil, errParse
				}
				native.Vendor = cfg.VendorID
				label := native.Label
				if label == "" {
					prefix := "TraeCN-"
					switch {
					case cfg.Solo:
						prefix = "TRAESOLO-"
					case cfg.Region == RegionGlobal:
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
				return core.CredentialSeed{
					VendorID: cfg.VendorID, Region: string(cfg.Region), Label: label,
					FileID: core.FileIDFromFileName(fileName), Token: native.AccessToken,
					ExpiresAt: native.ExpiresAt, AuthMode: "oauth",
				}, native, nil
			},
			RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
				updated, refreshed, errRefresh := RefreshCredential(ctx, native, cfg)
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
		}),
		config: cfg, sessions: sessions,
	}
}

func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredentialFor(raw, fileName, provider, v.profile())
}

// Configure 解析 Trae 专属配置。
//
// 上游按 X-Ide-Version-Code 下发不同模型目录：国内版构建号低于 20260806 时
// 拿不到 deepseek-v4.1-flash。门控再抬高时用户改配置即可，不必等发版。
// 键名按实例加前缀（traecn_ide_version / traeglobal_ide_version / …），
// 因为三条产品线的版本号体系不同，共用一个键会互相踩。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	prefix := v.config.VendorID + "_"
	version := strings.TrimSpace(raw[prefix+"ide_version"])
	versionCode := strings.TrimSpace(raw[prefix+"ide_version_code"])
	v.ideVersionMu.Lock()
	v.ideVersion = version
	v.ideVersionCode = versionCode
	v.ideVersionMu.Unlock()
}

// ---- 模型 ----

func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	// 借一个可用凭证探测上游目录（Catalog.StaticModels 只在有凭证时调用本方法）。
	cred, okCred := core.ProbeNativeCredential(v.Env, v.ID(), func(c *core.Credential) (*Credential, bool) {
		native, okNative := c.Native.(*Credential)
		return native, okNative
	})
	if !okCred || cred == nil {
		return nil, fmt.Errorf("trae: 无可用凭证，无法拉取模型目录")
	}
	models, errModels := v.catalog.fetchModels(ctx, cred, v.profile())
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI 把上游模型条目转成宿主契约类型，顺带记下倍率表。
//
// 倍率与清单必须出自同一轮探测：分开取会出现「模型换了一批、倍率还是
// 上一批」的错配。
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	core.FillCredits(&v.credits, models,
		func(model Model) string { return model.ID },
		func(model Model) string { return model.Credits })
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        model.Description,
			ContextLength:      int64(model.ContextWindow),
			MaxOutputTokens:    int64(model.MaxTokens),
			SupportsTools:      model.SupportsTools,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ModelCredits 实现 core.CreditsProvider：返回「官方模型 ID → 计费倍率」。
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// ---- 执行 ----

func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, "", ChatRequest{
		Credential: native,
		Profile:    v.profile(),
		Body:       req.Payload,
		Stream:     false,
		catalog:    &v.catalog,
	})
	if errChat != nil {
		return nil, core.ToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	return ChatStream(ctx, "", ChatRequest{
		Credential: native,
		Profile:    v.profile(),
		Body:       req.Payload,
		Stream:     true,
		catalog:    &v.catalog,
	}, sink.Emit)
}

// ---- 额度与签到 ----

func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	response, errQuota := FetchQuota(ctx, native, v.profile())
	if errQuota != nil {
		return nil, core.ToPluginError(errQuota)
	}
	return response, nil
}

func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	result, errCheckin := Checkin(ctx, native, v.profile())
	if errCheckin != nil {
		return nil, core.ToPluginError(errCheckin)
	}
	return &core.CheckinResult{
		Already: result.Already,
		Credit:  result.Credit,
		Message: result.Message,
	}, nil
}

// ---- 登录与续期 ----

func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.profile(), v.sessions)
}

func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, state, v.sessions)
}

// OwnsLoginSession 报告该登录会话是否由本实例持有。
//
// Trae 的会话表自持（回调监听要求实例隔离），没有登记到公共 LoginSessions，
// 根层路由登录请求时靠这个自证接口把 state 认回来。
func (v *adapter) OwnsLoginSession(sessionID string) bool {
	return v.sessions.owns(sessionID, v.profile().VendorID)
}

// LocalCallbackHint 返回控制台要展示的本机回调地址。
func (v *adapter) LocalCallbackHint() string { return v.sessions.callbackHint() }

// SubmitCallback 解析用户粘贴的 localhost 回调链接并完成登录。
func (v *adapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	return v.sessions.SubmitCallback(ctx, state, rawURL)
}

func (v *adapter) SupportsCheckin() bool { return SupportsCheckin(v.profile()) }

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor            = (*adapter)(nil)
	_ core.CallbackSubmitter = (*adapter)(nil)
	_ core.CheckinProvider   = (*adapter)(nil)
	_ core.Configurable      = (*adapter)(nil)
	_ core.LoginProvider     = (*adapter)(nil)
	_ core.LoginSessionOwner = (*adapter)(nil)
	_ core.QuotaProvider     = (*adapter)(nil)
	_ core.CreditsProvider   = (*adapter)(nil)
)
