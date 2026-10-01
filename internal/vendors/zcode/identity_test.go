package zcode

// 设备档案、凭证解析与 Plan 通道请求体变换的测试。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewDeviceProfileIsCoherent(t *testing.T) {
	// 档案必须是「成套」组合，不能是字段笛卡尔积拼出的假电脑。
	validOS := map[string]map[string]bool{
		"darwin": {"22.6.0": true, "23.6.0": true, "24.5.0": true, "24.6.0": true, "25.5.0": true},
		"win32":  {"10.0.19045": true, "10.0.22000": true, "10.0.22621": true, "10.0.22631": true, "10.0.26100": true, "10.0.26200": true},
	}
	validLocale := map[string]bool{
		"zh-CN|Asia/Shanghai": true, "en-US|America/New_York": true, "en-US|America/Los_Angeles": true,
		"en-GB|Europe/London": true, "de-DE|Europe/Berlin": true, "ja-JP|Asia/Tokyo": true,
		"ko-KR|Asia/Seoul": true, "en-SG|Asia/Singapore": true,
	}

	for i := 0; i < 200; i++ {
		profile := NewDeviceProfile()
		if validOS[profile.Platform] == nil {
			t.Fatalf("平台不应为 %q（官方桌面端主形态是 darwin/win32）", profile.Platform)
		}
		if !validOS[profile.Platform][profile.OSVersion] {
			t.Fatalf("os_version %q 与平台 %q 不匹配", profile.OSVersion, profile.Platform)
		}
		if !validLocale[profile.Language+"|"+profile.Timezone] {
			t.Fatalf("语言/时区组合不真实: %s/%s", profile.Language, profile.Timezone)
		}
		if len(profile.DeviceMID) != 36 {
			t.Fatalf("device_mid 应为 UUID: %q", profile.DeviceMID)
		}
		if profile.Platform == "win32" && profile.Arch != "x64" {
			t.Fatalf("win32 不应配 %q", profile.Arch)
		}
	}
}

func TestDeviceProfileIdentityHeaders(t *testing.T) {
	profile := DeviceProfile{
		Platform: "win32", Arch: "x64", OSVersion: "10.0.22631",
		Language: "zh-CN", Timezone: "Asia/Shanghai",
		Screen: "1920x1080", DeviceMID: "11111111-2222-4333-8444-555555555555",
	}
	headers := map[string]string{}
	BuildIdentityHeaders(profile).Apply(headers)

	checks := map[string]string{
		"X-Platform":          "win32-x64",
		"X-Os-Category":       "windows",
		"X-Os-Version":        "10.0.22631",
		"X-Client-Language":   "zh-CN",
		"X-Client-Timezone":   "Asia/Shanghai",
		"X-Device-Mid":        "11111111-2222-4333-8444-555555555555",
		"X-ZCode-App-Version": ClientAppVersion,
		"X-Title":             ClientTitle,
	}
	for key, want := range checks {
		if headers[key] != want {
			t.Fatalf("%s = %q, want %q", key, headers[key], want)
		}
	}
}

func TestTraceHeadersOmitsForbiddenKeys(t *testing.T) {
	// Plan 通道（start-plan）只允许三个 trace 头；带上 x-query-id / x-session-id
	// 会触发上游 3012 风控。
	headers := BuildTraceHeaders()
	for _, forbidden := range []string{"x-query-id", "x-session-id"} {
		if _, exists := headers[forbidden]; exists {
			t.Fatalf("Plan 通道不得发送 %s（会触发上游 3012 风控）", forbidden)
		}
	}
	for _, required := range []string{"x-request-id", "x-zcode-session-type", "x-zcode-trace-id"} {
		if headers[required] == "" {
			t.Fatalf("缺少必需追踪头 %s", required)
		}
	}
	if headers["x-zcode-session-type"] != "main" {
		t.Fatalf("x-zcode-session-type = %q", headers["x-zcode-session-type"])
	}
}

