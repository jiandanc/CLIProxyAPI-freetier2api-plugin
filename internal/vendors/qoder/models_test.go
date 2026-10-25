package qoder

import (
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestCanonicalModelID(t *testing.T) {
	cases := []struct {
		displayName string
		key         string
		want        string
	}{
		{"DeepSeek-V4-Pro", "dmodel", "deepseek-v4-pro"},
		{"DeepSeek-Flash", "dfmodel", "deepseek-flash"},
		{"Qwen3.8-Max", "qmodel_38max", "qwen3.8-max"},
		{"GLM-5.3-Flash", "gfmodel", "glm-5.3-flash"},
		{"Kimi-K2.8-Preview", "kmodel", "kimi-k2.8-preview"},
		{"DeepSeek V4.1  Flash", "dfmodel", "deepseek-v4.1-flash"},
		{" Sonus ", "smodel", "sonus"},
		{"Auto", "auto", "auto"},
		{"Performance", "performance", "performance"},
		// display_name 缺失时退回上游 key，保证总能拿到可用 ID。
		{"", "qfmodel", "qfmodel"},
		{"   ", "QModel_38Max", "qmodel_38max"},
	}
	for _, c := range cases {
		if got := canonicalModelID(c.displayName, c.key); got != c.want {
			t.Errorf("canonicalModelID(%q, %q) = %q; want %q", c.displayName, c.key, got, c.want)
		}
	}
}

// testModels 模拟一次上游 model/list 返回的清单（生产上只会来自上游）。
func testModels() []Model {
	return []Model{
		{Key: "auto", DisplayName: "Auto"},
		{Key: "performance", DisplayName: "Performance"},
		{Key: "qmodel_38max", DisplayName: "Qwen3.8-Max"},
		{Key: "qmodel_latest", DisplayName: "Qwen3.7-Max"},
		{Key: "kmodel_latest", DisplayName: "Kimi-K3"},
		{Key: "kmodel", DisplayName: "Kimi-K2.8-Preview"},
		{Key: "gmodel", DisplayName: "GLM-5.3"},
		{Key: "free", DisplayName: "Free-Model", PriceFactor: float64Ptr(0)},
		{Key: "gfmodel", DisplayName: "GLM-5.3-Flash"},
		{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro"},
		{Key: "dfmodel", DisplayName: "DeepSeek-Flash"},
		{Key: "mmodel", DisplayName: "MiniMax-M3"},
	}
}

func TestModelIndexReplaceAndLookup(t *testing.T) {
	var index modelIndex
	models := testModels()
	slugByKey := index.replace(models)

	if got := slugByKey["dmodel"]; got != "deepseek-v4-pro" {
		t.Fatalf("slug for dmodel = %q; want deepseek-v4-pro", got)
	}
	if len(slugByKey) != len(models) {
		t.Fatalf("slug count = %d; want %d", len(slugByKey), len(models))
	}

	cases := []struct{ name, want string }{
		// 对外 slug
		{"deepseek-v4-pro", "dmodel"},
		{"DeepSeek-V4-Pro", "dmodel"},
		// 旧上游 key 仍然可用（老客户端/旧脚本）
		{"dmodel", "dmodel"},
		{"DModel", "dmodel"},
		{"qmodel_38max", "qmodel_38max"},
		// 系统档位 slug 与 key 同名
		{"auto", "auto"},
		{"Performance", "performance"},
	}
	for _, c := range cases {
		got, ok := index.UpstreamKey(c.name)
		if !ok || got != c.want {
			t.Errorf("UpstreamKey(%q) = (%q, %v); want (%q, true)", c.name, got, ok, c.want)
		}
	}
	if _, ok := index.UpstreamKey("unknown-model"); ok {
		t.Error("unknown model must not resolve")
	}
	if _, ok := index.UpstreamKey("  "); ok {
		t.Error("blank name must not resolve")
	}
}

func TestModelIndexReplaceDropsStaleEntries(t *testing.T) {
	var index modelIndex
	index.replace([]Model{{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro"}})
	if _, ok := index.UpstreamKey("deepseek-v4-pro"); !ok {
		t.Fatal("first batch must be indexed")
	}

	// 上游换掉模型后，旧 slug 不应再命中。
	index.replace([]Model{{Key: "dfmodel", DisplayName: "DeepSeek-Flash"}})
	if _, ok := index.UpstreamKey("deepseek-v4-pro"); ok {
		t.Error("stale slug must be dropped after replace")
	}
	if key, ok := index.UpstreamKey("deepseek-flash"); !ok || key != "dfmodel" {
		t.Errorf("UpstreamKey(deepseek-flash) = (%q, %v); want (dfmodel, true)", key, ok)
	}
}

func TestModelIndexDuplicateSlugKeepsFirst(t *testing.T) {
	var index modelIndex
	slugByKey := index.replace([]Model{
		{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro"},
		{Key: "dfmodel", DisplayName: "DeepSeek-V4-Pro"},
	})
	if _, dup := slugByKey["dfmodel"]; dup {
		t.Error("duplicate display_name must not be registered a second time")
	}
	if key, _ := index.UpstreamKey("deepseek-v4-pro"); key != "dmodel" {
		t.Errorf("duplicate slug resolves to %q; want dmodel", key)
	}
}

func TestModelIndexSlugBeatsRawKey(t *testing.T) {
	var index modelIndex
	// 构造异常：某模型的 display_name 规范化后恰好等于另一模型的上游 key。
	// 清单里先声明的是 key="alpha" / display="Alpha"，因此 alpha 归它。
	index.replace([]Model{
		{Key: "alpha", DisplayName: "Alpha"},
		{Key: "beta", DisplayName: "alpha"},
	})
	if key, _ := index.UpstreamKey("alpha"); key != "alpha" {
		t.Errorf("UpstreamKey(alpha) = %q; want first declared entry (alpha)", key)
	}
}

// 国内版与国际版是两个 adapter 实例，索引互不干扰。
func TestModelIndexIsolatedPerInstance(t *testing.T) {
	var cn, global modelIndex
	cn.replace([]Model{{Key: "mmodel", DisplayName: "MiniMax-M2.7"}})
	global.replace([]Model{{Key: "mmodel", DisplayName: "MiniMax-M3"}})

	if key, _ := cn.UpstreamKey("minimax-m2.7"); key != "mmodel" {
		t.Errorf("cn minimax-m2.7 = %q; want mmodel", key)
	}
	if _, ok := cn.UpstreamKey("minimax-m3"); ok {
		t.Error("cn index must not contain the global model")
	}
	if key, _ := global.UpstreamKey("minimax-m3"); key != "mmodel" {
		t.Errorf("global minimax-m3 = %q; want mmodel", key)
	}
	if _, ok := global.UpstreamKey("minimax-m2.7"); ok {
		t.Error("global index must not contain the cn model")
	}
}

func TestModelsToPluginAPIUsesSlug(t *testing.T) {
	instance := newInstance(core.Env{}, RegionCN)
	adapterInstance, okAdapter := instance.(*adapter)
	if !okAdapter {
		t.Fatalf("newInstance returned %T; want *adapter", instance)
	}

	models := adapterInstance.modelsToPluginAPI(testModels())
	byID := make(map[string]bool, len(models))
	for _, model := range models {
		byID[model.ID] = true
		// 展示名仍是上游的 display_name（控制台/客户端看到的名字不变）。
		if model.ID == "deepseek-v4-pro" && model.DisplayName != "DeepSeek-V4-Pro" {
			t.Errorf("deepseek-v4-pro DisplayName = %q; want DeepSeek-V4-Pro", model.DisplayName)
		}
	}
	for _, want := range []string{"deepseek-v4-pro", "qwen3.8-max", "glm-5.3", "kimi-k3", "auto"} {
		if !byID[want] {
			t.Errorf("model id %q missing from registration", want)
		}
	}
	for _, unwanted := range []string{"dmodel", "qmodel_38max", "gmodel"} {
		if byID[unwanted] {
			t.Errorf("raw upstream key %q must not be registered as a model id", unwanted)
		}
	}

	// 注册时重建的索引可供出站解析。
	if key, _ := adapterInstance.models.UpstreamKey("deepseek-v4-pro"); key != "dmodel" {
		t.Errorf("index after registration = %q; want dmodel", key)
	}
}

// TestModelCreditsFromPriceFactor 锁定控制台的「费用倍率」数据源。
//
// 倍率只出现在上游 model/list 的 price_factor 上；宿主契约没有这个字段，
// 因此转换模型时必须顺手记下，否则控制台只能去解析 description 文本。
func TestModelCreditsFromPriceFactor(t *testing.T) {
	instance := newInstance(core.Env{}, RegionCN).(*adapter)
	models := []Model{
		{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro", PriceFactor: float64Ptr(0.42)},
		{Key: "gmodel", DisplayName: "GLM-5.3"},
		{Key: "free", DisplayName: "Free-Model", PriceFactor: float64Ptr(0)},
	}
	instance.modelsToPluginAPI(models)

	credits := instance.ModelCredits()
	if got := credits["deepseek-v4-pro"]; got != "0.42X" {
		t.Errorf("deepseek-v4-pro credits = %q; want 0.42X", got)
	}
	// 显式 0 是限免档，保留成 0.00X。
	if got := credits["free-model"]; got != "0.00X" {
		t.Errorf("显式 0 倍率 = %q; want 0.00X", got)
	}
	// price_factor 缺失表示上游没给倍率，不能显示成 x0.00。
	if _, ok := credits["glm-5.3"]; ok {
		t.Errorf("model without price_factor must have no credits entry, got %q", credits["glm-5.3"])
	}
	// 键必须是对外 slug，而不是上游 key（控制台按注册 ID 匹配）。
	if _, ok := credits["dmodel"]; ok {
		t.Error("credits must be keyed by the registered slug, not the upstream key")
	}
}

// float64Ptr 便于在测试里构造「上游显式给了倍率」的模型。
func float64Ptr(value float64) *float64 { return &value }
