package trae

import (
	"encoding/json"
	"testing"
)

// TestConsumptionRateParsesVariants 锁定计费倍率的解析。
//
// 上游把倍率藏在 display_contact_config.consumption_rate 里，键名在
// snake_case / camelCase 两态之间摇摆；enable=false 表示不参与计费
// （等价于「没有倍率」），rate=0 是合法的限免档，必须保留。
func TestConsumptionRateParsesVariants(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"snake", `{"consumption_rate":{"enable":true,"data":{"rate":0.94}}}`, "0.94"},
		{"camel", `{"consumptionRate":{"Enable":true,"Data":{"Rate":"0.7"}}}`, "0.7"},
		{"zero is legal", `{"consumption_rate":{"data":{"rate":0}}}`, "0"},
		{"disabled at rate", `{"consumption_rate":{"enable":false,"data":{"rate":0.9}}}`, ""},
		{"disabled at data", `{"consumption_rate":{"data":{"enable":false,"rate":0.9}}}`, ""},
		{"missing", `{}`, ""},
		{"null", ``, ""},
	}
	for _, tc := range cases {
		if got := consumptionRate(json.RawMessage(tc.raw)); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAdapterModelCredits 锁定倍率表与清单同源。
func TestAdapterModelCredits(t *testing.T) {
	v := &adapter{}
	v.modelsToPluginAPI([]Model{
		{ID: "glm-5.3", Credits: "0.94"},
		{ID: "plain"},
	})
	credits := v.ModelCredits()
	if got := credits["glm-5.3"]; got != "0.94X" {
		t.Fatalf("倍率 = %q, want 0.94X", got)
	}
	if _, exists := credits["plain"]; exists {
		t.Fatal("没有倍率的模型不应进表")
	}
}

// TestCatalogEntriesParsesCamelCaseOuter 锁定外层键名两态的端到端解析。
//
// 上游新一代响应把 display_contact_config / extra_config 换成了 camelCase。
// 只认 snake_case 时不会报错，只会让倍率与工具能力静默变成「未知」，所以
// 这里从 JSON 原文一路断言到 catalogEntries 的产出。
func TestCatalogEntriesParsesCamelCaseOuter(t *testing.T) {
	const body = `{"config_info_list":[
	 {"config_name":"glm-5.3","config_switch":true,"usage":"chat_completion",
	  "context_window_tokens":{"dev":200000},
	  "display_config":{"display_name":"GLM-5.3","model_capability":"reasoning_model"},
	  "extraConfig":"{\"native_function_call\":true}",
	  "displayContactConfig":{"consumptionRate":{"enable":true,"data":{"rate":0.94}}},
	  "model_detail_list":[{"model_name":"glm-5.3__dev","max_tokens":32000}]},
	 {"config_name":"legacy","config_switch":true,"usage":"chat_completion",
	  "display_config":{"display_name":"Legacy"},
	  "extra_config":"{\"native_function_call\":false}",
	  "display_contact_config":{"consumption_rate":{"data":{"rate":0}}},
	  "model_detail_list":[{"model_name":"legacy__dev","max_tokens":8192}]}]}`

	var parsed catalogResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	wireModels, metadata := catalogEntries(parsed.ConfigInfoList)
	if got := wireModels["glm-5.3"].WireName; got != "glm-5.3__dev" {
		t.Fatalf("wire = %q, want glm-5.3__dev", got)
	}
	model := metadata["glm-5.3"]
	if model.Credits != "0.94" {
		t.Fatalf("camelCase 外层倍率 = %q, want 0.94", model.Credits)
	}
	if !model.SupportsTools {
		t.Fatal("extraConfig 的 native_function_call 应为 true")
	}
	if model.ContextWindow != 200000 || model.MaxTokens != 32000 {
		t.Fatalf("上下文/输出 = %d/%d", model.ContextWindow, model.MaxTokens)
	}
	if !model.IsReasoning {
		t.Fatal("reasoning_model 应标记为推理模型")
	}

	legacy := metadata["legacy"]
	if legacy.Credits != "0" {
		t.Fatalf("snake_case 外层 rate=0 应保留，实际 %q", legacy.Credits)
	}
	if legacy.SupportsTools {
		t.Fatal("native_function_call=false 不应声明工具能力")
	}
}
