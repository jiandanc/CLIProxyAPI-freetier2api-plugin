package core

import (
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
)

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
		// 参数能力必须跟着模型声明走：把 tools / tool_choice 无条件列给所有
		// 模型，会让客户端对不支持工具调用的模型也渲染工具开关，发出去就是
		// 上游 400。基础采样参数是全部对话模型都接受的，保留。
		SupportedParameters: []string{"temperature", "top_p", "max_tokens", "stream"},
	}
	if info.SupportsImages {
		out.SupportedInputModalities = append(out.SupportedInputModalities, "image")
	}
	if info.SupportsTools {
		out.SupportedGenerationMethods = append(out.SupportedGenerationMethods, "tool_calls")
		out.SupportedParameters = append(out.SupportedParameters, "tools", "tool_choice")
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

// ModelSupportsImages 报告模型是否声明了图片输入能力。
func ModelSupportsImages(model pluginapi.ModelInfo) bool {
	for _, modality := range model.SupportedInputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
}

// ModelSupportsTools 报告模型是否声明了工具调用能力。
func ModelSupportsTools(model pluginapi.ModelInfo) bool {
	for _, method := range model.SupportedGenerationMethods {
		if strings.EqualFold(strings.TrimSpace(method), "tool_calls") {
			return true
		}
	}
	return false
}
