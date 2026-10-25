package admin

import (
	"context"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
)

// fakeVendor 是最小可注册供应商：模型层只用到身份与区域。
type fakeVendor struct {
	core.Meta
}

func (v *fakeVendor) Match(string, string, map[string]any) bool { return false }
func (v *fakeVendor) Parse([]byte, string) (*core.Credential, error) {
	return &core.Credential{}, nil
}
func (v *fakeVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) { return nil, nil }
func (v *fakeVendor) Execute(context.Context, *core.Credential, *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *fakeVendor) ExecuteStream(context.Context, *core.Credential, *core.ExecuteRequest, core.StreamSink) error {
	return nil
}

// ratedVendor 在 fakeVendor 之上补一份计费倍率，用于验证倍率随清单落盘。
type ratedVendor struct {
	fakeVendor
	credits map[string]string
}

// ModelCredits 跟真实供应商一样在本层完成归一化：admin 不再二次格式化，
// 否则「0」这类合法倍率会被反复改写。
func (v *ratedVendor) ModelCredits() map[string]string {
	out := make(map[string]string, len(v.credits))
	for id, raw := range v.credits {
		if rate := core.FormatRate(raw); rate != "" {
			out[id] = rate
		}
	}
	return out
}

func newTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	registry := core.NewRegistry()
	for _, vendor := range []core.Vendor{
		&fakeVendor{Meta: core.Meta{VendorID: "workbuddycn", VendorName: "WB CN", VendorRegion: "cn"}},
		&fakeVendor{Meta: core.Meta{VendorID: "workbuddyglobal", VendorName: "WB Global", VendorRegion: "global"}},
	} {
		if err := registry.Register(vendor); err != nil {
			t.Fatal(err)
		}
	}
	return NewCatalog(store, registry)
}

func TestExpandKeysByVendorAndRegion(t *testing.T) {
	manager := newTestCatalog(t)
	if got := manager.ExpandKeys("glm-5.2"); len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("bare key = %#v", got)
	}
	if got := manager.ExpandKeys("workbuddycn:glm-5.2"); len(got) != 1 || got[0] != "workbuddycn:glm-5.2" {
		t.Fatalf("vendor key = %#v", got)
	}
	keys := manager.ExpandKeys("cn:glm-5.2")
	if len(keys) == 0 {
		t.Fatal("region alias must expand to registered cn vendors")
	}
	for _, key := range keys {
		if key == "workbuddyglobal:glm-5.2" {
			t.Fatalf("cn alias leaked global key: %#v", keys)
		}
	}
}

func TestAliasUniquenessAndResolve(t *testing.T) {
	c := newTestCatalog(t)
	c.SetAlias("workbuddycn", "glm-5.2", "glm-max")
	if got := c.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.2" {
		t.Fatalf("resolve = %q", got)
	}
	c.SetAlias("workbuddycn", "glm-5.3", "glm-max")
	if got := c.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.3" {
		t.Fatalf("rebind resolve = %q", got)
	}
	if c.AliasFor("workbuddycn", "glm-5.2") != "" {
		t.Fatal("old alias should be replaced")
	}
}

// TestResolveOutboundAliasRoundTrip 验证别名正向设置与反向还原是对称的。
func TestResolveOutboundAliasRoundTrip(t *testing.T) {
	c := newTestCatalog(t)
	c.SetAlias("qodercn", "glm-5.3", "qoder-glm")
	if got := c.ResolveOutboundModel("qodercn", "qoder-glm"); got != "glm-5.3" {
		t.Fatalf("alias resolve = %q, want glm-5.3", got)
	}
	// 未设别名的官方 ID 原样返回。
	if got := c.ResolveOutboundModel("qodercn", "deepseek-flash"); got != "deepseek-flash" {
		t.Fatalf("passthrough = %q, want deepseek-flash", got)
	}
}

