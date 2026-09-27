package core

import (
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// SplitModelID 拆分模型 ID 为 (供应商标识, 裸模型名)。
//
// 本插件的模型以**裸名**注册（不带供应商前缀），跨供应商的同名模型由宿主按
// strings.EqualFold 合并。因此正常情况下模型 ID 就是裸名，本函数返回空前缀。
//
// 前缀协议仍然保留：模型禁用键与 extra_models 允许写 "workbuddycn:glm-5.2"
// 这样的限定形式，用于在多个供应商之间精确指定。取第一个冒号，前段必须是
// 已知的供应商标识或区域别名才剥离，否则视为裸名——避免把
// "gpt-5.5:turbo" 这类含冒号的真实模型名拆坏。
func SplitModelID(modelID string) (vendorID, bareModel string) {
	trimmed := strings.TrimSpace(modelID)
	index := strings.Index(trimmed, ":")
	if index < 0 {
		return "", trimmed
	}
	prefix := strings.ToLower(strings.TrimSpace(trimmed[:index]))
	if _, ok := VendorByID(prefix); !ok && !isRegionAlias(prefix) {
		return "", trimmed
	}
	return prefix, trimmed[index+1:]
}

// isRegionAlias 判断前缀是否为区域别名（cn / global）。
//
// 这是历史写法，比供应商 ID 短，保留以兼容旧的配置与客户端。
func isRegionAlias(prefix string) bool {
	switch prefix {
	case "cn", "global":
		return true
	}
	return false
}

// ModelKeyFor 生成模型在禁用表里的键：<vendorID>:<裸名>。
//
// 用「供应商标识 + 裸名」而不是裸名本身：同一个模型名可能来自多个供应商
// （如 glm-5.3 同时存在于 workbuddy 与 qoder），禁用其中一家不应影响另一家。
func ModelKeyFor(vendorID, modelID string) string {
	_, bare := SplitModelID(modelID)
	return strings.ToLower(strings.TrimSpace(vendorID)) + ":" + strings.TrimSpace(bare)
}

// ModelInfoToPluginAPI 把中立的模型描述转成宿主契约类型。
//
// vendorID 填充 OwnedBy；模型 ID 保持裸名（见 SplitModelID 的说明）。
func ModelInfoToPluginAPI(vendorID string, info ModelDescriptor) pluginapi.ModelInfo {
	id := strings.TrimSpace(info.ID)
	name := strings.TrimSpace(info.Name)
	if name == "" {
		name = id
	}
	out := pluginapi.ModelInfo{
		ID:                       id,
		Object:                   "model",
		OwnedBy:                  vendorID,
		Type:                     "chat",
		DisplayName:              name,
		Name:                     id,
		Description:              info.Description,
		ContextLength:            info.ContextLength,
		InputTokenLimit:          info.ContextLength,
		MaxCompletionTokens:      info.MaxOutputTokens,
		OutputTokenLimit:         info.MaxOutputTokens,
		SupportedInputModalities: []string{"text"},
		SupportedParameters:      []string{"temperature", "top_p", "max_tokens", "stream", "tools", "tool_choice"},
	}
	if info.SupportsImages {
		out.SupportedInputModalities = append(out.SupportedInputModalities, "image")
	}
	if info.SupportsTools {
		out.SupportedGenerationMethods = append(out.SupportedGenerationMethods, "tool_calls")
	}
	if len(info.Efforts) > 0 || info.DefaultEffort != "" {
		out.Thinking = &pluginapi.ThinkingSupport{
			Levels:         info.Efforts,
			DynamicAllowed: info.DefaultEffort != "",
			ZeroAllowed:    info.CanDisableThinking,
		}
	}
	return out
}

// ModelDescriptor 是供应商提供的模型元数据（供应商无关的中立形态）。
//
// 各供应商的上游模型结构差异很大，这里只保留注册给宿主时真正用到的字段，
// 供应商在自己的包里做一次转换。
type ModelDescriptor struct {
	ID              string
	Name            string
	Description     string
	ContextLength   int64
	MaxOutputTokens int64
	Efforts         []string
	DefaultEffort   string
	SupportsImages  bool
	SupportsTools   bool
	// CanDisableThinking 报告该模型是否允许关闭思考。
	CanDisableThinking bool
}
