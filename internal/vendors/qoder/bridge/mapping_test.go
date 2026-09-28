package bridge

import (
	"testing"
)

func TestResolveQoderModelKey(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"glm-5.3-flash", "gfmodel"},
		{"GLM-5.3-Flash", "gfmodel"},
		{"gfmodel", "gfmodel"},
		{"GFMODEL", "gfmodel"},
		{"glm-5.3", "gmodel"},
		{"GLM-5.3", "gmodel"},
		{"gmodel", "gmodel"},
		{"kimi-k3", "kmodel_latest"},
		{"Kimi-K3", "kmodel_latest"},
		{"kmodel_latest", "kmodel_latest"},
		{"deepseek-v4-pro", "dmodel"},
		{"DeepSeek-V4-Pro", "dmodel"},
		{"dmodel", "dmodel"},
		{"qwen3.8-max", "qmodel_38max"},
		{"Qwen3.8-Max", "qmodel_38max"},
		{"qmodel_38max", "qmodel_38max"},
	}

	for _, c := range cases {
		got := ResolveQoderModelKey(c.input)
		if got != c.expected {
			t.Errorf("ResolveQoderModelKey(%q) = %q; want %q", c.input, got, c.expected)
		}
	}
}

func TestMapModel(t *testing.T) {
	// 验证 MapModel 能正确解析标准小写模型名、展示名以及原始 key
	cases := []struct {
		agent    string
		model    string
		expected string
	}{
		{"codex", "glm-5.3-flash", "gfmodel"},
		{"codex", "GLM-5.3-Flash", "gfmodel"},
		{"codex", "gfmodel", "gfmodel"},
		{"codex", "kimi-k3", "kmodel_latest"},
		{"codex", "Kimi-K3", "kmodel_latest"},
		{"claude", "claude-sonnet-4-6", "gmodel"}, // sonnet 模糊匹配兜底
	}

	for _, c := range cases {
		got := MapModel(c.agent, c.model)
		if got != c.expected {
			t.Errorf("MapModel(%q, %q) = %q; want %q", c.agent, c.model, got, c.expected)
		}
	}
}

func TestRegisterKnownModels(t *testing.T) {
	dynamic := []QoderModel{
		{Key: "custom_sku_1", DisplayName: "Custom-Model-X"},
	}
	RegisterKnownModels(dynamic)

	if got := ResolveQoderModelKey("custom-model-x"); got != "custom_sku_1" {
		t.Errorf("ResolveQoderModelKey(\"custom-model-x\") = %q; want custom_sku_1", got)
	}
	if got := ResolveQoderModelKey("Custom-Model-X"); got != "custom_sku_1" {
		t.Errorf("ResolveQoderModelKey(\"Custom-Model-X\") = %q; want custom_sku_1", got)
	}
	if got := ResolveQoderModelKey("custom_sku_1"); got != "custom_sku_1" {
		t.Errorf("ResolveQoderModelKey(\"custom_sku_1\") = %q; want custom_sku_1", got)
	}
}
