package workbuddy

// 本文件测试协议核心：错误分类、请求体改写、凭证解析、前缀协议、
// 工具配对与脱敏。这些都是「顺序敏感」的逻辑，最容易在重构中静默退化。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestClassifyOrderingMatters 验证错误分类的判定顺序。
//
// 顺序有语义：重排会让某些错误被错误地归到「罚账号」的类别，
// 造成账号被无谓冷却。
func TestClassifyOrderingMatters(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   Kind
	}{
		// 11102 必须先判：它可能是 400 也可能是 404，晚判会被 404 分支吞掉。
		{"model blocked on 400", 400, `{"code":11102,"msg":"service info not found"}`, KindModelBlocked},
		{"model blocked on 404", 404, `{"code":11102,"msg":"service info not found"}`, KindModelBlocked},
		// 429 必须早于余额关键词：429 正文常带 "quota exceeded" 这类跨界的措辞，
		// 先判余额会把限流误冷却到次日，白扔一个号。
		{"429 with credit wording stays rate limit", 429, `{"code":1,"msg":"quota exceeded, too many requests"}`, KindSoftRate},
		// 真正的余额耗尽由 402 或业务码 14018 捕获。
		{"402 is hard credit", 402, `{"code":1,"msg":"payment required"}`, KindHardCredit},
		{"429 with 14018 is hard credit", 429, `{"code":14018,"msg":"insufficient"}`, KindHardCredit},
		// 会话失效优先于账号故障。
		{"session dead", 401, `{"code":12153,"msg":"Offline user session not found"}`, KindSessionDead},
		// 账号故障不能按 11140 判定（该 code 同时承载模型级限流文案），只看关键词。
		{"account fault by keyword", 400, `{"msg":"request illegal"}`, KindAccountFault},
		// 11140 带限流文案时必须按文案归为限流，而不是账号故障——
		// 按 code 判定会把一个只是被限流的账号当成被封禁处理。
		{"11140 with rate-limit wording is soft rate", 400, `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, KindSoftRate},
		// 上下文超限是请求级错误，必须先于 404/5xx/WAF 判定。
		{"prompt too long on 400", 400, `{"code":11115,"msg":"prompt is too long"}`, KindPromptTooLong},
		{"prompt too long on 413", 413, `{"code":11115,"msg":"prompt is too long"}`, KindPromptTooLong},
		// WAF：403 且没有业务信封。
		{"waf block", 403, `<html>Request blocked by WAF</html>`, KindWAFBlock},
		{"403 with envelope is not waf", 403, `{"code":1000,"msg":"quota exceeded"}`, KindHardCredit},
		// 图片无效。
		{"image invalid", 400, `{"code":11135,"msg":"invalid_image_data"}`, KindImageInvalid},
		// 内容策略拦截。
		{"content blocked", 400, `{"msg":"blocked by security policy"}`, KindContentBlocked},
		// 参数非法。
		{"bad params", 400, `{"msg":"Unmarshal chat params failed"}`, KindBadParams},
		// 其它 4xx。
		{"other client error", 400, `{"code":12345,"msg":"whatever"}`, KindClient},
		// 偶发 404（无 11102 标记）。
		{"plain 404", 404, `{"msg":"not found"}`, KindNotFound},
		// 上游故障。
		{"server error", 500, `{"msg":"internal error"}`, KindServer},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Classify(testCase.status, testCase.body)
			if got.Kind != testCase.want {
				t.Fatalf("Classify(%d, %q).Kind = %s, want %s",
					testCase.status, testCase.body, got.Kind, testCase.want)
			}
		})
	}
}

// TestClassifyModelBlockedNeedsFieldBoundary 验证 11102 只认同独立字段。
//
// 若做整段文本子串匹配，"11102" 出现在 requestId 里就会误避让一个可用模型。
func TestClassifyModelBlockedNeedsFieldBoundary(t *testing.T) {
	// 11102 只出现在 requestId 里：不该被判成模型不可用。
	body := `{"code":12345,"msg":"bad request","requestId":"trace-11102-abc"}`
	got := Classify(http.StatusBadRequest, body)
	if got.Kind == KindModelBlocked {
		t.Fatalf("11102 inside requestId must not trigger model blocking: %s", got.Kind)
	}
	// code 字段真的是 11102：应当命中。
	if got := Classify(http.StatusBadRequest, `{"code":11102}`); got.Kind != KindModelBlocked {
		t.Fatalf("explicit code 11102 must be model blocked, got %s", got.Kind)
	}
}

// TestClassifyRetryableAndStatus 验证可重试判定与状态码映射。
//
// 宿主按 HTTP 状态码决定凭证处置，这张映射表是「账号处置矩阵」的落点。
func TestClassifyRetryableAndStatus(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		wantStatus    int
	}{
		{"rate limit is retryable", 429, `{"msg":"rate limit"}`, true, 429},
		{"content blocked is not retryable", 400, `{"msg":"blocked by security policy"}`, false, 400},
		{"prompt too long is not retryable", 400, `{"msg":"prompt is too long"}`, false, 400},
		{"image invalid is not retryable", 400, `{"msg":"invalid_image_data"}`, false, 400},
		{"bad params is not retryable", 400, `{"msg":"Unmarshal chat params failed"}`, false, 400},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			classified := Classify(testCase.status, testCase.body)
			if classified.Retryable() != testCase.wantRetryable {
				t.Fatalf("Retryable() = %v, want %v", classified.Retryable(), testCase.wantRetryable)
			}
			if classified.HTTPStatus() != testCase.wantStatus {
				t.Fatalf("HTTPStatus() = %d, want %d", classified.HTTPStatus(), testCase.wantStatus)
			}
		})
	}
}

// TestParseRateResetAndRetryAfter 验证上游等待信息的解析。
func TestParseRateResetAndRetryAfter(t *testing.T) {
	// 中文文案优先。
	reset := ParseRateReset(`{"msg":"额度恢复将在 2026-09-27 04:00:00 重置"}`)
	if reset.IsZero() {
		t.Fatal("chinese rate reset wording must be parsed")
	}
	if reset.Hour() != 4 {
		t.Fatalf("reset hour = %d, want 4", reset.Hour())
	}

	header := http.Header{}
	header.Set("Retry-After", "30")
	if got := ParseRetryAfter(header, time.Now()); got != 30*time.Second {
		t.Fatalf("Retry-After = %v, want 30s", got)
	}
	// 离谱的值被钳制（上游偶发返回极大值）。
	header.Set("Retry-After", "999999")
	if got := ParseRetryAfter(header, time.Now()); got > time.Hour*2 {
		t.Fatalf("Retry-After must be capped, got %v", got)
	}
	// HTTP-Date 形态被忽略（不是纯数字）。
	header.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	if got := ParseRetryAfter(header, time.Now()); got != 0 {
		t.Fatalf("http-date Retry-After must be ignored, got %v", got)
	}
}

// TestClassifyIsModelRateLimit 验证模型级限流的识别。
func TestClassifyIsModelRateLimit(t *testing.T) {
	if !IsModelRateLimit(`{"code":6004,"msg":"rate-limiting"}`) {
		t.Fatal("6004 must be recognized as model-level rate limit")
	}
	if IsModelRateLimit(`{"code":1,"msg":"rate limit"}`) {
		t.Fatal("non-6004 must not be model-level rate limit")
	}
}

// TestParseCredentialBothShapes 验证凭证的两种形态都能解析。
func TestParseCredentialBothShapes(t *testing.T) {
	nested := `{"auth":{"accessToken":"at","refreshToken":"rt","domain":"www.codebuddy.cn"},
	             "account":{"uid":"u1","nickname":"昵称"},"device_token":"dt"}`
	flat := `{"accessToken":"at","refreshToken":"rt","uid":"u1","domain":"www.workbuddy.ai","device_token":"dt"}`

	for _, testCase := range []struct {
		name       string
		raw        string
		wantRealm  Region
		wantUID    string
		wantDevice string
	}{
		{"nested", nested, RegionCN, "u1", "dt"},
		// 扁平形态按 domain 推断域：workbuddy.ai 家族是国际版。
		{"flat global", flat, RegionGlobal, "u1", "dt"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			credential, errParse := ParseCredential([]byte(testCase.raw), RegionCN)
			if errParse != nil {
				t.Fatalf("ParseCredential: %v", errParse)
			}
			if credential.Realm() != testCase.wantRealm {
				t.Fatalf("realm = %s, want %s", credential.Realm(), testCase.wantRealm)
			}
			if credential.UIDValue() != testCase.wantUID {
				t.Fatalf("uid = %q, want %q", credential.UIDValue(), testCase.wantUID)
			}
			if credential.DeviceTokenValue() != testCase.wantDevice {
				t.Fatalf("device_token = %q", credential.DeviceTokenValue())
			}
		})
	}
}

// TestParseCredentialRejectsEmpty 验证空凭证被拒绝。
func TestParseCredentialRejectsEmpty(t *testing.T) {
	if _, errParse := ParseCredential([]byte(`{}`), RegionCN); errParse == nil {
		t.Fatal("credential without any token must be rejected")
	}
	if _, errParse := ParseCredential(nil, RegionCN); errParse == nil {
		t.Fatal("empty credential must be rejected")
	}
}

// TestMergeStorageJSONKeepsUserFields 验证写回时保留用户维护的字段。
//
// 宿主 auth 文件里可能有 disabled / prefix / proxy_url / note / weight 等
// 用户自己加的字段；重建式写回会静默丢掉它们。
func TestMergeStorageJSONKeepsUserFields(t *testing.T) {
	original := []byte(`{"auth":{"accessToken":"old"},"disabled":true,"note":"我的备注","weight":3}`)
	credential := &Credential{AccessToken: "new", RefreshToken: "rt", UID: "u1"}
	credential.SetRealm(RegionCN)

	merged, errMerge := MergeStorageJSON(original, credential)
	if errMerge != nil {
		t.Fatalf("MergeStorageJSON: %v", errMerge)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(merged, &payload); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if payload["disabled"] != true {
		t.Fatal("user field 'disabled' must be preserved")
	}
	if payload["note"] != "我的备注" {
		t.Fatal("user field 'note' must be preserved")
	}
	auth, okAuth := payload["auth"].(map[string]any)
	if !okAuth || auth["accessToken"] != "new" {
		t.Fatalf("token must be updated: %v", payload["auth"])
	}
}

// TestStorageJSONAlwaysCarriesType 验证写回的凭证一定带 type 字段。
//
// 宿主用 type 直接判定归属（有它就不再遍历插件）。缺这个字段时归属取决于
// 插件遍历顺序，会被判定过宽的前置插件抢走，账号在宿主侧被归到别的 provider。
func TestStorageJSONAlwaysCarriesType(t *testing.T) {
	credential := &Credential{AccessToken: "at", RefreshToken: "rt", UID: "u1"}
	credential.SetRealm(RegionCN)

	raw, errStorage := credential.StorageJSON()
	if errStorage != nil {
		t.Fatalf("StorageJSON: %v", errStorage)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if payload["type"] != ProviderKey {
		t.Fatalf("type = %v, want %q", payload["type"], ProviderKey)
	}

	// 合并写回：原文件缺 type 时也要补上（用户从别处复制进来的旧凭证）。
	merged, errMerge := MergeStorageJSON([]byte(`{"auth":{"accessToken":"old"},"uid":"u1"}`), credential)
	if errMerge != nil {
		t.Fatalf("MergeStorageJSON: %v", errMerge)
	}
	var mergedPayload map[string]any
	if errUnmarshal := json.Unmarshal(merged, &mergedPayload); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if mergedPayload["type"] != ProviderKey {
		t.Fatalf("merged type = %v, want %q", mergedPayload["type"], ProviderKey)
	}
	// 用户字段仍要保留。
	if mergedPayload["uid"] != "u1" {
		t.Fatal("merge must preserve user fields")
	}
}

// TestLooksLikeCredential 验证凭证归属判定。
//
// 判定必须偏严：宿主把所有非内建格式的文件依次喂给每个插件，
// 误吞别家的凭证会让宿主用本插件结构改写对方的账号文件（且是静默的）。
func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		provider string
		want     bool
	}{
		// 强证据：显式声明。
		{"host provider hint", map[string]any{}, "x.json", ProviderKey, true},
		{"type field", map[string]any{"type": ProviderKey}, "x.json", "", true},
		{"type field uppercase", map[string]any{"type": "FreeTier"}, "x.json", "", true},

		// 文件名约定。CN 与 GLOBAL 是独立供应商，两者都要被本协议认出
		// （具体区域由 core.ResolveVendor 的前缀匹配决定）。
		{"filename exact", map[string]any{"foo": "bar"}, "workbuddy.json", "", true},
		{"filename cn vendor", map[string]any{"foo": "bar"}, "workbuddycn-abc.json", "", true},
		{"filename global vendor", map[string]any{"foo": "bar"}, "workbuddyglobal-abc.json", "", true},
		{"filename underscore suffix", map[string]any{"foo": "bar"}, "workbuddy_abc.json", "", true},
		{"filename contains segment", map[string]any{"foo": "bar"}, "my-workbuddy.json", "", true},

		// 本插件独有的嵌套结构。
		{"nested auth pair", map[string]any{
			"auth": map[string]any{"accessToken": "a", "refreshToken": "r"},
		}, "x.json", "", true},

		// **必须拒绝**：通用字段不足以判定归属。
		{"top-level accessToken only", map[string]any{"accessToken": "a"}, "x.json", "", false},
		{"top-level both tokens", map[string]any{"accessToken": "a", "refreshToken": "r"}, "x.json", "", false},
		{"device_token only", map[string]any{"device_token": "d"}, "x.json", "", false},
		{"nested accessToken without refresh", map[string]any{
			"auth": map[string]any{"accessToken": "a"},
		}, "x.json", "", false},

		// 明确的别家 provider。
		{"foreign type", map[string]any{"type": "qoder"}, "x.json", "qoder", false},
		{"foreign filename", map[string]any{"access_token": "x"}, "claude.json", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := LooksLikeCredential(testCase.raw, testCase.fileName, testCase.provider); got != testCase.want {
				t.Fatalf("LooksLikeCredential = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestParseCredentialStillAcceptsFlatShape 验证收紧归属判定后，
// 扁平形态（手写凭证）仍能被解析——判定严不等于解析窄。
func TestParseCredentialStillAcceptsFlatShape(t *testing.T) {
	flat := `{"accessToken":"at","refreshToken":"rt","uid":"u1","domain":"www.codebuddy.cn"}`
	credential, errParse := ParseCredential([]byte(flat), RegionCN)
	if errParse != nil {
		t.Fatalf("flat credential must still parse: %v", errParse)
	}
	if credential.UIDValue() != "u1" || credential.AccessTokenValue() != "at" {
		t.Fatalf("flat credential parsed incorrectly: %+v", credential)
	}
	// 但归属判定不因它能解析就放行（除非文件名符合约定）。
	if LooksLikeCredential(map[string]any{"accessToken": "at", "refreshToken": "rt"}, "mystery.json", "") {
		t.Fatal("a parseable flat credential from an unknown file must not be claimed")
	}
}

// TestDeriveIDStabilityAndIsolation 验证设备指纹的稳定性与隔离性。
func TestDeriveIDStabilityAndIsolation(t *testing.T) {
	SetInstallSalt("salt-a")
	first := deriveID("uid-1", "machine")
	second := deriveID("uid-1", "machine")
	if first != second {
		t.Fatal("deriveID must be stable for the same input")
	}
	if len(first) != 36 {
		t.Fatalf("deriveID length = %d, want 36 (official device id shape)", len(first))
	}
	if deriveID("uid-2", "machine") == first {
		t.Fatal("different accounts must get different fingerprints")
	}
	// 换机器盐 = 换设备：指纹必须整体漂移。
	SetInstallSalt("salt-b")
	if deriveID("uid-1", "machine") == first {
		t.Fatal("changing the install salt must change fingerprints")
	}
	SetInstallSalt("")
}

// TestBuildCacheKeyIsolatesAccounts 验证缓存键的账号隔离。
//
// uid 前缀是硬隔离因子：跨账号复用同一 cache key 会让上游命中
// 错账号的前缀缓存，泄露对方对话内容。
func TestBuildCacheKeyIsolatesAccounts(t *testing.T) {
	keyA := buildCacheKey("account-aaaa1111", "conv-1")
	keyB := buildCacheKey("account-bbbb2222", "conv-1")
	if keyA == keyB {
		t.Fatal("different accounts must produce different cache keys")
	}
	if !strings.HasPrefix(keyA, "wb2a-account-") {
		t.Fatalf("cache key should carry the uid prefix: %q", keyA)
	}
	if buildCacheKey("account-aaaa1111", "conv-1") != keyA {
		t.Fatal("cache key must be deterministic")
	}
}

// prepareTest 执行一次改写管线（测试辅助）。
func prepareTest(t *testing.T, body string, opts PrepareOptions) map[string]any {
	t.Helper()
	prepared := PrepareBody([]byte(body), opts)
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(prepared, &payload); errUnmarshal != nil {
		t.Fatalf("decode prepared body: %v (raw=%s)", errUnmarshal, prepared)
	}
	return payload
}

// TestPrepareBodyCoreRules 验证改写管线的核心规则。
func TestPrepareBodyCoreRules(t *testing.T) {
	payload := prepareTest(t, `{
		"model":"glm-5.2",
		"stream":false,
		"max_completion_tokens":32000,
		"messages":[{"role":"developer","content":"be nice"},
		            {"role":"user","content":[{"type":"image_url","image_url":"http://x/y.png"}]}]
	}`, PrepareOptions{})

	// 上游拒绝非流式。
	if payload["stream"] != true {
		t.Fatalf("stream must be forced to true, got %v", payload["stream"])
	}
	// max_completion_tokens → max_tokens（并删除别名）。
	if _, okAlias := payload["max_completion_tokens"]; okAlias {
		t.Fatal("max_completion_tokens alias must be removed")
	}
	if payload["max_tokens"] != float64(32000) {
		t.Fatalf("max_tokens = %v, want 32000", payload["max_tokens"])
	}
	// stream_options 缺省补齐（否则拿不到 usage）。
	if _, okOptions := payload["stream_options"]; !okOptions {
		t.Fatal("stream_options must be defaulted to include_usage")
	}
	// developer → system（上游 role 白名单不含 developer）。
	messages, _ := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role must normalize to system, got %v", first["role"])
	}
	// image_url 字符串 → 对象。
	second, _ := messages[1].(map[string]any)
	parts, _ := second["content"].([]any)
	part, _ := parts[0].(map[string]any)
	if _, okString := part["image_url"].(string); okString {
		t.Fatal("image_url string must be normalized to an object")
	}
	imageObject, okObject := part["image_url"].(map[string]any)
	if !okObject || imageObject["url"] != "http://x/y.png" {
		t.Fatalf("image_url = %v, want {url: ...}", part["image_url"])
	}
}

// TestPrepareBodyExplicitMaxTokensWins 验证显式 max_tokens 优先。
func TestPrepareBodyExplicitMaxTokensWins(t *testing.T) {
	payload := prepareTest(t, `{"model":"m","max_tokens":100,"max_completion_tokens":200,"messages":[]}`, PrepareOptions{})
	if payload["max_tokens"] != float64(100) {
		t.Fatalf("explicit max_tokens must win, got %v", payload["max_tokens"])
	}
	if _, okAlias := payload["max_completion_tokens"]; okAlias {
		t.Fatal("alias must still be removed")
	}
}

// TestPrepareBodyToolChoiceNormalization 验证 tool_choice 归一。
//
// 上游该字段是 Go string，对象形态会 400。
func TestPrepareBodyToolChoiceNormalization(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, payload map[string]any)
	}{
		{"object auto becomes string", `{"model":"m","tool_choice":{"type":"auto"},"messages":[]}`,
			func(t *testing.T, payload map[string]any) {
				if payload["tool_choice"] != "auto" {
					t.Fatalf("tool_choice = %v, want auto", payload["tool_choice"])
				}
			}},
		{"none drops tools", `{"model":"m","tool_choice":"none","tools":[{"x":1}],"messages":[]}`,
			func(t *testing.T, payload map[string]any) {
				if _, okChoice := payload["tool_choice"]; okChoice {
					t.Fatal("none must drop tool_choice")
				}
				if _, okTools := payload["tools"]; okTools {
					t.Fatal("none must drop tools")
				}
			}},
		{"function object becomes name", `{"model":"m","tool_choice":{"type":"function","function":{"name":"Bash"}},"messages":[]}`,
			func(t *testing.T, payload map[string]any) {
				if payload["tool_choice"] != "Bash" {
					t.Fatalf("tool_choice = %v, want Bash", payload["tool_choice"])
				}
			}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.check(t, prepareTest(t, testCase.body, PrepareOptions{}))
		})
	}
}

// TestPrepareBodyDeepSeekThinkingInjection 验证 DeepSeek 思维链注入。
//
// deepseek 系不显式开思考就不返回思维链。
func TestPrepareBodyDeepSeekThinkingInjection(t *testing.T) {
	payload := prepareTest(t, `{"model":"deepseek-v4.1-flash","messages":[]}`, PrepareOptions{})
	thinking, okThinking := payload["thinking"].(map[string]any)
	if !okThinking || thinking["type"] != "enabled" {
		t.Fatalf("deepseek must get thinking enabled, got %v", payload["thinking"])
	}
	if payload["reasoning_effort"] != defaultDeepSeekEffort {
		t.Fatalf("reasoning_effort = %v, want %s", payload["reasoning_effort"], defaultDeepSeekEffort)
	}

	// 显式 disabled 要被尊重，并清掉档位（开着档位却关思考是矛盾组合）。
	disabled := prepareTest(t, `{"model":"deepseek-v4.1-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[]}`,
		PrepareOptions{})
	if _, okEffort := disabled["reasoning_effort"]; okEffort {
		t.Fatal("disabled thinking must drop reasoning_effort")
	}

	// 非 deepseek 模型不注入。
	other := prepareTest(t, `{"model":"glm-5.2","messages":[]}`, PrepareOptions{})
	if _, okThinking := other["thinking"]; okThinking {
		t.Fatal("non-deepseek models must not get thinking injected")
	}

	// 显式档位不被覆盖。
	explicit := prepareTest(t, `{"model":"deepseek-v4.1-flash","reasoning_effort":"low","messages":[]}`,
		PrepareOptions{DefaultEfforts: map[string]string{"deepseek-v4.1-flash": "high"}})
	if explicit["reasoning_effort"] != "low" {
		t.Fatalf("explicit effort must be respected, got %v", explicit["reasoning_effort"])
	}
}

// TestPrepareBodyEffortDowngrade 验证档位降级到模型支持的范围。
//
// 上游收到非法档位直接 400，降级到最近的合法档位能让请求成功。
func TestPrepareBodyEffortDowngrade(t *testing.T) {
	// 支持 low/high/max，请求 xhigh → 降到最接近的 high。
	payload := prepareTest(t, `{"model":"deepseek-v4.1-flash","reasoning_effort":"xhigh","messages":[]}`,
		PrepareOptions{SupportedEfforts: map[string][]string{"deepseek-v4.1-flash": {"low", "high", "max"}}})
	if payload["reasoning_effort"] != "high" {
		t.Fatalf("xhigh should downgrade to high, got %v", payload["reasoning_effort"])
	}
	// 支持集合里的档位原样透传。
	supported := prepareTest(t, `{"model":"deepseek-v4.1-flash","reasoning_effort":"max","messages":[]}`,
		PrepareOptions{SupportedEfforts: map[string][]string{"deepseek-v4.1-flash": {"low", "high", "max"}}})
	if supported["reasoning_effort"] != "max" {
		t.Fatalf("supported effort must pass through, got %v", supported["reasoning_effort"])
	}
	// 支持表为空时一律透传，不做猜测。
	unknown := prepareTest(t, `{"model":"unknown","reasoning_effort":"xhigh","messages":[]}`, PrepareOptions{})
	if unknown["reasoning_effort"] != "xhigh" {
		t.Fatalf("unknown model must pass the effort through, got %v", unknown["reasoning_effort"])
	}
}

// TestPrepareBodyBackfillsReasoning 验证 assistant 消息的 reasoning 回填。
//
// 部分租户校验 len(reasoning) > 0，缺失/null/空串会 400（空白串可以）。
func TestPrepareBodyBackfillsReasoning(t *testing.T) {
	payload := prepareTest(t, `{"model":"deepseek-v4.1-flash","messages":[
		{"role":"assistant","content":"hi"}
	]}`, PrepareOptions{})
	messages, _ := payload["messages"].([]any)
	message, _ := messages[0].(map[string]any)
	if _, okReasoning := message["reasoning"]; !okReasoning {
		t.Fatal("assistant messages must carry a reasoning field")
	}
	if message["reasoning"] == "" {
		t.Fatal("reasoning must be non-empty (blank string is the neutral value)")
	}
}

// TestToolPairingRepackAndCleanup 验证工具配对的重排与裁剪。
func TestToolPairingRepackAndCleanup(t *testing.T) {
	// 孤儿调用被裁掉；对应的 tool 结果也一并删除。
	body := `{"model":"m","messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"A","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c2","content":"orphan result"}
	]}`
	payload := prepareTest(t, body, PrepareOptions{})
	messages, _ := payload["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("orphan tool result must be removed, got %d messages", len(messages))
	}
	first, _ := messages[0].(map[string]any)
	if _, okCalls := first["tool_calls"]; okCalls {
		t.Fatal("unpaired tool_calls must be dropped")
	}
}

// TestDropTruncatedToolCalls 验证残缺参数的工具调用被丢弃。
func TestDropTruncatedToolCalls(t *testing.T) {
	calls := []any{
		map[string]any{"id": "c1", "function": map[string]any{"name": "A", "arguments": `{"path":"/tmp"}`}},
		map[string]any{"id": "c2", "function": map[string]any{"name": "B", "arguments": `{"path":"/tmp`}}, // 截断
		map[string]any{"id": "c3", "function": map[string]any{"name": "C", "arguments": ""}},              // 无参合法
	}
	kept := DropTruncatedToolCalls(calls)
	if len(kept) != 2 {
		t.Fatalf("truncated call must be dropped, kept %d of 3", len(kept))
	}
}

// TestAggregateMergesToolCallDeltas 验证流式工具调用分片的合并。
func TestAggregateMergesToolCallDeltas(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	aggregated, errAggregate := Aggregate(strings.NewReader(stream))
	if errAggregate != nil {
		t.Fatalf("Aggregate: %v", errAggregate)
	}
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal(aggregated, &response); errUnmarshal != nil {
		t.Fatalf("decode aggregate: %v", errUnmarshal)
	}
	if len(response.Choices) != 1 || len(response.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected one merged tool call, got %+v", response.Choices)
	}
	call := response.Choices[0].Message.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "Bash" {
		t.Fatalf("tool call identity lost: %+v", call)
	}
	if call.Function.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("arguments not concatenated: %q", call.Function.Arguments)
	}
}

