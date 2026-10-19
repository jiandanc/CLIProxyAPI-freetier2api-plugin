package tests

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// TestModelsForAuthIsolatesRealms 验证 realm 隔离的核心机制。
//
// cn 凭证只拿到 cn:* 模型，global 凭证只拿到 global:*。
// 宿主据此在选凭证阶段淘汰跨域凭证（conductor_selection.go 的
// authSupportsRouteModel → ClientSupportsModel）——这是双域隔离的落点。
func TestModelsForAuthIsolatesRealms(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.addAccount("2", "global-account", sampleCredentialJSON("global", "g1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if (strings.Contains(url, "codebuddy.cn") || strings.Contains(url, "copilot.tencent.com")) && strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"},{"id":"cn-exclusive","name":"CN Exclusive"}]}}`,
			}
		}
		if strings.Contains(url, "workbuddy.ai") && strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"},{"id":"global-exclusive","name":"Global Exclusive"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	// 测试 1：国内凭证拿到裸名 glm-5.2 与 cn-exclusive，绝不拿到 global-exclusive
	res1 := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})
	var resp1 pluginapi.ModelResponse
	_ = json.Unmarshal(res1, &resp1)
	var ids1 []string
	for _, m := range resp1.Models {
		ids1 = append(ids1, m.ID)
	}
	if !containsString(ids1, "glm-5.2") || !containsString(ids1, "cn-exclusive") {
		t.Fatalf("cn auth missing models: %v", ids1)
	}
	if containsString(ids1, "global-exclusive") {
		t.Fatalf("cn auth must NOT contain global-exclusive: %v", ids1)
	}

	// 测试 2：海外凭证拿到裸名 glm-5.2 与 global-exclusive，绝不拿到 cn-exclusive
	res2 := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "2"})
	var resp2 pluginapi.ModelResponse
	_ = json.Unmarshal(res2, &resp2)
	var ids2 []string
	for _, m := range resp2.Models {
		ids2 = append(ids2, m.ID)
	}
	if !containsString(ids2, "glm-5.2") || !containsString(ids2, "global-exclusive") {
		t.Fatalf("global auth missing models: %v", ids2)
	}
	if containsString(ids2, "cn-exclusive") {
		t.Fatalf("global auth must NOT contain cn-exclusive: %v", ids2)
	}

	// 测试 3：按域独立禁用：禁用 cn:glm-5.2 后，国内凭证失去 glm-5.2，海外凭证依然拥有 glm-5.2
	testApp.HandleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["cn:glm-5.2"],"disabled":true}`),
	})
	res1After := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})
	var resp1After pluginapi.ModelResponse
	_ = json.Unmarshal(res1After, &resp1After)
	var ids1After []string
	for _, m := range resp1After.Models {
		ids1After = append(ids1After, m.ID)
	}
	if containsString(ids1After, "glm-5.2") {
		t.Fatalf("cn auth must not contain disabled cn:glm-5.2, got: %v", ids1After)
	}

	res2After := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "2"})
	var resp2After pluginapi.ModelResponse
	_ = json.Unmarshal(res2After, &resp2After)
	var ids2After []string
	for _, m := range resp2After.Models {
		ids2After = append(ids2After, m.ID)
	}
	if !containsString(ids2After, "glm-5.2") {
		t.Fatalf("global auth should still contain glm-5.2 after disabling only cn: %v", ids2After)
	}
}

