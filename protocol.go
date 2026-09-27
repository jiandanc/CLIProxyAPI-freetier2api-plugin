package main

// 本文件实现模型 ID 的 realm 前缀协议与错误语义映射。

import (
	"errors"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// 模型 ID 形如 "cn:glm-5.2" / "global:gpt-5.5"。
//
// 这套前缀协议**沿用原项目**（internal/server/resolve_model.go）：
// 取第一个冒号，前段恰为 cn / global（大小写敏感）才剥离，否则视为裸名。
//
// 前缀是网关侧的路由协议，上游不认——出站前必须剥回裸名。
// 它的作用：让宿主的「按凭证注册模型」机制把 cn 前缀的模型只挂在 cn 凭证上，
// 从而实现双域隔离（详见 models.go 的 ModelsForAuth）。
const realmSeparator = ":"

// SplitModelID 拆分模型 ID 为 (realm, 裸模型名)。
//
// 无前缀或前缀非法时 realm 返回空串，表示「由凭证决定域」。
func SplitModelID(modelID string) (realm, bareModel string) {
	trimmed := strings.TrimSpace(modelID)
	index := strings.Index(trimmed, realmSeparator)
	if index < 0 {
		return "", trimmed
	}
	prefix := trimmed[:index]
	switch prefix {
	case string(workbuddy.RegionCN):
		return string(workbuddy.RegionCN), trimmed[index+1:]
	case string(workbuddy.RegionGlobal):
		return string(workbuddy.RegionGlobal), trimmed[index+1:]
	}
	return "", trimmed
}

// PrefixModelID 给裸模型名加上 realm 前缀。
func PrefixModelID(region workbuddy.Region, bareModel string) string {
	return string(region) + realmSeparator + strings.TrimSpace(bareModel)
}

// ModelInfoToPluginAPI 把内部模型元数据转成宿主契约类型。
//
// functionCalling 与 reasoning 的档位会一并透出，客户端据此渲染能力提示。
func ModelInfoToPluginAPI(region workbuddy.Region, model workbuddy.ModelInfo) pluginapi.ModelInfo {
	id := strings.TrimSpace(model.ID)
	name := strings.TrimSpace(model.Name)
	if name == "" {
		name = model.ID
	}
	info := pluginapi.ModelInfo{
		ID:                       id,
		Object:                   "model",
		OwnedBy:                  providerKey,
		Type:                     "chat",
		DisplayName:              name,
		Name:                     model.ID,
		Description:              describeModel(region, model),
		ContextLength:            model.ContextWindow,
		InputTokenLimit:          model.ContextWindow,
		MaxCompletionTokens:      model.MaxTokens,
		OutputTokenLimit:         model.MaxTokens,
		SupportedInputModalities: []string{"text"},
		SupportedParameters:      []string{"temperature", "top_p", "max_tokens", "stream", "tools", "tool_choice"},
	}
	if model.SupportsImages {
		info.SupportedInputModalities = append(info.SupportedInputModalities, "image")
	}
	if model.SupportsToolCall {
		info.SupportedGenerationMethods = append(info.SupportedGenerationMethods, "tool_calls")
	}
	if len(model.Efforts) > 0 || model.DefaultEffort != "" {
		info.Thinking = &pluginapi.ThinkingSupport{
			Levels:         model.Efforts,
			DynamicAllowed: model.DefaultEffort != "",
			ZeroAllowed:    model.CanDisableThinking,
		}
	}
	return info
}

// describeModel 拼接模型说明（带积分倍率前缀）。
//
// 倍率是用户最关心的成本信息，放在说明最前面。
func describeModel(region workbuddy.Region, model workbuddy.ModelInfo) string {
	description := strings.TrimSpace(model.Description)
	credits := strings.TrimSpace(model.Credits)
	if credits == "" {
		return description
	}
	// 上游的 credits 形如 "x0.05"，去掉尾部 "credits" 字样避免 "x0.05 credits credit"。
	credits = strings.TrimSuffix(credits, "credit")
	credits = strings.TrimSuffix(credits, "credits")
	prefix := "[" + strings.TrimSpace(credits) + " credit] "
	return prefix + description
}

// errorToPluginError 把上游分类错误转成插件错误。
//
// 这一步是「原项目账号处置矩阵」的落点：插件把 Kind 翻译成宿主认得的
// HTTP 状态码与可重试标记，由宿主决定冷却/禁用/轮转。
func errorToPluginError(err error) error {
	if err == nil {
		return nil
	}
	var upstreamErr *workbuddy.Error
	if !asUpstreamError(err, &upstreamErr) {
		// 非上游分类错误（本地问题）：按 502 报告，让宿主换号重试。
		return retryablePluginError("workbuddy_request_failed", err.Error(), 502)
	}
	status := upstreamErr.HTTPStatus()
	code := "workbuddy_" + upstreamErr.Kind.String()
	if upstreamErr.Retryable() {
		return retryablePluginError(code, upstreamErr.Msg, status)
	}
	return newPluginError(code, upstreamErr.Msg, status)
}

// asUpstreamError 判断错误是否为上游分类错误。
func asUpstreamError(err error, target **workbuddy.Error) bool {
	if err == nil {
		return false
	}
	var upstreamErr *workbuddy.Error
	if errors.As(err, &upstreamErr) {
		*target = upstreamErr
		return true
	}
	return false
}

// RealmForRequest 决定一次请求实际使用的域。
//
// 模型带前缀时以前缀为准；不带前缀时由凭证决定——
// 这样用户手打裸模型名也能工作，代价是「裸名 + 双域账号」时域不可预测。
func RealmForRequest(modelID string, cred *workbuddy.Credential) (workbuddy.Region, string) {
	realm, bare := SplitModelID(modelID)
	if realm != "" {
		return workbuddy.NormalizeRegion(realm), bare
	}
	if cred != nil {
		return cred.Realm(), bare
	}
	return workbuddy.RegionCN, bare
}

// cbProviderKey 是 cb 包里的 provider 键（凭证归属判定与请求头归属都用它）。
// 定义在这里是为了让编译期断言能校验两处一致——不一致会让凭证归属判定失效。
const cbProviderKey = workbuddy.ProviderKey