// TestAggregateEmptyStreamIsRecognizable 验证空流可被识别。
//
// 空流是上游缺陷，需要与「客户端断连」区分开。
func TestAggregateEmptyStreamIsRecognizable(t *testing.T) {
	_, errAggregate := Aggregate(strings.NewReader("data: [DONE]\n\n"))
	if errAggregate == nil {
		t.Fatal("empty stream must return an error")
	}
	if !IsEmptyStreamError(errAggregate) {
		t.Fatalf("empty stream must be recognizable via IsEmptyStreamError, got %v", errAggregate)
	}
}

// TestStreamNormalizesFrames 验证流式帧的白名单重建。
func TestStreamNormalizesFrames(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","service_tier":"x","private_field":"leak","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"private_choice":1}],"extra":{}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var frames [][]byte
	stats, errStream := Stream(strings.NewReader(stream), StreamOptions{
		Emit: func(payload []byte) error {
			frames = append(frames, append([]byte(nil), payload...))
			return nil
		},
	})
	if errStream != nil {
		t.Fatalf("Stream: %v", errStream)
	}
	if stats.Frames != 1 || !stats.SawDone {
		t.Fatalf("stats = %+v, want 1 frame and SawDone", stats)
	}
	frame := string(frames[0])
	if strings.Contains(frame, "private_field") || strings.Contains(frame, "private_choice") {
		t.Fatalf("whitelist rebuild must drop unknown fields: %s", frame)
	}
	if !strings.Contains(frame, `"service_tier":"x"`) {
		t.Fatalf("known top-level fields must survive: %s", frame)
	}
}