func TestParseCredentialRoundTrip(t *testing.T) {
	original := &Credential{
		APIKey:   "abc12345.secret99",
		JWTToken: "header.payload.signature",
		Email:    "User@Example.com",
		UserID:   "u-1",
		AuthMode: "oauth",
		Label:    "me",
		Profile:  NewDeviceProfile(),
	}
	raw, errMarshal := original.StorageJSON()
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}

	parsed, errParse := ParseCredential(raw)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if parsed.APIKey != original.APIKey || parsed.JWTToken != original.JWTToken {
		t.Fatalf("凭证字段未往返: %+v", parsed)
	}
	if parsed.Email != "user@example.com" {
		t.Fatalf("邮箱应归一为小写: %q", parsed.Email)
	}
	// device_mid 必须持久化：变了上游即视为新设备。
	if parsed.Profile.DeviceMID != original.Profile.DeviceMID {
		t.Fatalf("device_mid 未往返: %q != %q", parsed.Profile.DeviceMID, original.Profile.DeviceMID)
	}
	if parsed.Profile.Platform != original.Profile.Platform {
		t.Fatalf("档案平台未往返: %q", parsed.Profile.Platform)
	}
}

func TestParseCredentialRequiresSomeToken(t *testing.T) {
	if _, errParse := ParseCredential([]byte(`{"vendor":"zcode"}`)); errParse == nil {
		t.Fatal("没有任何凭证时必须报错")
	}
}

func TestOutboundTokenPrefersAPIKey(t *testing.T) {
	// 对话只走 API Key 通道（Plan 通道需人机验证码），因此出站凭证必须优先它。
	cred := &Credential{APIKey: "key.s", JWTToken: "a.b.c"}
	if got := cred.OutboundToken(); got != "key.s" {
		t.Fatalf("OutboundToken = %q, want the API key", got)
	}
	// 只有 JWT 时回退（此时候选路径本身会拒绝，见 resolveTargetURL）。
	if got := (&Credential{JWTToken: "a.b.c"}).OutboundToken(); got != "a.b.c" {
		t.Fatalf("OutboundToken = %q, want the JWT", got)
	}
}

func TestResolveTargetURLRequiresAPIKey(t *testing.T) {
	// 有 API Key 时打到 api.z.ai 的回退端点。
	target, errResolve := resolveTargetURL("", &Credential{APIKey: "k.s"})
	if errResolve != nil {
		t.Fatalf("unexpected error: %v", errResolve)
	}
	if target != ZAIOrigin+PathAPIMessages {
		t.Fatalf("target = %q", target)
	}

	// 只有 JWT 时必须明确拒绝，而不是发出必然 3007 的请求。
	if _, errResolve := resolveTargetURL("", &Credential{JWTToken: "a.b.c"}); errResolve == nil {
		t.Fatal("缺少 API Key 时必须拒绝，而不是走需要验证码的 Plan 通道")
	}
}

func TestLooksLikeCredentialRejectsForeign(t *testing.T) {
	// 必须偏严：误吞别家凭证会让宿主用本插件的结构覆盖对方账号。
	cases := []struct {
		raw      map[string]any
		fileName string
		want     bool
	}{
		{map[string]any{"vendor": "qodercn", "api_key": "a.b"}, "zcode-x.json", false},
		{map[string]any{"vendor": "zcode"}, "any.json", true},
		{map[string]any{"api_key": "a.b"}, "zcode-x.json", true},
		// 裸凭证无任何 ZCode 痕迹：不认领。
		{map[string]any{"api_key": "sk-abcdef"}, "random.json", false},
	}
	for _, tc := range cases {
		if got := LooksLikeCredential(tc.raw, tc.fileName, ""); got != tc.want {
			t.Fatalf("LooksLikeCredential(%v, %s) = %v, want %v", tc.raw, tc.fileName, got, tc.want)
		}
	}
}

func TestUserIDFromJWT(t *testing.T) {
	// {"user_id":"u-42"} 的 base64url（无填充）。
	token := "header." + "eyJ1c2VyX2lkIjoidS00MiJ9" + ".signature"
	if got := UserIDFromJWT(token); got != "u-42" {
		t.Fatalf("UserIDFromJWT = %q, want u-42", got)
	}
	if got := UserIDFromJWT("not-a-jwt"); got != "" {
		t.Fatalf("非法 JWT 应返回空串，得到 %q", got)
	}
}

func TestApplyStartPlanSystemIsIdempotent(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"system":   "user instructions",
	}
	applyStartPlanSystem(body, "GLM-5.3")
	first, _ := body["system"].([]any)
	if len(first) == 0 {
		t.Fatal("身份块未注入")
	}

	// 重复调用不得重复拼接（上游按内容审查，重复块会被判异常）。
	applyStartPlanSystem(body, "GLM-5.3")
	second, _ := body["system"].([]any)
	if len(second) != len(first) {
		t.Fatalf("重复注入改变了块数: %d != %d", len(second), len(first))
	}

	// 官方块在最前，用户原有 system 保留在后。
	head, _ := second[0].(map[string]any)
	if !strings.Contains(head["text"].(string), "ZCode") {
		t.Fatalf("首块应为官方身份块: %v", head)
	}
	tail, _ := second[len(second)-1].(map[string]any)
	if tail["text"] != "user instructions" {
		t.Fatalf("用户 system 应保留: %v", tail)
	}
}