// TestStaticModelsBareAndMerged 验证静态目录为裸模型名且同名自动去重合并。
func TestStaticModelsBareAndMerged(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "workbuddycn-test.json", sampleCredentialJSON("cn", "test"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body: `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2",
				  "credits":"x0.05","maxInputTokens":1000000,"maxOutputTokens":131072,
				  "supportsImages":true,"supportsToolCall":true,
				  "reasoning":{"supportedEfforts":["high","max"],"defaultEffort":"high"}}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	result := callMethod(t, pluginabi.MethodModelStatic, pluginapi.StaticModelRequest{})
	var response pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode static models: %v", errUnmarshal)
	}
	countGLM := 0
	var glmModel pluginapi.ModelInfo
	for _, model := range response.Models {
		if model.ID == "glm-5.2" {
			countGLM++
			glmModel = model
		}
		if strings.HasPrefix(model.ID, "cn:") || strings.HasPrefix(model.ID, "global:") {
			t.Fatalf("model ID must be bare, got %q", model.ID)
		}
	}
	if countGLM != 1 {
		t.Fatalf("glm-5.2 must be merged into exactly 1 model entry, got %d", countGLM)
	}
	if glmModel.ContextLength != 1000000 {
		t.Fatalf("glm-5.2 context_length = %d, want 1000000", glmModel.ContextLength)
	}
	if glmModel.Thinking == nil || len(glmModel.Thinking.Levels) == 0 {
		t.Fatal("glm-5.2 must expose reasoning levels")
	}

	// 未添加凭证的供应商（如 opencodezen / cline / qoderglobal）的模型不应出现在模型清单中。
	for _, m := range response.Models {
		if strings.Contains(strings.ToLower(m.ID), "cline-free") ||
			strings.Contains(strings.ToLower(m.ID), "qmodel") {
			t.Fatalf("model %q from unconfigured vendor must not be present in static models", m.ID)
		}
	}
}

// TestPrepareBodyPromptModes 验证单 Pass 中提示词三种模式（passthrough/custom/append）及模型裸名改写。
func TestPrepareBodyPromptModes(t *testing.T) {
	baseJSON := []byte(`{"model":"cn:glm-5.2","messages":[{"role":"system","content":"old sys"},{"role":"user","content":"hello"}]}`)

	// 1. passthrough
	outPass := workbuddy.PrepareBody(baseJSON, workbuddy.PrepareOptions{
		Model:      "glm-5.2",
		PromptMode: "passthrough",
		PromptText: "custom prompt",
	})
	var mapPass map[string]any
	_ = json.Unmarshal(outPass, &mapPass)
	if mapPass["model"] != "glm-5.2" {
		t.Fatalf("model must be rewritten to bare name, got: %v", mapPass["model"])
	}
	msgsPass := mapPass["messages"].([]any)
	if msgsPass[0].(map[string]any)["content"] != "old sys" {
		t.Fatalf("passthrough should keep old sys, got: %v", msgsPass[0])
	}

	// 2. custom
	outCustom := workbuddy.PrepareBody(baseJSON, workbuddy.PrepareOptions{
		Model:      "glm-5.2",
		PromptMode: "custom",
		PromptText: "pure prompt",
	})
	var mapCustom map[string]any
	_ = json.Unmarshal(outCustom, &mapCustom)
	msgsCustom := mapCustom["messages"].([]any)
	if len(msgsCustom) != 2 || msgsCustom[0].(map[string]any)["content"] != "pure prompt" {
		t.Fatalf("custom should replace sys, got: %v", msgsCustom)
	}

	// 3. append
	outAppend := workbuddy.PrepareBody(baseJSON, workbuddy.PrepareOptions{
		Model:      "glm-5.2",
		PromptMode: "append",
		PromptText: "appended rule",
	})
	var mapAppend map[string]any
	_ = json.Unmarshal(outAppend, &mapAppend)
	msgsAppend := mapAppend["messages"].([]any)
	if len(msgsAppend) != 3 || msgsAppend[1].(map[string]any)["content"] != "appended rule" {
		t.Fatalf("append should insert prompt after leading sys, got: %v", msgsAppend)
	}
}

// TestDisabledModelsConfig 验证 YAML 配置 disabled_models 的解析格式与生效行为。
func TestDisabledModelsConfig(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	// 1. 块列表写法，含三种前缀形态
	yamlBlock := []byte(`
enabled_realms: cn,global
disabled_models:
  - workbuddycn:glm-5.2
  - cn:deepseek-v4-pro
  - qwen-3.5-plus
`)
	cfg, errDecode := testApp.DecodeConfig(yamlBlock)
	if errDecode != nil {
		t.Fatalf("decodeConfig failed: %v", errDecode)
	}
	if errApply := testApp.ApplyConfig(cfg); errApply != nil {
		t.Fatalf("applyConfig failed: %v", errApply)
	}

	// 厂商限定只影响该厂商
	if !testApp.IsModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("workbuddycn:glm-5.2 must be disabled")
	}
	if testApp.IsModelDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("workbuddyglobal:glm-5.2 should NOT be disabled")
	}
	// 区域别名展开到该区域所有厂商
	if !testApp.IsModelDisabled("workbuddycn", "deepseek-v4-pro") {
		t.Fatal("workbuddycn:deepseek-v4-pro must be disabled by cn: prefix")
	}
	if testApp.IsModelDisabled("workbuddyglobal", "deepseek-v4-pro") {
		t.Fatal("workbuddyglobal:deepseek-v4-pro should NOT be disabled by cn: prefix")
	}
	// 裸名对所有厂商生效
	if !testApp.IsModelDisabled("workbuddycn", "qwen-3.5-plus") || !testApp.IsModelDisabled("workbuddyglobal", "qwen-3.5-plus") {
		t.Fatal("bare name must be disabled on all vendors")
	}

	// 2. 流式列表写法
	yamlFlow := []byte(`
enabled_realms: cn,global
disabled_models: [workbuddyglobal:glm-5.2]
`)
	cfgFlow, errDecodeFlow := testApp.DecodeConfig(yamlFlow)
	if errDecodeFlow != nil {
		t.Fatalf("decode flow config failed: %v", errDecodeFlow)
	}
	if errApply := testApp.ApplyConfig(cfgFlow); errApply != nil {
		t.Fatalf("applyConfig flow failed: %v", errApply)
	}
	if !testApp.IsModelDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("flow list workbuddyglobal:glm-5.2 must be disabled")
	}
	// 新配置整体替换：上一轮的 cn 限定不再生效
	if testApp.IsModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("workbuddycn:glm-5.2 should not be disabled after reconfigure")
	}
}

// TestConfigAndStateDisabledModelsUnion 验证配置声明与页面操作取并集：
// 页面「启用」不能放开配置里声明的禁用项。
func TestConfigAndStateDisabledModelsUnion(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)
	cfg.DisabledModels = []string{"cn:glm-5.2"}
	if errApply := testApp.ApplyConfig(cfg); errApply != nil {
		t.Fatalf("applyConfig failed: %v", errApply)
	}
	if !testApp.IsModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config-declared disable must take effect")
	}

	// 页面尝试启用同一模型：配置声明仍在，禁用不放开。
	testApp.HandleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["cn:glm-5.2"],"disabled":false}`),
	})
	if !testApp.IsModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("page-side enable must NOT lift a config-declared disable")
	}

	// 页面禁用另一个模型：两个来源并存，都被拦截。
	testApp.HandleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["workbuddycn:glm-5.3"],"disabled":true}`),
	})
	if !testApp.IsModelDisabled("workbuddycn", "glm-5.3") {
		t.Fatal("page-side disable must take effect")
	}
	if !testApp.IsModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config disable must survive the page operation")
	}
}

// TestModelAliasScopedAndRouting 验证别名的两条语义：
//   - 别名是**供应商内**生效的对外 ID（同供应商内唯一、换绑旧模型）；
//   - 出站时按供应商还原成官方 ID（跨供应商同名别名各自路由回自己的模型）。
func TestModelAliasScopedAndRouting(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// 给两家供应商各绑一个同名别名：宿主会合并成一个 ID 并在两家间调度。
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.2","alias":"glm-max"}`),
	})
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"qodercn:gmodel","alias":"glm-max"}`),
	})

	if got := testApp.ModelAliasFor("workbuddycn", "glm-5.2"); got != "glm-max" {
		t.Fatalf("workbuddycn alias = %q, want glm-max", got)
	}
	if got := testApp.ModelAliasFor("qodercn", "gmodel"); got != "glm-max" {
		t.Fatalf("qodercn alias = %q, want glm-max", got)
	}
	// 作用域是单个模型 ID：同供应商的其它模型不受影响。
	if got := testApp.ModelAliasFor("workbuddycn", "glm-5.3"); got != "" {
		t.Fatalf("unrelated model alias = %q, want empty", got)
	}

	// 出站：同一别名按请求供应商还原成各自的官方 ID。
	if got := testApp.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.2" {
		t.Fatalf("testApp.ResolveOutboundModel(workbuddycn) = %q, want glm-5.2", got)
	}
	if got := testApp.ResolveOutboundModel("qodercn", "glm-max"); got != "gmodel" {
		t.Fatalf("testApp.ResolveOutboundModel(qodercn) = %q, want gmodel", got)
	}
	// 未设别名的名字原样返回（Qoder 等直接以上游 key 注册时无需翻译）。
	if got := testApp.ResolveOutboundModel("qodercn", "gmodel"); got != "gmodel" {
		t.Fatalf("passthrough = %q, want gmodel", got)
	}

	// 同供应商内别名唯一：把别名换绑到另一个模型，旧绑定被清掉。
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.3","alias":"glm-max"}`),
	})
	if got := testApp.ModelAliasFor("workbuddycn", "glm-5.2"); got != "" {
		t.Fatalf("rebind must clear the old binding, got %q", got)
	}
	if got := testApp.ResolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.3" {
		t.Fatalf("after rebind resolveOutboundModel = %q, want glm-5.3", got)
	}

	// 清除别名（空串）恢复官方 ID。
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.3","alias":""}`),
	})
	if got := testApp.ModelAliasFor("workbuddycn", "glm-5.3"); got != "" {
		t.Fatalf("clear alias failed, got %q", got)
	}
}