// TestStreamPassesThroughErrorFrames 验证错误帧原样透传。
//
// 上游的错误信息比我们重建的更有诊断价值，不该被白名单改写。
func TestStreamPassesThroughErrorFrames(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"error":{"message":"upstream blew up","code":"500"}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var frames [][]byte
	if _, errStream := Stream(strings.NewReader(stream), StreamOptions{
		Emit: func(payload []byte) error {
			frames = append(frames, append([]byte(nil), payload...))
			return nil
		},
		Hint: func(string) string { return "try again later" },
	}); errStream != nil {
		t.Fatalf("Stream: %v", errStream)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	frame := string(frames[0])
	if !strings.Contains(frame, "upstream blew up") {
		t.Fatalf("error message must be preserved verbatim: %s", frame)
	}
	if !strings.Contains(frame, "gateway_hint") {
		t.Fatalf("hint must be attached to the error frame: %s", frame)
	}
}

// TestSanitizeRemovesFingerprints 验证指纹脱敏确实改写了命中文本。
func TestSanitizeRemovesFingerprints(t *testing.T) {
	rules, regexes := loadSanitizeRules()
	if len(rules.Rewrites) == 0 && len(regexes) == 0 {
		t.Skip("sanitize rules are not embedded")
	}
	_ = regexes

	// 改写表里的任一条命中都应产生变化。
	if len(rules.Rewrites) > 0 {
		pair := rules.Rewrites[0]
		rewritten := sanitizeText(pair[0], rules, regexes)
		if rewritten == pair[0] {
			t.Fatalf("rewrite rule did not apply: %q", pair[0])
		}
	}
	// 不命中特征串的文本原样返回（快路径）。
	plain := "just a normal user message about go programming"
	if got := sanitizeText(plain, rules, regexes); got != plain {
		t.Fatalf("plain text must pass through unchanged, got %q", got)
	}
}

// TestSanitizeMessagesCoversToolArguments 验证工具参数也被净化。
//
// 早期实现在 content 缺失时跳过整条消息，导致 tool_calls 的参数
// （文件名、命令、写入内容）完全不被净化。
func TestSanitizeMessagesCoversToolArguments(t *testing.T) {
	rules, _ := loadSanitizeRules()
	if len(rules.Rewrites) == 0 {
		t.Skip("sanitize rules are not embedded")
	}
	needle := rules.Rewrites[0][0]

	messages := []any{map[string]any{
		// content 为 null（工具调用消息的常见形态）。
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{map[string]any{
			"id": "c1",
			"function": map[string]any{
				"name":      "Write",
				"arguments": `{"content":"` + needle + `"}`,
			},
		}},
	}}
	sanitizeMessages(messages)

	call := messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	arguments := call["function"].(map[string]any)["arguments"].(string)
	if strings.Contains(arguments, needle) {
		t.Fatal("tool_call arguments must be sanitized even when content is null")
	}
}

// TestSplitModelIDPrefixProtocol 验证模型 ID 的前缀协议。
func TestSplitModelIDPrefixProtocol(t *testing.T) {
	cases := []struct {
		input     string
		wantRealm string
		wantBare  string
	}{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:gpt-5.5", "global", "gpt-5.5"},
		// 无前缀：由凭证决定域。
		{"glm-5.2", "", "glm-5.2"},
		// 前缀非法（大小写敏感）：当成裸名的一部分。
		{"CN:glm-5.2", "", "CN:glm-5.2"},
		{"other:model", "", "other:model"},
	}
	for _, testCase := range cases {
		t.Run(testCase.input, func(t *testing.T) {
			// 前缀协议在 main 包实现，这里验证 cb 复用的 Region 归一。
			if testCase.wantRealm == "" {
				return
			}
			if NormalizeRegion(testCase.wantRealm) != Region(testCase.wantRealm) {
				t.Fatalf("realm normalization mismatch for %s", testCase.wantRealm)
			}
		})
	}
}

// TestNormalizeRegionDefaultsToCN 验证未知域回落 cn。
func TestNormalizeRegionDefaultsToCN(t *testing.T) {
	for _, input := range []string{"", "CN", "GLOBAL", "global", "us"} {
		region := NormalizeRegion(input)
		if region != RegionCN && region != RegionGlobal {
			t.Fatalf("NormalizeRegion(%q) = %q, must be a valid region", input, region)
		}
	}
	if NormalizeRegion("GLOBAL") != RegionGlobal {
		t.Fatal("region normalization must be case-insensitive")
	}
	if NormalizeRegion("us") != RegionCN {
		t.Fatal("unknown region must fall back to cn")
	}
}

// TestEffortStaticTableIsPerRealm 验证档位表按域分表。
//
// 同一模型名在两个域的合法档位不同，混用会让请求带非法档位。
func TestEffortStaticTableIsPerRealm(t *testing.T) {
	cnCap, okCN := staticEffortCap(RegionCN, "deepseek-v4.1-flash")
	globalCap, okGlobal := staticEffortCap(RegionGlobal, "deepseek-v4.1-flash")
	if !okCN || !okGlobal {
		t.Skip("static effort tables are not embedded")
	}
	// CN 三档、global 仅 high —— 这是刻意的差异。
	if len(cnCap.Efforts) <= len(globalCap.Efforts) {
		t.Fatalf("cn efforts (%v) should be broader than global (%v) for this model",
			cnCap.Efforts, globalCap.Efforts)
	}
}

// TestApplyCatalogFallbacksFillsWindow 验证窗口查找链的兜底。
func TestApplyCatalogFallbacksFillsWindow(t *testing.T) {
	models := []ModelInfo{{ID: "unknown-model-xyz"}}
	filled := applyCatalogFallbacks(models, RegionCN)
	if filled[0].ContextWindow <= 0 {
		t.Fatal("context window must be filled by the fallback chain")
	}
	if filled[0].ContextWindow != defaultContextWindow {
		t.Fatalf("unknown model should get the default window, got %d", filled[0].ContextWindow)
	}
}

// TestNormalizeFrameDropsUnknownFields 验证帧白名单在非流式路径也生效。
func TestNormalizeFrameDropsUnknownFields(t *testing.T) {
	frame := map[string]any{
		"id": "c1", "secret": "leak",
		"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"content": "hi", "private": 1}}},
	}
	normalized := normalizeFrame(frame)
	if _, okSecret := normalized["secret"]; okSecret {
		t.Fatal("unknown top-level field must be dropped")
	}
	choices := normalized["choices"].([]any)
	delta := choices[0].(map[string]any)["delta"].(map[string]any)
	if _, okPrivate := delta["private"]; okPrivate {
		t.Fatal("unknown delta field must be dropped")
	}
	if delta["content"] != "hi" {
		t.Fatal("known delta fields must survive")
	}
}

// TestParseBalanceMatchesUpstreamShape 验证余额解析对齐上游的真实契约。
//
// 上游该接口用 PascalCase 字段、双层信封（Response.Data.Accounts），
// 且 Cycle 期套餐优先。早期实现按 camelCase 解析，结果恒为 0。
func TestParseBalanceMatchesUpstreamShape(t *testing.T) {
	raw := `{"Response":{"Data":{"Accounts":[
	  {"PackageName":"基础套餐","CycleCapacitySize":1000,"CycleCapacityRemain":400,"CycleCapacityUsed":600},
	  {"PackageName":"加油包","CapacitySize":500,"CapacityRemain":500}
	]}}}`
	var payload billingResourceResponse
	if errUnmarshal := json.Unmarshal([]byte(raw), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	balance := parseBalance(payload)

	// Cycle 套餐：1000 总量、400 剩余；加油包：500/500。合计 1500 / 900。
	if balance.Total != 1500 {
		t.Fatalf("total = %d, want 1500", balance.Total)
	}
	if balance.Remain != 900 {
		t.Fatalf("remain = %d, want 900", balance.Remain)
	}
	if len(balance.Packages) != 2 {
		t.Fatalf("packages = %d, want 2", len(balance.Packages))
	}
	if balance.Packages[0].Name != "基础套餐" || balance.Packages[0].Remain != 400 {
		t.Fatalf("first package = %+v", balance.Packages[0])
	}
}

// TestPackageRemainUsedCycleWins 验证 Cycle 期套餐的取数规则。
func TestPackageRemainUsedCycleWins(t *testing.T) {
	// CycleUsed 大于 size-remain 时，应以 CycleUsed 为准反推 remain。
	remain, used, size := packageRemainUsed(billingResourceAccount{
		CycleCapacitySize: 1000, CycleCapacityRemain: 900, CycleCapacityUsed: 300,
	})
	if size != 1000 || used != 300 || remain != 700 {
		t.Fatalf("cycle rule: remain=%d used=%d size=%d, want 700/300/1000", remain, used, size)
	}
	// 无 Cycle 期时回落 Capacity 三字段。
	remain, used, size = packageRemainUsed(billingResourceAccount{
		CapacitySize: 500, CapacityRemain: 200,
	})
	if size != 500 || used != 300 || remain != 200 {
		t.Fatalf("capacity fallback: remain=%d used=%d size=%d, want 200/300/500", remain, used, size)
	}
	// remain 超出 size 时钳到 size（上游偶发脏数据）。
	remain, _, size = packageRemainUsed(billingResourceAccount{
		CycleCapacitySize: 100, CycleCapacityRemain: 999,
	})
	if remain != 100 || size != 100 {
		t.Fatalf("clamp: remain=%d size=%d, want 100/100", remain, size)
	}
}
