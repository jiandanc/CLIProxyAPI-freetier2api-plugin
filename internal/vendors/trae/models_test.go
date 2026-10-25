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