// TestStaleModelStateIsIgnoredNotDeleted 覆盖升级后的陈旧键语义：
//
// 供应商换了对外模型 ID（Qoder 从上游 key 改为规范化 slug），用户状态里仍留着
// 旧键别名与旧键禁用项。它们必须对当前模型失效（否则请求被翻回旧 ID 再撞上
// 禁用），但**不能被删除**——清单可能因为探测失败而暂时拿不到，删除会让
// 用户配置永久丢失。
func TestStaleModelStateIsIgnoredNotDeleted(t *testing.T) {
	c := newTestCatalog(t)
	c.SetAlias("qodercn", "dfmodel", "deepseek-flash")
	c.SetAlias("qodercn", "glm-5.3", "qoder-glm")
	c.SetDisabled([]string{"qodercn:dfmodel", "qodercn:glm-5.3"}, true)
	c.CacheStatic("qodercn", []pluginapi.ModelInfo{{ID: "deepseek-flash"}, {ID: "glm-5.3"}})

	// 旧 ID 不再注册：别名与禁用都不作用于请求。
	if got := c.ResolveOutboundModel("qodercn", "deepseek-flash"); got != "deepseek-flash" {
		t.Fatalf("stale alias must be ignored, resolve = %q", got)
	}
	if c.IsDisabled("qodercn", "deepseek-flash") {
		t.Fatal("stale disabled key must not disable the current model id")
	}

	// 仍然注册着的官方 ID 不受影响。
	if got := c.ResolveOutboundModel("qodercn", "qoder-glm"); got != "glm-5.3" {
		t.Fatalf("live alias resolve = %q, want glm-5.3", got)
	}
	if !c.IsDisabled("qodercn", "glm-5.3") {
		t.Fatal("live disabled key must still apply")
	}

	// 状态原样保留：清单恢复后旧配置能自动重新生效。
	if c.AliasFor("qodercn", "dfmodel") != "deepseek-flash" {
		t.Fatal("stale alias must be kept in state, only ignored at resolution")
	}
	for _, key := range c.store.Snapshot().DisabledModels {
		if key == "qodercn:dfmodel" {
			return
		}
	}
	t.Fatal("stale disabled key must be kept in state, only ignored at lookup")
}

// TestFallbackModelListDoesNotUnlockDisabledModels 覆盖「清单暂时拿不全」的风险：
//
// 某次上游清单拉取可能只拿到部分模型（探测失败、降级或上游抖动）。此时绝不能
// 放开用户禁用的模型，也不能丢掉禁用配置——一次抖动不能改变鉴权语义。
func TestFallbackModelListDoesNotUnlockDisabledModels(t *testing.T) {
	c := newTestCatalog(t)
	c.SetDisabled([]string{"qodercn:kimi-k3"}, true)

	// 兜底/不完整清单里没有 kimi-k3（不能据此判定它已消失）。
	c.CacheStatic("qodercn", []pluginapi.ModelInfo{{ID: "deepseek-flash"}})
	if c.IsDisabled("qodercn", "kimi-k3") {
		t.Fatal("a model missing from a partial list must not be treated as registered")
	}

	// 清单为空（全部探测失败）：再保守一层，禁用照旧生效，不放行。
	c.CacheStatic("qodercn", nil)
	if !c.IsDisabled("qodercn", "kimi-k3") {
		t.Fatal("with no model list at all, disabled models must stay disabled")
	}
}

func TestDisabledUnionConfigAndState(t *testing.T) {
	c := newTestCatalog(t)
	c.Configure(ModelSettings{EnabledRealms: []string{"cn", "global"}, DisabledModels: []string{"cn:glm-5.2"}})
	if !c.IsDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config disable must apply")
	}
	if c.IsDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("config disable must be region scoped")
	}
	c.SetDisabled([]string{"workbuddyglobal:glm-5.2"}, true)
	if !c.IsDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("state disable must apply")
	}
	// 页面启用不能放开配置声明的禁用项。
	c.SetDisabled([]string{"cn:glm-5.2"}, false)
	if !c.IsDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config-declared disable must survive page enable")
	}
}

func TestMergeSameNameModels(t *testing.T) {
	merged := MergeSameNameModels([]pluginapi.ModelInfo{
		{ID: "glm-5.2", ContextLength: 1000, SupportedInputModalities: []string{"text"}},
		{ID: "GLM-5.2", ContextLength: 2000, SupportedInputModalities: []string{"image"}},
	})
	if len(merged) != 1 {
		t.Fatalf("merged len = %d", len(merged))
	}
	if merged[0].ContextLength != 2000 {
		t.Fatalf("context = %d", merged[0].ContextLength)
	}
	if len(merged[0].SupportedInputModalities) != 2 {
		t.Fatalf("modalities = %#v", merged[0].SupportedInputModalities)
	}
}

// TestCacheProbedModelsKeepsPreviousOnEmpty 锁定「探测失败不清空缓存」。
//
// 真实事故：OpenCode ZEN 的 model.for_auth 在探测失败时按契约返回空清单
// （让宿主淘汰坏凭证），调用方若照样写缓存，一次瞬时 401 就会让整家供应商
// 从控制台消失。探测型写入必须走 CacheProbedModels。
func TestCacheProbedModelsKeepsPreviousOnEmpty(t *testing.T) {
	manager := newTestCatalog(t)
	manager.CacheProbedModels("workbuddycn", []pluginapi.ModelInfo{{ID: "glm-5.2"}, {ID: "glm-5.3"}})

	if wrote := manager.CacheProbedModels("workbuddycn", nil); wrote {
		t.Fatal("empty probe result must not be written")
	}
	if got := len(manager.Registered("workbuddycn")); got != 2 {
		t.Fatalf("cached models = %d, want the previous 2 kept", got)
	}

	// 非空结果照常覆盖。
	if wrote := manager.CacheProbedModels("workbuddycn", []pluginapi.ModelInfo{{ID: "glm-5.4"}}); !wrote {
		t.Fatal("non-empty probe result must be written")
	}
	models := manager.Registered("workbuddycn")
	if len(models) != 1 || models[0].ID != "glm-5.4" {
		t.Fatalf("cache = %+v, want glm-5.4", models)
	}
}

