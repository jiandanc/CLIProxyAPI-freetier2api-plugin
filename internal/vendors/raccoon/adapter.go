package raccoon

// 本文件把小浣熊（商汤 Raccoon）协议适配成 core.Vendor。
//
// 能力面（对齐 Agent2API 系列实测协议）：
//   - 凭证解析与偏严归属（JWT access/refresh 对 + uid）；
//   - chat 走 LLM 网关（OpenAI 形状，原样转发）；
//   - 模型目录实时拉取（model_catalog），拉不到回落静态单条；
//   - token 自动续期（POST /refresh，refresh_token 可能轮换）；
//   - 积分额度查询（GET {points_base}/balance）；
//   - 每日签到（登录领取 grant，幂等）；
//   - 扫码登录（qrcode_code 轮询，无需回调地址）与授权码换票兜底。
//
// 小浣熊只有国内一个站点（xiaohuanxiong.com），无区域之分。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// adapter 是 core.Vendor 在小浣熊上的实现。
type adapter struct {
	*core.NativeAdapter[*Credential]
	// llmBase / authBase / pointsBase / origin 是各域基址覆盖（Configure 填充）。
	llmBase    string
	authBase   string
	pointsBase string
	origin     string

	// credits 是「官方模型 ID → 计费倍率文案」，与 models 同源同生命周期。
	//
	// 宿主契约（pluginapi.ModelInfo）没有倍率字段，控制台只能通过
	// core.CreditsProvider 拿到，因此和模型清单在同一轮解析里一起产出。
	credits core.CreditTable
}

// Instances 构造小浣熊实例（只有一个，没有区域之分）。
func Instances(env core.Env) []core.Vendor {
	inst := &adapter{}
	// 钩子捕获 inst：Configure 写入的基址覆盖对续期同样生效。
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
			uid := native.UIDOrTokenID()
			return core.CredentialSeed{
				VendorID: VendorID,
				Label:    core.FirstNonEmpty(native.NicknameValue(), uid, core.MaskToken(native.AccessTokenValue())),
				FileID:   core.FileIDFromFileName(fileName), UID: uid,
				Token: native.AccessTokenValue(), RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAtValue(), AuthMode: "oauth",
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			accessBefore, refreshBefore := native.Snapshot()
			access, refresh, expiresAt, uid, errRefresh := RefreshToken(ctx, inst.resolveAuthBase(), native)
			if errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			if expiresAt == 0 {
				expiresAt = core.JWTExpiry(access)
			}
			native.ApplyTokens(access, refresh, expiresAt)
			native.ApplyIdentity(uid, "")
			refreshed := !native.UnchangedSince(accessBefore, refreshBefore)
			return core.NativeRefresh[*Credential]{
				Native: native, Token: native.AccessTokenValue(), RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAtValue(), Changed: refreshed,
			}, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeCredentialJSON(original, native)
		},
	})
	return []core.Vendor{inst}
}

// Configure 解析小浣熊专属配置（各域基址覆盖）。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	v.llmBase = strings.TrimSpace(raw["raccoon_llm_base_url"])
	v.authBase = strings.TrimSpace(raw["raccoon_auth_base_url"])
	v.pointsBase = strings.TrimSpace(raw["raccoon_points_base_url"])
	v.origin = strings.TrimSpace(raw["raccoon_origin"])
}

// ConfigFields 声明小浣熊专属配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "raccoon_llm_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "LLM 网关基址覆盖（默认 " + DefaultLLMBase + "）。"},
		{Name: "raccoon_auth_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "鉴权基址覆盖（默认 " + DefaultAuthBase + "）。"},
		{Name: "raccoon_points_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "积分基址覆盖（默认 " + DefaultPointsBase + "）。"},
		{Name: "raccoon_origin", Type: pluginapi.ConfigFieldTypeString, Description: "主站 origin 覆盖（默认 " + DefaultOrigin + "）。"},
	}
}

// Match 判断一份凭证是否属于本供应商。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// ---- 模型 ----

// StaticModels 返回模型目录。
//
// 探测需要一把有效凭证；借不到就返回错误（不编清单）。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	token := v.probeToken()
	if token == "" {
		return nil, fmt.Errorf("no raccoon credential available for model probe")
	}
	models, errModels := FetchModels(ctx, v.resolveLLMBase(), token)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// ModelsForAuth 返回该凭证可用的模型。
