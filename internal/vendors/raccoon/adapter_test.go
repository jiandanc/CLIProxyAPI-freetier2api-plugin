package raccoon

import (
	"encoding/json"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestInstancesSingle(t *testing.T) {
	instances := Instances(core.Env{})
	if len(instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(instances))
	}
	if instances[0].ID() != VendorID || instances[0].Name() != VendorName {
		t.Fatalf("instance = %q / %q", instances[0].ID(), instances[0].Name())
	}
	if instances[0].Region() != "" {
		t.Fatalf("raccoon has no region concept, got %q", instances[0].Region())
	}
}

func TestParseCredential(t *testing.T) {
	cred, errParse := ParseCredential([]byte(`{"access_token":"eyJ.a.b","refresh_token":"r1","uid":"7591340","nickname":"小浣熊-1","expires_at":1791529021000}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cred.AccessTokenValue() != "eyJ.a.b" || cred.RefreshTokenValue() != "r1" {
		t.Fatalf("tokens = %+v", cred)
	}
	if cred.UIDValue() != "7591340" || cred.NicknameValue() != "小浣熊-1" {
		t.Fatalf("identity = %+v", cred)
	}
	if cred.ExpiresAtValue() != 1791529021 {
		t.Fatalf("expires_at should normalize ms→s, got %d", cred.ExpiresAtValue())
	}
	if _, errParse := ParseCredential([]byte(`{"nickname":"no token"}`)); errParse == nil {
		t.Fatal("expected error for credential without any token")
	}
}

func TestLooksLikeCredentialStrict(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		raw      string
		want     bool
	}{
		{"filename", "raccoon-u1.json", `{}`, true},
		{"filename pinyin", "xiaohuanxiong-u1.json", `{}`, true},
		{"filename family", "my-raccoon.json", `{}`, true},
		{"vendor field", "acc.json", `{"vendor":"raccoon"}`, true},
		{"snake token pair + uid", "acc.json", `{"access_token":"a","refresh_token":"r","uid":"1"}`, true},
		{"pair without uid is foreign", "acc.json", `{"access_token":"a","refresh_token":"r"}`, false},
		{"camel pair is foreign", "acc.json", `{"accessToken":"a","refreshToken":"r"}`, false},
		{"generic token is foreign", "acc.json", `{"token":"t"}`, false},
		{"foreign vendor is foreign", "acc.json", `{"vendor":"opencodezen"}`, false},
		{"empty is nothing", "acc.json", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			_ = json.Unmarshal([]byte(tc.raw), &payload)
			if got := LooksLikeCredential(payload, tc.fileName, ""); got != tc.want {
				t.Fatalf("LooksLikeCredential(%q, %q) = %v, want %v", tc.fileName, tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseCatalogShapes(t *testing.T) {
	// 数字与字符串两种 context_window 形态，params 嵌套优先级低于顶层。
	body := `{"code":0,"data":{"categories":[
		{"type":"a","models":[{"name":"m1","description":"d1","context_window":100000,"max_tokens":"50000"}]},
		{"type":"b","models":[{"name":"m2","description":"d2","params":{"context_window":20,"max_tokens":10}}]},
		{"name":"m1","description":"dup"},
		{"name":"","description":"drop"},
		{"name":"m3","visible":false,"description":"invisible"}
	]}}`
	models, errParse := parseCatalog([]byte(body))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want 2 (m1,m2)", models)
	}
	if models[0].ID != "m1" || models[0].ContextWindow != 100000 || models[0].MaxOutputTokens != 50000 {
		t.Fatalf("m1 = %+v", models[0])
	}
	if models[1].ID != "m2" || models[1].ContextWindow != 20 || models[1].MaxOutputTokens != 10 {
		t.Fatalf("m2 = %+v", models[1])
	}
}

func TestParseCatalogBillingMultiplier(t *testing.T) {
	// 上游把 billing_multiplier 发成数字、字符串、嵌 params 三种形态；
	// 0 是合法倍率（限免档），缺失才是「没提供」，负数属于异常数据。
	body := `{"code":0,"data":{"categories":[
		{"type":"a","models":[
			{"name":"num","billing_multiplier":0.8},
			{"name":"str","billing_multiplier":"x0.05"},
			{"name":"nested","params":{"billing_multiplier":"1.00"}},
			{"name":"zero","billing_multiplier":0},
			{"name":"neg","billing_multiplier":-1},
			{"name":"bool","billing_multiplier":true,"tags":["vision"]},
			{"name":"eff","billing_multiplier":1.0,"billing_effective_multiplier":0.5,"tags":["image-understanding"]},
			{"name":"textual","tags":["chat"]},
			{"name":"missing"}
		]}
	]}}`
	models, errParse := parseCatalog([]byte(body))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	// Model.Credits 保留上游原文（含 "0"），过滤只发生在倍率表边界。
	wantRaw := map[string]string{"num": "0.8", "str": "x0.05", "nested": "1.00", "zero": "0", "neg": "-1", "eff": "0.5"}
	for _, model := range models {
		if want, ok := wantRaw[model.ID]; ok {
			if model.Credits != want {
				t.Fatalf("%s.Credits = %q, want %q", model.ID, model.Credits, want)
			}
			continue
		}
		if model.Credits != "" {
			t.Fatalf("%s.Credits = %q, want empty", model.ID, model.Credits)
		}
	}
	// 非数字形态（true/对象）不是倍率，必须当「没提供」。
	for _, model := range models {
		if model.ID == "bool" && model.Credits != "" {
			t.Fatalf("bool 形态的倍率必须留空，实际 %q", model.Credits)
		}
	}
	// 实付倍率优先于基准倍率；图片能力由 tags 推断。
	images := map[string]bool{"bool": true, "eff": true, "textual": false}
	for _, model := range models {
		if want, ok := images[model.ID]; ok && model.SupportsImages != want {
			t.Fatalf("%s.SupportsImages = %t, want %t", model.ID, model.SupportsImages, want)
		}
	}

	v := &adapter{}
	if got := v.modelsToPluginAPI(models); len(got) != len(models) {
		t.Fatalf("plugin models = %d, want %d", len(got), len(models))
	}
	credits := v.ModelCredits()
	want := map[string]string{"num": "0.80X", "str": "0.05X", "nested": "1.00X", "zero": "0.00X", "eff": "0.50X"}
	if len(credits) != len(want) {
		t.Fatalf("credits = %v, want %v", credits, want)
	}
	for id, label := range want {
		if credits[id] != label {
			t.Fatalf("credits[%s] = %q, want %q", id, credits[id], label)
		}
	}
	for _, id := range []string{"neg", "missing", "bool", "textual"} {
		if _, ok := credits[id]; ok {
			t.Fatalf("credits should not contain %q: %v", id, credits)
		}
	}
	credits["num"] = "mutated"
	if v.ModelCredits()["num"] != "0.80X" {
		t.Fatal("ModelCredits must return a copy")
	}
}

func TestParseCatalogBusinessError(t *testing.T) {
	if _, err := parseCatalog([]byte(`{"code":14018,"message":"积分不足"}`)); err == nil {
		t.Fatal("expected business error")
	}
}

func TestConfigureBaseURLs(t *testing.T) {
	v := Instances(core.Env{})[0].(*adapter)
	v.Configure(core.Settings{}, map[string]string{
		"raccoon_llm_base_url":    "http://llm.test",
		"raccoon_auth_base_url":   "http://auth.test",
		"raccoon_points_base_url": "http://points.test",
		"raccoon_origin":          "http://origin.test",
	})
	if v.resolveLLMBase() != "http://llm.test" || v.resolveAuthBase() != "http://auth.test" ||
		v.resolvePointsBase() != "http://points.test" || v.resolveOrigin() != "http://origin.test" {
		t.Fatalf("base URLs = %q/%q/%q/%q", v.resolveLLMBase(), v.resolveAuthBase(), v.resolvePointsBase(), v.resolveOrigin())
	}
	v.Configure(core.Settings{}, map[string]string{})
	if v.resolveLLMBase() != DefaultLLMBase || v.resolveAuthBase() != DefaultAuthBase {
		t.Fatalf("fallback base URLs = %q/%q", v.resolveLLMBase(), v.resolveAuthBase())
	}
}

func TestParseCallbackCode(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		code string
		want bool
	}{
		{"official scheme", "office-raccoon://auth/callback?code=ac_1&state=s1", "ac_1", true},
		{"loopback callback rejected (authorize-page flow only)", "http://127.0.0.1:17867/auth/callback?code=ac_2&state=s2", "", false},
		{"evil host", "office-raccoon://evil.com/callback?code=x", "", false},
		{"wrong path", "http://127.0.0.1:1/other?code=x", "", false},
		{"garbage", "not a url ::", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, ok := ParseCallbackCode(tc.raw)
			if ok != tc.want || code != tc.code {
				t.Fatalf("ParseCallbackCode(%q) = %q/%v, want %q/%v", tc.raw, code, ok, tc.code, tc.want)
			}
		})
	}
}

func TestAuthorizeURL(t *testing.T) {
	got := AuthorizeURLWith("https://x.test", "办公小浣熊客户端", "st")
	if !strings.HasPrefix(got, "https://x.test/code/authorize?") || !strings.Contains(got, "state=st") {
		t.Fatalf("AuthorizeURLWith = %q", got)
	}
}

func TestClassifyByStatusAndText(t *testing.T) {
	if f := Classify(401, "denied"); f.Kind != core.FailureKindSessionDead {
		t.Fatalf("401 kind = %q", f.Kind)
	}
	if f := Classify(429, "slow"); f.Kind != core.FailureKindSoftRate {
		t.Fatalf("429 kind = %q", f.Kind)
	}
	if f := Classify(200, "积分不足"); f.Kind != core.FailureKindHardCredit {
		t.Fatalf("quota text kind = %q", f.Kind)
	}
	// "insufficient" 单独出现不判额度（避免 context window 误伤）。
	if f := Classify(400, "context window is insufficient for this model"); f.Kind != core.FailureKindClient {
		t.Fatalf("insufficient-without-quota-word kind = %q", f.Kind)
	}
	if f := ClassifyBusiness(200, 14018, "额度不足"); f.Kind != core.FailureKindHardCredit {
		t.Fatalf("14018 kind = %q", f.Kind)
	}
}
