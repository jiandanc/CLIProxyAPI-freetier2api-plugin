package autoclaw

// 本文件锁定国际版模型目录（autoclaw-model-config）的响应形态解析。
//
// 上游实测返回的是**顶层 models 数组**（没有 code/msg/data 信封）：
//
//	{"models":[{"id":"zai_auto",...,"input":["image","text"]}, ...]}
//
// 旧实现只认 data.models：JSON 解析成功、模型却是 0 条，于是宿主把该凭证
// 当作「没有可用模型」淘汰，控制台模型列表整片为空，而日志里只有一行
// "probe returned 0 model(s)"。这里同时锁住两种形态与「空清单要报错」。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modelConfigTopLevelFixture 是上游实测响应的结构（数值按真实值）。
const modelConfigTopLevelFixture = `{"models":[
 {"id":"zai_auto","name":"Auto","reasoning":true,"input":["image","text"],
  "contextWindow":1048576,"maxTokens":131072,
  "tooltip":"Automatically picks the best model for your task."},
 {"id":"zai_auto-fast","name":"Auto Fast","reasoning":true,"input":["text"],
  "contextWindow":1048576,"maxTokens":131072},
 {"id":"zaicoding_glm-5.3","name":"GLM-5.3","reasoning":true,"input":["text"],
  "contextWindow":1048576,"maxTokens":307200,"tooltip":"GLM-5.3 coding"}]}`

// modelConfigEnvelopeFixture 是旧信封形态。
const modelConfigEnvelopeFixture = `{"code":0,"msg":"SUCCESS","data":{"models":[
 {"id":"legacy-model","name":"Legacy","contextWindow":200000,"maxTokens":8192}]}}`

// serveModels 起一个只服务 ModelsConfigPath 的桩，并断言带了鉴权头。
func serveModels(t *testing.T, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ModelsConfigPath {
			t.Errorf("path = %q, want %q", r.URL.Path, ModelsConfigPath)
		}
		if got := r.Header.Get("Authorization"); !strings.Contains(got, "tok-abc") {
			t.Errorf("Authorization = %q, want token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestFetchModelsParsesTopLevelModels(t *testing.T) {
	useLoopbackHost(t)
	baseURL := serveModels(t, modelConfigTopLevelFixture)

	models, errFetch := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errFetch != nil {
		t.Fatalf("fetch models: %v", errFetch)
	}
	if len(models) != 3 {
		t.Fatalf("models = %d, want 3（顶层 models 未被识别）", len(models))
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	auto, okAuto := byID["zai_auto"]
	if !okAuto {
		t.Fatalf("missing zai_auto, got %v", byID)
	}
	if auto.Name != "Auto" || auto.ContextWindow != 1048576 || auto.MaxOutputTokens != 131072 || !auto.IsReasoning {
		t.Fatalf("zai_auto metadata = %+v", auto)
	}
	if !auto.SupportsImages || !auto.SupportsTools {
		t.Fatalf("zai_auto capabilities = %+v, want images+tools", auto)
	}
	if glm := byID["zaicoding_glm-5.3"]; glm.SupportsImages {
		t.Fatalf("zaicoding_glm-5.3 (input=text) must not claim image support")
	}
}

func TestFetchModelsStillParsesEnvelope(t *testing.T) {
	useLoopbackHost(t)
	baseURL := serveModels(t, modelConfigEnvelopeFixture)

	models, errFetch := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errFetch != nil {
		t.Fatalf("fetch models: %v", errFetch)
	}
	if len(models) != 1 || models[0].ID != "legacy-model" {
		t.Fatalf("models = %+v, want legacy-model", models)
	}
}

func TestFetchModelsErrorsOnUnknownShape(t *testing.T) {
	useLoopbackHost(t)
	// 解析成功但没有任何模型：必须报错，而不是静默返回空清单。
	baseURL := serveModels(t, `{"code":0,"msg":"SUCCESS","data":{}}`)

	models, errFetch := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errFetch == nil {
		t.Fatalf("expected error for empty model list, got %+v", models)
	}
	if !strings.Contains(errFetch.Error(), "0 model") {
		t.Fatalf("error = %v, want it to mention 0 model", errFetch)
	}
}

// TestFetchModelsParsesCreditLevel 锁定计费档位的透传。
//
// 上游 creditConsumptionLevel 是文字档位（低/中/高），不是折扣倍数；原样
// 透传到倍率列，整数则标成「档位 N」，缺失则留空（控制台显示「—」，不能
// 写成免费，也不能把 3 格式化成 3.00X）。
func TestFetchModelsParsesCreditLevel(t *testing.T) {
	baseURL := serveModels(t, `{"models":[
	 {"id":"zai_auto","creditConsumptionLevel":"中"},
	 {"id":"zai_auto-fast","creditConsumptionLevel":"低"},
	 {"id":"numeric","creditConsumptionLevel":3},
	 {"id":"none"}]}`)

	models, errModels := FetchModels(context.Background(), baseURL, "tok-abc", RegionGlobal)
	if errModels != nil {
		t.Fatalf("fetch: %v", errModels)
	}
	want := map[string]string{"zai_auto": "中", "zai_auto-fast": "低", "numeric": "档位 3", "none": ""}
	for _, model := range models {
		if model.Credits != want[model.ID] {
			t.Fatalf("%s: Credits = %q, want %q", model.ID, model.Credits, want[model.ID])
		}
	}

	// 国内静态表没有上游档位，必须留空而不是把「没给」写成 0/免费。
	for _, model := range staticCNModels() {
		if model.Credits != "" {
			t.Fatalf("静态模型 %s 不应带倍率: %q", model.ID, model.Credits)
		}
	}
}

// TestLevelTextToleratesShapes 守住未知形态：宁可留空，也不把结构塞进倍率列。
func TestLevelTextToleratesShapes(t *testing.T) {
	cases := map[string]string{
		`"高"`:           "高",
		`3`:             "档位 3", // 整数是档位编号，不能被格式化成伪倍率
		`null`:          "",
		``:              "",
		`{"level":"低"}`: "",
	}
	for raw, want := range cases {
		if got := levelText(json.RawMessage(raw)); got != want {
			t.Fatalf("levelText(%s) = %q, want %q", raw, got, want)
		}
	}
}

// TestAdapterModelCredits 锁定倍率表与清单同源。
func TestAdapterModelCredits(t *testing.T) {
	v := &adapter{}
	v.modelsToPluginAPI([]Model{
		{ID: "zai_auto", Credits: "中"},
		{ID: "tier_3", Credits: "档位 3"},
		{ID: "plain"},
	})
	credits := v.ModelCredits()
	if got := credits["zai_auto"]; got != "中" {
		t.Fatalf("倍率 = %q, want 中", got)
	}
	if got := credits["tier_3"]; got != "档位 3" {
		t.Fatalf("整数档位 = %q, want 档位 3（不能变成 3.00X）", got)
	}
	if _, exists := credits["plain"]; exists {
		t.Fatal("没有档位的模型不应进表")
	}
}