// TestModelAliasSwapRegistersAliasID 验证别名被当作模型的对外 ID 注册。
func TestModelAliasSwapRegistersAliasID(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	in := []pluginapi.ModelInfo{
		{ID: "qmodel_38max", Name: "Qwen3.8-Max", DisplayName: "Qwen3.8-Max"},
		{ID: "gmodel", Name: "GLM-5.3", DisplayName: "GLM-5.3"},
	}
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"qodercn:qmodel_38max","alias":"qwen-max"}`),
	})

	out := testApp.ApplyModelAliases("qodercn", in)
	if out[0].ID != "qwen-max" || out[0].Name != "qwen-max" || out[0].DisplayName != "qwen-max" {
		t.Fatalf("aliased model must expose the alias as ID/Name: %+v", out[0])
	}
	// 未设别名的模型原样保留官方 ID。
	if out[1].ID != "gmodel" || out[1].Name != "GLM-5.3" {
		t.Fatalf("unaliased model must keep official ID: %+v", out[1])
	}
}

// TestRewritePayloadModel 验证出站请求体里的 model 被改写成官方 ID。
func TestRewritePayloadModel(t *testing.T) {
	out := app.RewritePayloadModel([]byte(`{"model":"gmodel","messages":[]}`), "gm51model")
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(out, &decoded); errUnmarshal != nil {
		t.Fatalf("decode rewritten payload: %v", errUnmarshal)
	}
	if decoded["model"] != "gm51model" {
		t.Fatalf("payload model = %v, want gm51model", decoded["model"])
	}

	// 非法 JSON 原样返回（不让本地解析失败中断一次对话）。
	bad := []byte(`not json`)
	if got := app.RewritePayloadModel(bad, "gmodel"); string(got) != string(bad) {
		t.Fatalf("invalid payload must pass through unchanged, got %q", string(got))
	}
}

// TestModelChangeMarksRestartPending 验证禁用/别名变更会置「待生效」标记，
// 且宿主重建模型注册表（applyModelCatalog）时自动清除它。
func TestModelChangeMarksRestartPending(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)

	if testApp.SnapshotState().RestartPending {
		t.Fatal("fresh state must not be restart-pending")
	}

	// 禁用模型：置位。
	testApp.HandleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["workbuddycn:glm-5.2"],"disabled":true}`),
	})
	if !testApp.SnapshotState().RestartPending {
		t.Fatal("toggle must mark restart-pending")
	}

	// 宿主重建注册表（register/reconfigure 路径）时清除。
	testApp.ApplyModelCatalog(cfg)
	if testApp.SnapshotState().RestartPending {
		t.Fatal("applyModelCatalog must clear restart-pending")
	}

	// 别名变更同样置位，且状态负载带上标记，页面据此常驻提示。
	testApp.HandleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.2","alias":"glm-max"}`),
	})
	if !testApp.SnapshotState().RestartPending {
		t.Fatal("alias change must mark restart-pending")
	}
	payload := testApp.BuildStatusPayload(pluginapi.ManagementRequest{})
	if payload["restart_pending"] != true {
		t.Fatalf("status payload restart_pending = %v, want true", payload["restart_pending"])
	}
}