//
// 用**该凭证自己的 token** 去探测；目录失败时回落静态兜底单条而不是
// 返回空（小浣熊目录接口稳定，兜底只是为了目录抖动时账号还能用）。
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	models, errModels := FetchModels(ctx, v.resolveLLMBase(), native.AccessTokenValue())
	if errModels != nil {
		logger.Info("raccoon: model catalog fallback to static: %v", errModels)
		models = StaticFallbackModels()
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI 把目录条目转成宿主契约类型，顺带记下倍率表。
//
// 倍率与清单必须出自同一轮探测：分开取会出现「模型换了一批、倍率还是
// 上一批」的错配（与 cline / qoder 的处理一致）。
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	core.FillCredits(&v.credits, models,
		func(model Model) string { return model.ID },
		func(model Model) string { return model.Credits })
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 id,
			Name:               core.FirstNonEmpty(model.Name, id),
			Description:        core.FirstNonEmpty(model.Description, "小浣熊上游模型"),
			ContextLength:      model.ContextWindow,
			MaxOutputTokens:    model.MaxOutputTokens,
			SupportsTools:      model.SupportsTools,
			SupportsImages:     model.SupportsImages,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ModelCredits 实现 core.CreditsProvider：返回「官方模型 ID → 计费倍率」。
//
// 倍率取自目录条目的 billing_effective_multiplier（实付）并回落
// billing_multiplier，统一经 core.FormatRate 归一化成 "0.80X" 这类文案；
// 上游没给的模型不进表，控制台据此显示「—」而不是把「没提供」画成「免费」。
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, v.resolveLLMBase(), ChatRequest{
		Token: native.AccessTokenValue(),
		Body:  req.Payload,
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
//
// 只投递上游的原生 SSE 数据载荷，不做 `data: ` 包装与分帧——包装由宿主
// 按声明格式完成。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	resp, errStream := ChatStream(ctx, v.resolveLLMBase(), ChatRequest{
		Token: native.AccessTokenValue(),
		Body:  req.Payload,
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// ---- 额度与签到 ----

// Quota 查询账号积分余额。
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	available, errBalance := FetchBalance(ctx, v.resolvePointsBase(), native)
	if errBalance != nil {
		return nil, core.ToPluginError(errBalance)
	}
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "points", TierName: "小浣熊积分"},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "可用积分", Value: float64(available), Format: "number"},
		},
	}, nil
}

// Checkin 执行一次每日签到（登录领取积分，幂等）。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	_, errGrant := ClaimDailyGrant(ctx, v.resolveOrigin(), native)
	if errGrant != nil {
		if errGrant == ErrCheckinNotGranted {
			return &core.CheckinResult{Already: true, Message: "今日积分已发放（幂等命中）"}, nil
		}
		return nil, core.ToPluginError(errGrant)
	}
	return &core.CheckinResult{Message: "登录领取成功，积分已入账"}, nil
}

func (v *adapter) SupportsCheckin() bool { return true }

// ---- 登录与续期 ----

// LoginStart 发起一次网页授权登录：生成一次性 state 并登记会话，
// 返回官方授权页 URL（用户在页面登录后浏览器跳 office-raccoon:// 回调，
// 把整条链接粘回控制台由 SubmitCallback 完成换票）。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	state := core.RandomHex(32)
	sessionID := VendorID + "_" + state
	StoreLoginSession(v.Env.Logins, sessionID, &PendingLogin{State: state})
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       AuthorizeURLWith(v.resolveOrigin(), loginAppName, state),
		State:     sessionID,
		ExpiresAt: time.Now().Add(qrLoginTTL),
		Metadata:  map[string]any{core.VendorKey: VendorID},
	}, nil
}

// LoginPoll 轮询一次登录状态。
//
// 授权页模式没有可轮询的上游端点：凭证只在用户把回调链接粘回来时
// 经 SubmitCallback 到达。这里只回报进度；state 失配（用户粘贴）由
// SubmitCallback 拒绝。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	_, exists, _ := CheckLoginSession(v.Env.Logins, state)
	if !exists {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: pendingMessage,
	}, nil
}