func TestApplyCacheControl(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "first"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "reply"}}},
		},
	}
	applyCacheControl(body)

	messages := body["messages"].([]any)
	// 最后一条非 system 消息的末块应带 cache_control。
	last := messages[1].(map[string]any)
	blocks := last["content"].([]any)
	block := blocks[len(blocks)-1].(map[string]any)
	cc, _ := block["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" {
		t.Fatalf("cache_control 未追加: %v", block)
	}
	// 前一条不应被改动。
	first := messages[0].(map[string]any)
	if _, isString := first["content"].(string); !isString {
		t.Fatalf("非末条消息不应被改写: %v", first["content"])
	}
}

func TestParseConfigModels(t *testing.T) {
	body := []byte(`{"code":0,"data":{"builtinModels":[
		{"modelId":"GLM-5.3","name":"GLM-5.3","contextWindow":1000000,"maxCompletionTokens":128000,
		 "reasoning":{"levels":{"low":{},"high":{}}}},
		{"modelId":"GLM-5.3-Flash","name":"GLM-5.3-Flash","contextWindow":1000000,"maxCompletionTokens":128000,
		 "capabilities":{"vision":true}}
	]}}`)
	models := parseConfigModels(body)
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if models[0].ID != "GLM-5.3" || !models[0].IsReasoning {
		t.Fatalf("unexpected first model: %+v", models[0])
	}
	if !models[1].SupportsImages {
		t.Fatalf("GLM-5.3-Flash 应支持图片: %+v", models[1])
	}
	if models[0].ContextWindow != 1000000 || models[0].MaxTokens != 128000 {
		t.Fatalf("元数据未解析: %+v", models[0])
	}
}

func TestClassifyOrderingCaptchaBeforeAuth(t *testing.T) {
	// 403 + captcha 文案是验证码挑战，不是凭证失效——先判 403 会把账号错杀。
	risk := Classify(403, `{"code":3007,"msg":"captcha verify failed"}`)
	if risk.HTTPStatus() != 403 {
		t.Fatalf("验证码挑战应映射为软冷却(403)，得到 %d", risk.HTTPStatus())
	}

	// 纯 403 才是凭证失效。
	dead := Classify(403, `{"msg":"forbidden"}`)
	if dead.HTTPStatus() != 403 {
		t.Fatalf("凭证失效应映射为 403，得到 %d", dead.HTTPStatus())
	}
	if !strings.Contains(strings.ToLower(dead.Msg), "forbidden") && !strings.Contains(dead.Msg, "凭证") {
		t.Fatalf("unexpected message: %q", dead.Msg)
	}

	// 401 → 鉴权失败。
	if got := Classify(401, `{}`); got.HTTPStatus() != 401 {
		t.Fatalf("401 应映射为 401，得到 %d", got.HTTPStatus())
	}
}

func TestClassifyBusinessCodeWithoutStatus(t *testing.T) {
	// 上游以 HTTP 200 包装业务错误时，status=0 必须由 Kind 推导，
	// 不能把 200 当成错误的 HTTP 码透传给宿主。
	errQuota := Classify(0, `{"code":1005,"msg":"daily quota exhausted"}`)
	if errQuota.HTTPStatus() != 402 {
		t.Fatalf("额度耗尽应映射为 402，得到 %d", errQuota.HTTPStatus())
	}
}

func TestMergeStorageJSONPreservesUserKeys(t *testing.T) {
	original := []byte(`{"type":"freetier","vendor":"zcode","api_key":"old","disabled":true,"note":"mine"}`)
	updated := &Credential{APIKey: "new", JWTToken: "a.b.c", AuthMode: "oauth"}
	merged, errMerge := MergeStorageJSON(original, updated)
	if errMerge != nil {
		t.Fatalf("merge: %v", errMerge)
	}

	var out map[string]any
	if errUnmarshal := json.Unmarshal(merged, &out); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if out["api_key"] != "new" || out["jwt_token"] != "a.b.c" {
		t.Fatalf("凭证未更新: %v", out)
	}
	// 用户手工维护的字段必须保留，否则刷新一次就把配置抹了。
	if out["disabled"] != true || out["note"] != "mine" {
		t.Fatalf("用户字段丢失: %v", out)
	}
}