// TestDefaultStateDirFallback 验证存在 ~/.cli-proxy-api 时优先用其子目录。
func TestDefaultStateDirFallback(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("FREETIER2API_PLUGIN_HOME", "")

	// 1. 无 ~/.cli-proxy-api → 回退 ~/.freetier2api-plugin
	dir1 := app.DefaultStateDir()
	want1 := filepath.Join(tempHome, ".freetier2api-plugin")
	if dir1 != want1 {
		t.Fatalf("defaultStateDir = %q, want %q", dir1, want1)
	}

	// 2. 即使存在 ~/.cli-proxy-api（CPA auth-dir），也不再进入该目录，保持隔离以防被当作凭证扫描
	cpaDir := filepath.Join(tempHome, ".cli-proxy-api")
	if errMkdir := os.MkdirAll(cpaDir, 0o755); errMkdir != nil {
		t.Fatalf("create temp cpa dir: %v", errMkdir)
	}
	dir2 := app.DefaultStateDir()
	want2 := filepath.Join(tempHome, ".freetier2api-plugin")
	if dir2 != want2 {
		t.Fatalf("defaultStateDir with cpa dir = %q, want %q", dir2, want2)
	}
}

// containsString 报告切片是否含某个值。
func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// keysOf 返回 map 的键（用于错误信息）。
func keysOf(source map[string]pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(source))
	for key := range source {
		out = append(out, key)
	}
	return out
}
