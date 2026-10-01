package main

// 本文件把 OpenCode ZEN 适配成 core.Vendor。
//
// 与其它供应商的关键差异：
//   - **没有登录流程**：凭证是用户自己申请的静态 API key，没有 OAuth、
//     没有设备码、没有 token 交换。因此 LoginStart / LoginPoll 返回明确的
//     「本供应商不支持登录，请用 API key 添加」；
//   - **没有区域**：只有一个部署（opencode.ai），Region() 返回空串；
//   - **没有额度接口**：上游不提供余额查询，Quota 返回「不支持」；
//   - **没有续期**：key 不会变，Refresh 只做一次可用性校验（列模型）。

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/opencodezen"
)

// opencodeZenVendor 是 core.Vendor 在 OpenCode ZEN 上的实现。
type opencodeZenVendor struct{}

// newOpenCodeZenVendor 构造 ZEN 实例（只有一个，没有区域之分）。
func newOpenCodeZenVendor() core.Vendor { return &opencodeZenVendor{} }

func (v *opencodeZenVendor) ID() string     { return opencodezen.VendorID }
func (v *opencodeZenVendor) Name() string   { return opencodezen.VendorName }
func (v *opencodeZenVendor) Region() string { return "" }

// ModelPrefix 返回空：本供应商用裸模型名注册。
func (v *opencodeZenVendor) ModelPrefix() string { return "" }

// AuthMode 报告本供应商的凭证形态。
//
// 页面据此决定账号列是否脱敏：API key 本身就是凭证，明文渲染进 HTML
// 等于把它写进浏览器缓存与截图里。
func (v *opencodeZenVendor) AuthMode() string { return "apikey" }

// ---- 凭证 ----

// Match 判断一份凭证是否属于本供应商。
func (v *opencodeZenVendor) Match(fileName, provider string, raw map[string]any) bool {
	return opencodezen.LooksLikeCredential(raw, fileName, provider)
}

// Parse 把凭证 JSON 解析成 core.Credential。
func (v *opencodeZenVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := opencodezen.ParseCredential(raw)
	if errParse != nil {
		return nil, errParse
	}
	// Label 兜底用**脱敏后**的 key：Label 会进日志、进管理接口响应、
	// 进页面 HTML，绝不能是完整凭证。
	label := native.Label
	if label == "" {
		label = opencodezen.MaskedKey(native.APIKey)
	}
	return &core.Credential{
		VendorID: opencodezen.VendorID,
		Label:    label,
		// UID 用脱敏 key：它只用于状态去重与展示，不需要（也不该）是完整 key。
		FileID:   strings.TrimSuffix(strings.TrimSpace(fileName), ".json"),
		Token:    native.APIKey,
		AuthMode: "apikey",
		Native:   native,
	}, nil
}

// nativeCredential 取回协议层凭证对象。
func (v *opencodeZenVendor) nativeCredential(cred *core.Credential) (*opencodezen.Credential, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", 401)
	}
	if native, okNative := cred.Native.(*opencodezen.Credential); okNative && native != nil {
		return native, nil
	}
	return nil, core.NewPluginError("credential_missing",
		"credential was not parsed by the opencodezen vendor", 401)
}

// ---- 模型 ----

// StaticModels 返回 ZEN 的模型清单。
//
// 需要一把有效的 key 才能探测（上游模型接口要认证）。借一个本供应商的
// 可用凭证去打；借不到就返回空清单——不编一份假清单，否则用户会看到
// 调用必失败的模型。
func (v *opencodeZenVendor) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	apiKey := probeAPIKey()
	if apiKey == "" {
		return nil, fmt.Errorf("no opencodezen credential available for model probe")
	}
	models, errModels := opencodezen.FetchModels(ctx, baseURLOverride(opencodezen.VendorID), apiKey)
	if errModels != nil {
		return nil, errModels
	}
	return zenModelsToPluginAPI(models), nil
}

// ModelsForAuth 返回该凭证可用的模型。
//
// ZEN 的清单是全局的（不按账号区分），因此与 StaticModels 同源；
// 但用**该凭证自己的 key** 去探测，这样 key 失效时该账号自然拿不到模型，
// 宿主据此把它从选号里淘汰。
func (v *opencodeZenVendor) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	models, errModels := opencodezen.FetchModels(ctx, baseURLOverride(opencodezen.VendorID), native.APIKey)
	if errModels != nil {
		// 探测失败返回空清单而不是报错：宿主会因为空清单跳过该凭证，
		// 这正是我们想要的结果（避免用坏 key 打上游）。
		logger.Debug("opencodezen: model probe failed: %v", errModels)
		return nil, nil
	}
	return zenModelsToPluginAPI(models), nil
}