// TestCacheProbedModelsPersistsOnlyOnChange 验证清单落在 state.json 里，
// 且内容未变化时不重复写盘（宿主每个凭证都会调一次 model.for_auth）。
func TestCacheProbedModelsPersistsOnlyOnChange(t *testing.T) {
	manager := newTestCatalog(t)
	manager.CacheProbedModels("workbuddycn", []pluginapi.ModelInfo{{ID: "glm-5.2"}})

	snapshot := manager.store.Snapshot()
	if len(snapshot.Models) != 1 || snapshot.Models[0].ID != "glm-5.2" {
		t.Fatalf("state models = %+v, want glm-5.2 persisted", snapshot.Models)
	}

	// 哨兵时间：内容没变就不该再落盘，它必须原样留着。
	manager.store.Mutate(func(s *state.State) { s.ModelsFetchedAt = "sentinel" })
	manager.CacheProbedModels("workbuddycn", []pluginapi.ModelInfo{{ID: "glm-5.2"}})
	if got := manager.store.Snapshot().ModelsFetchedAt; got != "sentinel" {
		t.Fatalf("identical list must not be re-persisted (fetched_at = %q)", got)
	}

	manager.CacheProbedModels("workbuddycn", []pluginapi.ModelInfo{{ID: "glm-5.3"}})
	updated := manager.store.Snapshot()
	if len(updated.Models) != 1 || updated.Models[0].ID != "glm-5.3" {
		t.Fatalf("state models = %+v, want glm-5.3 after a changed probe", updated.Models)
	}
	if updated.ModelsFetchedAt == "sentinel" {
		t.Fatal("a changed list must be persisted")
	}
}

// TestCreditsFollowProbeAndSurviveRestart 锁定「费用倍率」的两条不变量：
//
//  1. 倍率与模型清单是同一次探测的产物，必须一起落盘；
//  2. 插件重启后（新的 Catalog + LoadCachedState）倍率立刻可用，
//     不必等一轮上游探测——否则控制台会在启动后显示一排「—」。
func TestCreditsFollowProbeAndSurviveRestart(t *testing.T) {
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, errLoad := store.Load(); errLoad != nil {
		t.Fatal(errLoad)
	}
	registry := core.NewRegistry()
	if errRegister := registry.Register(&ratedVendor{
		fakeVendor: fakeVendor{Meta: core.Meta{VendorID: "qodercn", VendorName: "Qoder CN", VendorRegion: "cn"}},
		credits:    map[string]string{"glm-5.3": "x0.42"},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	manager := NewCatalog(store, registry)
	manager.CacheProbedModels("qodercn", []pluginapi.ModelInfo{{ID: "glm-5.3"}})

	snapshot := store.Snapshot()
	if len(snapshot.Models) != 1 || snapshot.Models[0].Credits != "0.42X" {
		t.Fatalf("state models = %+v, want credits persisted with the model", snapshot.Models)
	}

	restored := NewCatalog(store, registry)
	restored.LoadCachedState(snapshot.Models)
	if got := restored.CreditsFor("qodercn", "glm-5.3"); got != "0.42X" {
		t.Fatalf("credits after restart = %q, want 0.42X", got)
	}
	// 未知模型不能凭空编出倍率。
	if got := restored.CreditsFor("qodercn", "unknown-model"); got != "" {
		t.Fatalf("unknown model credits = %q, want empty", got)
	}
}

// TestCreditsChangeTriggersPersist 验证倍率变化本身也会落盘。
//
// 模型清单不变、只有倍率变了（上游调价）时，persistProbed 若只比较 ID/名称
// 就会静默丢掉这次更新。
func TestCreditsChangeTriggersPersist(t *testing.T) {
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, errLoad := store.Load(); errLoad != nil {
		t.Fatal(errLoad)
	}
	vendor := &ratedVendor{
		fakeVendor: fakeVendor{Meta: core.Meta{VendorID: "qodercn", VendorName: "Qoder CN", VendorRegion: "cn"}},
		credits:    map[string]string{"glm-5.3": "x0.42"},
	}
	registry := core.NewRegistry()
	if errRegister := registry.Register(vendor); errRegister != nil {
		t.Fatal(errRegister)
	}
	manager := NewCatalog(store, registry)
	manager.CacheProbedModels("qodercn", []pluginapi.ModelInfo{{ID: "glm-5.3"}})

	vendor.credits = map[string]string{"glm-5.3": "x0.80"}
	manager.CacheProbedModels("qodercn", []pluginapi.ModelInfo{{ID: "glm-5.3"}})
	if got := store.Snapshot().Models[0].Credits; got != "0.80X" {
		t.Fatalf("persisted credits = %q, want 0.80X after repricing", got)
	}
}