// LocalCallbackHint 返回控制台要展示的回调形态（授权码换票兜底用）。
func (v *adapter) LocalCallbackHint() string {
	return "office-raccoon://auth/callback?code=…&state=…"
}

// SubmitCallback 解析用户粘贴的回调链接（或裸授权码）并换票完成登录。
//
// 这是扫码登录的兜底：用户也可以在官方页面走 office-raccoon:// 回调，
// 把地址栏整条链接粘回来换票。
func (v *adapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	session, pollState := v.Env.Logins.Poll(strings.TrimSpace(state))
	if pollState == core.PollMissing {
		return nil, core.NewPluginError("session_not_found", "登录会话不存在或已过期，请重新发起登录", http.StatusNotFound)
	}
	pending, okPending := session.Data.(*PendingLogin)
	if !okPending {
		return nil, core.NewPluginError("login_session_broken", "登录会话数据已损坏，请重新发起登录", http.StatusBadRequest)
	}
	code, callbackState, okCode := ParseCallbackCode(rawURL)
	if !okCode {
		return nil, core.NewPluginError("callback_parse_failed",
			"回调链接解析失败，请确认粘贴的是完整回调链接（office-raccoon://auth/callback?code=…）",
			http.StatusBadRequest)
	}
	if !CheckCallbackState(pending, callbackState) {
		return nil, core.NewPluginError("callback_state_mismatch",
			"登录回调 state 校验失败，请重新发起登录",
			http.StatusBadRequest)
	}
	access, refresh, expiresAt, uid, errExchange := ExchangeAuthorizationCode(ctx, v.resolveAuthBase(), code)
	if errExchange != nil {
		return nil, core.ToPluginError(errExchange)
	}
	credential := &Credential{AccessToken: access, RefreshToken: refresh, ExpiresAt: expiresAt}
	if expiresAt == 0 {
		credential.ExpiresAt = core.JWTExpiry(access)
	}
	if uid != "" {
		credential.ApplyIdentity(uid, "")
	}
	if uidInfo, nickname, errInfo := FetchUserInfo(ctx, v.resolveAuthBase(), credential); errInfo == nil {
		credential.ApplyIdentity(uidInfo, nickname)
	}
	if credential.UIDOrTokenID() == "" {
		return nil, core.NewPluginError("login_failed", "授权码换票未返回用户标识", http.StatusInternalServerError)
	}
	storageJSON, errStorage := credential.StorageJSON()
	if errStorage != nil {
		return nil, core.NewPluginError("login_failed", "构建凭证失败："+errStorage.Error(), http.StatusInternalServerError)
	}
	v.Env.Logins.Delete(strings.TrimSpace(state))
	return &core.CallbackSubmitResult{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "小浣熊登录成功",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(VendorID, credential.UIDOrTokenID()),
			Label:       core.FirstNonEmpty(credential.NicknameValue(), credential.UIDOrTokenID()),
			StorageJSON: storageJSON,
		},
	}, nil
}

// MergeCredentialJSON 实现存储合并（见 credential.go）。

// ---- 内部工具 ----

func (v *adapter) resolveLLMBase() string    { return BaseURL(v.llmBase, DefaultLLMBase) }
func (v *adapter) resolveAuthBase() string   { return BaseURL(v.authBase, DefaultAuthBase) }
func (v *adapter) resolvePointsBase() string { return BaseURL(v.pointsBase, DefaultPointsBase) }
func (v *adapter) resolveOrigin() string     { return BaseURL(v.origin, DefaultOrigin) }

// probeToken 借一个本供应商的可用凭证取 access token（供模型探测用）。
func (v *adapter) probeToken() string {
	native, okNative := core.ProbeNativeCredential(v.Env, VendorID, func(cred *core.Credential) (*Credential, bool) {
		value, okValue := cred.Native.(*Credential)
		return value, okValue
	})
	if !okNative || native == nil {
		return ""
	}
	return native.AccessTokenValue()
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.CallbackSubmitter    = (*adapter)(nil)
	_ core.CheckinProvider      = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.CreditsProvider      = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.Refresher            = (*adapter)(nil)
	_ core.StorageMerger        = (*adapter)(nil)
)