// zenModelsToPluginAPI 把上游模型条目转成宿主契约类型。
func zenModelsToPluginAPI(models []opencodezen.Model) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = id
		}
		contextWindow := model.ContextWindow
		if contextWindow == 0 {
			contextWindow = defaultContextWindowFor(id)
		}
		maxOutput := model.MaxOutputTokens
		if maxOutput == 0 {
			maxOutput = defaultMaxOutputFor(id)
		}
		out = append(out, core.ModelInfoToPluginAPI("", core.ModelDescriptor{
			ID:                 id,
			Name:               name,
			Description:        "OpenCode ZEN 上游模型",
			ContextLength:      contextWindow,
			MaxOutputTokens:    maxOutput,
			SupportsTools:      model.SupportsTools,
			CanDisableThinking: !model.IsReasoning,
		}))
	}
	return out
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *opencodeZenVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := opencodezen.Chat(ctx, baseURLOverride(opencodezen.VendorID), opencodezen.ChatRequest{
		APIKey: native.APIKey,
		Body:   req.Payload,
		Stream: false,
	})
	if errChat != nil {
		return nil, errorToPluginError(errChat)
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {jsonContentType}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
//
// 只投递上游的原生 SSE 数据载荷，不做 `data: ` 包装与分帧——包装由宿主
// 按声明格式完成。插件侧重复分帧会与宿主产生不一致的行为。
func (v *opencodeZenVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return errNative
	}
	resp, errStream := opencodezen.ChatStream(ctx, baseURLOverride(opencodezen.VendorID), opencodezen.ChatRequest{
		APIKey: native.APIKey,
		Body:   req.Payload,
		Stream: true,
	})
	if errStream != nil {
		return errorToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return forwardRawSSE(resp.Body, sink)
}

// CountTokens 估算 token 数（按字节数除以 3，不调上游）。
func (v *opencodeZenVendor) CountTokens(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (int, error) {
	if len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

// ---- 额度与签到 ----

// Quota 报告本供应商没有额度查询接口。
//
// ZEN 上游不提供余额查询（实测：其网关项目只做本地 token 计数）。
// 返回明确的「不支持」比编一个数字诚实——页面会显示「—」。
func (v *opencodeZenVendor) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	return nil, core.NewPluginError("quota_unsupported",
		"OpenCode ZEN 上游不提供额度查询接口（额度以订阅页为准）", http.StatusNotImplemented)
}

// SupportsCheckin 报告本供应商是否提供签到（ZEN 没有）。
func (v *opencodeZenVendor) SupportsCheckin() bool { return false }

// Checkin 报告本供应商不支持签到。
func (v *opencodeZenVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	return nil, nil
}

// ---- 登录与续期 ----

// LoginStart 报告本供应商不支持登录。
//
// ZEN 的凭证是用户到 OpenCode 网站申请的静态 API key，上游没有设备授权
// 或 OAuth 流程（实测：其网关项目零 OAuth 代码）。因此这里返回明确的
// 说明，而不是假装开始一个永远不会完成的登录。
func (v *opencodeZenVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return nil, core.NewPluginError("login_unsupported",
		"OpenCode ZEN 不支持登录：请到 OpenCode 官网申请 API key 后，用「添加 API Key」方式添加", http.StatusNotImplemented)
}

// LoginPoll 报告本供应商不支持登录轮询。
func (v *opencodeZenVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return nil, core.NewPluginError("login_unsupported",
		"OpenCode ZEN 不支持登录", http.StatusNotImplemented)
}

// Refresh 校验凭证是否仍然可用。
//
// API key 不会轮换，因此这里**只校验不写回**（refreshed 恒为 false）：
// 列一次模型能验证 key 的有效性，成功即说明账号可用。
func (v *opencodeZenVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	if _, errModels := opencodezen.FetchModels(ctx, baseURLOverride(opencodezen.VendorID), native.APIKey); errModels != nil {
		return nil, false, errModels
	}
	return cred, false, nil
}

// Tasks 返回本实例支持的任务动作（ZEN 没有任务体系）。
func (v *opencodeZenVendor) Tasks() []core.Task { return nil }

// SupportsLogin 报告本供应商不支持登录。
func (v *opencodeZenVendor) SupportsLogin() bool { return false }

// ---- 内部工具 ----

// probeAPIKey 借一个本供应商的可用凭证取 API key（供模型探测用）。
func probeAPIKey() string {
	ctx := context.Background()
	entries, errList := listHostAuths(ctx, "")
	if errList != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		stored, okStored := getAuthJSONByIndex(ctx, "", entry.AuthIndex)
		if !okStored {
			continue
		}
		credential, okParse := parseVendorCredential(stored, entry.Name, nil)
		if !okParse || credential.VendorIDValue() != opencodezen.VendorID {
			continue
		}
		if native, okNative := credential.Native.(*opencodezen.Credential); okNative {
			return native.APIKey
		}
	}
	return ""
}

// init 注册 ZEN 实例。
func init() {
	core.RegisterVendor(newOpenCodeZenVendor())
}
