package core

import (
	"slices"
	"testing"
)

// TestModelInfoDeclaresToolsOnlyWhenSupported 锁定 supported_parameters 的声明。
//
// 之前所有模型都无条件列 tools / tool_choice，连不支持工具调用的模型也让
// 客户端渲染工具开关——用户勾上就发，上游直接 400。能力声明必须跟着
// SupportsTools 走；基础采样参数对所有对话模型都成立，始终保留。
func TestModelInfoDeclaresToolsOnlyWhenSupported(t *testing.T) {
	withTools := ModelInfoToPluginAPI("demo", ModelDescriptor{ID: "tooly", SupportsTools: true})
	withoutTools := ModelInfoToPluginAPI("demo", ModelDescriptor{ID: "plain"})

	for _, param := range []string{"temperature", "top_p", "max_tokens", "stream"} {
		if !slices.Contains(withTools.SupportedParameters, param) {
			t.Fatalf("支持工具的模型缺少基础参数 %q: %v", param, withTools.SupportedParameters)
		}
		if !slices.Contains(withoutTools.SupportedParameters, param) {
			t.Fatalf("普通模型缺少基础参数 %q: %v", param, withoutTools.SupportedParameters)
		}
	}
	for _, param := range ToolParameters {
		if !slices.Contains(withTools.SupportedParameters, param) {
			t.Fatalf("支持工具的模型缺少 %q: %v", param, withTools.SupportedParameters)
		}
		if slices.Contains(withoutTools.SupportedParameters, param) {
			t.Fatalf("不支持工具的模型不应声明 %q: %v", param, withoutTools.SupportedParameters)
		}
	}

	if !slices.Contains(withTools.SupportedGenerationMethods, "tool_calls") {
		t.Fatalf("支持工具的模型缺少 tool_calls: %v", withTools.SupportedGenerationMethods)
	}
	if slices.Contains(withoutTools.SupportedGenerationMethods, "tool_calls") {
		t.Fatalf("不支持工具的模型不应声明 tool_calls: %v", withoutTools.SupportedGenerationMethods)
	}
}
