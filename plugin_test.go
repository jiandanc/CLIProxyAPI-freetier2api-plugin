package main

// 本文件测试插件的 ABI 层：注册、模型注册、凭证解析、执行与流式转发。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// TestRegisterReturnsFullContract 验证注册响应满足宿主契约。
func TestRegisterReturnsFullContract(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)

	result := callMethod(t, pluginabi.MethodPluginRegister, lifecycleRequest{ConfigYAML: buildConfigYAML(cfg)})
	var registration registration
	if errUnmarshal := json.Unmarshal(result, &registration); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}

	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", registration.SchemaVersion, pluginabi.SchemaVersion)
	}
	if registration.Metadata.Version == "" {
		t.Fatal("metadata.version must not be empty")
	}
	capabilities := registration.Capabilities
	if !capabilities.ModelProvider || !capabilities.AuthProvider || !capabilities.Executor ||
		!capabilities.QuotaProvider || !capabilities.ManagementAPI {
		t.Fatalf("all five capabilities must be declared: %+v", capabilities)
	}
	// model.for_auth 只在 executor_model_scope 含 oauth/both 时才被宿主调用；
	// 声明错会让 realm 隔离静默失效。
	if capabilities.ExecutorModelScope != string(pluginapi.ExecutorModelScopeBoth) {
		t.Fatalf("executor_model_scope = %q, want %q (model.for_auth depends on it)",
			capabilities.ExecutorModelScope, pluginapi.ExecutorModelScopeBoth)
	}
	// 只声明 chat-completions：多协议由宿主翻译。
	if len(capabilities.ExecutorInputFormats) != 1 || capabilities.ExecutorInputFormats[0] != formatChatCompletions {
		t.Fatalf("executor_input_formats = %v, want [%s]", capabilities.ExecutorInputFormats, formatChatCompletions)
	}
}

// TestReconfigureKeepsBackgroundRunning 验证热更新返回完整注册响应且不重启后台。
func TestReconfigureKeepsBackgroundRunning(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)

	callMethod(t, pluginabi.MethodPluginRegister, lifecycleRequest{ConfigYAML: buildConfigYAML(cfg)})
	if !backgroundRunning() {
		t.Fatal("background work must start on register")
	}

	result := callMethod(t, pluginabi.MethodPluginReconfigure, lifecycleRequest{ConfigYAML: buildConfigYAML(cfg)})
	var registration registration
	if errUnmarshal := json.Unmarshal(result, &registration); errUnmarshal != nil {
		t.Fatalf("decode reconfigure response: %v", errUnmarshal)
	}
	if registration.Metadata.Name == "" {
		t.Fatal("reconfigure must return the full registration shape")
	}
	if !backgroundRunning() {
		t.Fatal("reconfigure must not stop the background work")
	}
}

// TestUnknownMethodReturnsEnvelopeNotError 验证未知方法以信封形式报错。
func TestUnknownMethodReturnsEnvelopeNotError(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	raw, errHandle := handleMethod("no.such.method", nil)
	if errHandle != nil {
		t.Fatalf("unknown method must not return a Go error: %v", errHandle)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("want unknown_method envelope, got %s", raw)
	}
}

// TestAuthParseRecognizesOwnCredential 验证凭证归属判定。
func TestAuthParseRecognizesOwnCredential(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	cases := []struct {
		name       string
		fileName   string
		provider   string
		body       string
		wantHandle bool
	}{
		{"nested credential", "workbuddy-abc.json", "", sampleCredentialJSON("cn", "abc"), true},
		// 生产环境的真实形态：workbuddy-<uuid>.json，只有 auth+account 两个键、
		// 没有 type 字段也没有 device_token。这是用户实际放进 auths/ 的文件。
		{"real world uuid file", "workbuddy-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json", "",
			`{"account":{"enterpriseId":"","nickname":"x","uid":"79fdc1fc-de43-40da-b6f0-fe05d9e4367b"},
			  "auth":{"accessToken":"at","domain":"www.codebuddy.cn","expiresAt":1792274401,
			          "realm":"cn","refreshToken":"rt"}}`, true},
		// 扁平形态靠文件名约定归属（顶层 token 是通用字段，不能作判据）。
		{"flat credential with proper name", "workbuddy-wb.json", "", `{"accessToken":"at","uid":"u1"}`, true},
		{"provider hint", "other.json", providerKey, `{"accessToken":"at"}`, true},
		// 文件名与结构都不指向本插件：必须拒绝，否则会误吞别家的凭证。
		{"ambiguous flat file", "wb.json", "", `{"accessToken":"at","uid":"u1"}`, false},
		{"foreign provider", "claude.json", "anthropic", `{"access_token":"x","refresh_token":"y"}`, false},
		{"not json", "notes.json", "", "this is not json", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
				Provider: testCase.provider,
				FileName: testCase.fileName,
				RawJSON:  []byte(testCase.body),
			})
			var response pluginapi.AuthParseResponse
			if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
				t.Fatalf("decode parse response: %v", errUnmarshal)
			}
			if response.Handled != testCase.wantHandle {
				t.Fatalf("handled = %v, want %v", response.Handled, testCase.wantHandle)
			}
			if !testCase.wantHandle {
				return
			}
			if response.Auth.Provider != providerKey {
				t.Fatalf("provider = %q, want %q", response.Auth.Provider, providerKey)
			}
			if response.Auth.FileName != testCase.fileName {
				t.Fatalf("file_name = %q, want %q", response.Auth.FileName, testCase.fileName)
			}
		})
	}
}

// TestAuthParsePutsRefreshTokenInMetadata 验证宿主刷新依赖的契约。
//
// 宿主判断「这个凭证能不能刷新」看的是 Metadata 里的 refresh_token。
// 缺了它，OAuth 账号的 access token 过期后永远不会被续期。
func TestAuthParsePutsRefreshTokenInMetadata(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
		FileName: "workbuddy-x.json",
		RawJSON:  []byte(sampleCredentialJSON("cn", "x")),
	})
	var response pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode parse response: %v", errUnmarshal)
	}
	if response.Auth.Metadata["refresh_token"] == nil {
		t.Fatal("metadata.refresh_token is required for host-driven refresh")
	}
	if response.Auth.Metadata["realm"] != "cn" {
		t.Fatalf("metadata.realm = %v, want cn", response.Auth.Metadata["realm"])
	}
	// 账号令牌绝不回传到 metadata/attributes（只有 StorageJSON 里可以有）。
	for _, key := range []string{"access_token", "token", "device_token"} {
		if _, okKey := response.Auth.Metadata[key]; okKey {
			t.Fatalf("metadata must not leak %q", key)
		}
	}
	if _, okKey := response.Auth.Attributes["access_token"]; okKey {
		t.Fatal("attributes must not leak access_token")
	}
}

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
	handleModelsToggle(pluginapi.ManagementRequest{
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

// TestExecutorNonStreamingAggregatesUpstream 验证非流式执行的完整链路。
func TestExecutorNonStreamingAggregatesUpstream(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v2/chat/completions") {
			// 上游强制流式：非流式由插件侧聚合。
			return fakeUpstreamResponse{Status: http.StatusOK, Body: strings.Join([]string{
				`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}`,
				"",
				`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"，世界"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
				"",
				"data: [DONE]",
				"",
			}, "\n")}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	result := callMethod(t, pluginabi.MethodExecutorExecute, executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "cn:glm-5.2",
			Format:  formatChatCompletions,
			Payload: []byte(`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		},
	})
	var response pluginapi.ExecutorResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode executor response: %v", errUnmarshal)
	}

	var aggregated struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(response.Payload, &aggregated); errUnmarshal != nil {
		t.Fatalf("decode aggregated payload: %v (raw=%s)", errUnmarshal, response.Payload)
	}
	if aggregated.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", aggregated.Object)
	}
	if len(aggregated.Choices) != 1 || aggregated.Choices[0].Message.Content != "你好，世界" {
		t.Fatalf("content not aggregated as expected: %+v", aggregated.Choices)
	}
	if aggregated.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop (default when upstream omits it)", aggregated.Choices[0].FinishReason)
	}
	// total_tokens 缺失时应由 prompt + completion 补齐。
	if aggregated.Usage.TotalTokens != 7 {
		t.Fatalf("total_tokens = %d, want 7 (derived)", aggregated.Usage.TotalTokens)
	}
}

// TestExecutorStreamEmitsNormalizedFrames 验证流式转发把帧规范化后投递。
func TestExecutorStreamEmitsNormalizedFrames(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v2/chat/completions") {
			return fakeUpstreamResponse{Status: http.StatusOK, Body: strings.Join([]string{
				`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"A"}}],"session_id":"leak-me"}`,
				"",
				`data: {"choices":[{"index":0,"delta":{"content":"B"}}]}`,
				"",
				"data: [DONE]",
				"",
			}, "\n")}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	result := callMethod(t, pluginabi.MethodExecutorExecuteStream, executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "cn:glm-5.2",
			Format:  formatChatCompletions,
			Payload: []byte(`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		},
		StreamID: "s1",
	})
	var response rpcExecutorStreamResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode stream response: %v", errUnmarshal)
	}
	if response.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream content-type = %q", response.Headers.Get("Content-Type"))
	}

	if !waitFor(t, 3e9, func() bool { return len(host.emittedChunks()) >= 2 }) {
		t.Fatalf("expected at least 2 emitted chunks, got %v", host.emittedChunks())
	}
	chunks := host.emittedChunks()
	for _, chunk := range chunks {
		if strings.Contains(chunk, "session_id") {
			t.Fatalf("upstream private field leaked to downstream: %s", chunk)
		}
		if !strings.HasPrefix(chunk, "data:") {
			// chat-completions SSE 分片必须符合标准 SSE 规范（带 data: 前缀）。
			t.Fatalf("chat-completions chunk must have data: prefix, got %q", chunk)
		}
	}
	// 第二帧缺 id，应被续传为第一帧的 id。
	if !strings.Contains(chunks[1], `"id":"c1"`) {
		t.Fatalf("missing id must be backfilled from the first frame: %s", chunks[1])
	}
	// 流必须以一次 close 收尾。
	if !waitFor(t, 3e9, func() bool { return len(host.closed) == 1 }) {
		t.Fatalf("expected exactly one stream close, got %d", len(host.closed))
	}
	if len(host.streamErrors()) != 0 {
		t.Fatalf("successful stream must not report an error: %v", host.streamErrors())
	}
}

// TestExecutorStreamReportsUpstreamError 验证上游错误被上报为流失败。
func TestExecutorStreamReportsUpstreamError(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v2/chat/completions") {
			return fakeUpstreamResponse{
				Status: http.StatusTooManyRequests,
				Body:   `{"code":6004,"msg":"The model provider is rate-limiting requests."}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	callMethod(t, pluginabi.MethodExecutorExecuteStream, executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "cn:glm-5.2",
			Payload: []byte(`{"model":"cn:glm-5.2","messages":[]}`),
		},
		StreamID: "s1",
	})

	if !waitFor(t, 3e9, func() bool { return len(host.streamErrors()) > 0 }) {
		t.Fatal("upstream 429 must surface as a stream failure")
	}
	if !strings.Contains(host.streamErrors()[0], "rate-limiting") {
		t.Fatalf("stream error should carry the upstream message verbatim: %v", host.streamErrors())
	}
}

// TestExecutorRejectsUnsupportedFormat 验证格式声明不一致时明确报错。
func TestExecutorRejectsUnsupportedFormat(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	raw, errHandle := handleMethod(pluginabi.MethodExecutorExecute, mustMarshal(executorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "1", Model: "cn:x", Format: "gemini"},
	}))
	if errHandle == nil {
		t.Fatalf("unsupported format must produce an error envelope, got %s", raw)
	}

	_ = workbuddy.RegionCN
	_ = http.StatusOK
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

// TestQuotaResetDomesticAndGlobal 验证国内版账号可签到重置额度，国际版被正确拒绝。
func TestQuotaResetDomesticAndGlobal(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.addAccount("2", "global-account", sampleCredentialJSON("global", "g1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "daily-checkin") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"credit":10,"energy":5,"streak":3}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	// 国际版账号重置应被拦截并提示
	resGlobal := callMethod(t, pluginabi.MethodQuotaReset, pluginapi.QuotaResetRequest{AuthID: "2"})
	var respGlobal pluginapi.QuotaResetResponse
	if errUnmarshal := json.Unmarshal(resGlobal, &respGlobal); errUnmarshal != nil {
		t.Fatalf("decode global quota reset: %v", errUnmarshal)
	}
	if respGlobal.Success {
		t.Fatal("global account quota reset must not succeed via checkin")
	}

	// 国内版账号重置应成功执行签到
	resCN := callMethod(t, pluginabi.MethodQuotaReset, pluginapi.QuotaResetRequest{AuthID: "1"})
	var respCN pluginapi.QuotaResetResponse
	if errUnmarshal := json.Unmarshal(resCN, &respCN); errUnmarshal != nil {
		t.Fatalf("decode cn quota reset: %v", errUnmarshal)
	}
	if !respCN.Success {
		t.Fatalf("cn account quota reset should succeed, message: %s", respCN.Message)
	}
}

// TestManagementAutoAllRouteDispatched 验证 /tasks/auto_all 路由分发成功且不报 404。
func TestManagementAutoAllRouteDispatched(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	raw, err := handleMethod(pluginabi.MethodManagementHandle, mustMarshal(pluginapi.ManagementRequest{
		Method: "POST",
		Path:   "/v0/management/plugins/freetier2api/tasks/auto_all",
	}))
	if err != nil {
		t.Fatalf("handleMethod error: %v", err)
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	var resp pluginapi.ManagementResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("/tasks/auto_all must be routed, got 404: %s", string(resp.Body))
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/tasks/auto_all expected 200, got: %d", resp.StatusCode)
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

// TestFormatSSEChunk 验证流式分片格式化确保满足 SSE 标准规范（以 data: 开头，以 \n\n 结尾）。
func TestFormatSSEChunk(t *testing.T) {
	// 1. 裸 JSON 分片自动补全 data: 与 \n\n
	out := formatSSEChunk([]byte(`{"id":"1"}`))
	if string(out) != "data: {\"id\":\"1\"}\n\n" {
		t.Errorf("unexpected format: %q", string(out))
	}

	// 2. 已有 data: 前缀的分片确保末尾有 \n\n
	out = formatSSEChunk([]byte("data: {\"id\":\"2\"}\n\n"))
	if string(out) != "data: {\"id\":\"2\"}\n\n" {
		t.Errorf("unexpected format: %q", string(out))
	}

	// 3. 裸 [DONE] 标记补全为 data: [DONE]\n\n
	out = formatSSEChunk([]byte("[DONE]"))
	if string(out) != "data: [DONE]\n\n" {
		t.Errorf("unexpected format: %q", string(out))
	}

	// 4. 空分片返回 nil
	if out = formatSSEChunk(nil); out != nil {
		t.Errorf("expected nil for empty payload, got %q", string(out))
	}
	if out = formatSSEChunk([]byte("   \n\r  ")); out != nil {
		t.Errorf("expected nil for whitespace payload, got %q", string(out))
	}
}

// TestDisabledModelsConfig 验证 YAML 配置 disabled_models 的解析格式与生效行为。
func TestDisabledModelsConfig(t *testing.T) {
	// 1. 块列表写法，含三种前缀形态
	yamlBlock := []byte(`
enabled_realms: cn,global
disabled_models:
  - workbuddycn:glm-5.2
  - cn:deepseek-v4-pro
  - qwen-3.5-plus
`)
	cfg, errDecode := decodeConfig(yamlBlock)
	if errDecode != nil {
		t.Fatalf("decodeConfig failed: %v", errDecode)
	}
	if errApply := applyConfig(cfg); errApply != nil {
		t.Fatalf("applyConfig failed: %v", errApply)
	}

	// 厂商限定只影响该厂商
	if !isModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("workbuddycn:glm-5.2 must be disabled")
	}
	if isModelDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("workbuddyglobal:glm-5.2 should NOT be disabled")
	}
	// 区域别名展开到该区域所有厂商
	if !isModelDisabled("workbuddycn", "deepseek-v4-pro") {
		t.Fatal("workbuddycn:deepseek-v4-pro must be disabled by cn: prefix")
	}
	if isModelDisabled("workbuddyglobal", "deepseek-v4-pro") {
		t.Fatal("workbuddyglobal:deepseek-v4-pro should NOT be disabled by cn: prefix")
	}
	// 裸名对所有厂商生效
	if !isModelDisabled("workbuddycn", "qwen-3.5-plus") || !isModelDisabled("workbuddyglobal", "qwen-3.5-plus") {
		t.Fatal("bare name must be disabled on all vendors")
	}

	// 2. 流式列表写法
	yamlFlow := []byte(`
enabled_realms: cn,global
disabled_models: [workbuddyglobal:glm-5.2]
`)
	cfgFlow, errDecodeFlow := decodeConfig(yamlFlow)
	if errDecodeFlow != nil {
		t.Fatalf("decode flow config failed: %v", errDecodeFlow)
	}
	if errApply := applyConfig(cfgFlow); errApply != nil {
		t.Fatalf("applyConfig flow failed: %v", errApply)
	}
	if !isModelDisabled("workbuddyglobal", "glm-5.2") {
		t.Fatal("flow list workbuddyglobal:glm-5.2 must be disabled")
	}
	// 新配置整体替换：上一轮的 cn 限定不再生效
	if isModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("workbuddycn:glm-5.2 should not be disabled after reconfigure")
	}
}

// TestConfigAndStateDisabledModelsUnion 验证配置声明与页面操作取并集：
// 页面「启用」不能放开配置里声明的禁用项。
func TestConfigAndStateDisabledModelsUnion(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)
	cfg.DisabledModels = []string{"cn:glm-5.2"}
	if errApply := applyConfig(cfg); errApply != nil {
		t.Fatalf("applyConfig failed: %v", errApply)
	}
	if !isModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("config-declared disable must take effect")
	}

	// 页面尝试启用同一模型：配置声明仍在，禁用不放开。
	handleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["cn:glm-5.2"],"disabled":false}`),
	})
	if !isModelDisabled("workbuddycn", "glm-5.2") {
		t.Fatal("page-side enable must NOT lift a config-declared disable")
	}

	// 页面禁用另一个模型：两个来源并存，都被拦截。
	handleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["workbuddycn:glm-5.3"],"disabled":true}`),
	})
	if !isModelDisabled("workbuddycn", "glm-5.3") {
		t.Fatal("page-side disable must take effect")
	}
	if !isModelDisabled("workbuddycn", "glm-5.2") {
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
	handleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.2","alias":"glm-max"}`),
	})
	handleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"qodercn:gmodel","alias":"glm-max"}`),
	})

	if got := modelAliasFor("workbuddycn", "glm-5.2"); got != "glm-max" {
		t.Fatalf("workbuddycn alias = %q, want glm-max", got)
	}
	if got := modelAliasFor("qodercn", "gmodel"); got != "glm-max" {
		t.Fatalf("qodercn alias = %q, want glm-max", got)
	}
	// 作用域是单个模型 ID：同供应商的其它模型不受影响。
	if got := modelAliasFor("workbuddycn", "glm-5.3"); got != "" {
		t.Fatalf("unrelated model alias = %q, want empty", got)
	}

	// 出站：同一别名按请求供应商还原成各自的官方 ID。
	if got := resolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.2" {
		t.Fatalf("resolveOutboundModel(workbuddycn) = %q, want glm-5.2", got)
	}
	if got := resolveOutboundModel("qodercn", "glm-max"); got != "gmodel" {
		t.Fatalf("resolveOutboundModel(qodercn) = %q, want gmodel", got)
	}
	// 未设别名的名字原样返回（Qoder 等直接以上游 key 注册时无需翻译）。
	if got := resolveOutboundModel("qodercn", "gmodel"); got != "gmodel" {
		t.Fatalf("passthrough = %q, want gmodel", got)
	}

	// 同供应商内别名唯一：把别名换绑到另一个模型，旧绑定被清掉。
	handleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.3","alias":"glm-max"}`),
	})
	if got := modelAliasFor("workbuddycn", "glm-5.2"); got != "" {
		t.Fatalf("rebind must clear the old binding, got %q", got)
	}
	if got := resolveOutboundModel("workbuddycn", "glm-max"); got != "glm-5.3" {
		t.Fatalf("after rebind resolveOutboundModel = %q, want glm-5.3", got)
	}

	// 清除别名（空串）恢复官方 ID。
	handleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.3","alias":""}`),
	})
	if got := modelAliasFor("workbuddycn", "glm-5.3"); got != "" {
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
	handleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"qodercn:qmodel_38max","alias":"qwen-max"}`),
	})

	out := applyModelAliases("qodercn", in)
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
	out := rewritePayloadModel([]byte(`{"model":"gmodel","messages":[]}`), "gm51model")
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(out, &decoded); errUnmarshal != nil {
		t.Fatalf("decode rewritten payload: %v", errUnmarshal)
	}
	if decoded["model"] != "gm51model" {
		t.Fatalf("payload model = %v, want gm51model", decoded["model"])
	}

	// 非法 JSON 原样返回（不让本地解析失败中断一次对话）。
	bad := []byte(`not json`)
	if got := rewritePayloadModel(bad, "gmodel"); string(got) != string(bad) {
		t.Fatalf("invalid payload must pass through unchanged, got %q", string(got))
	}
}

// TestModelChangeMarksRestartPending 验证禁用/别名变更会置「待生效」标记，
// 且宿主重建模型注册表（applyModelCatalog）时自动清除它。
func TestModelChangeMarksRestartPending(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)

	if snapshotState().RestartPending {
		t.Fatal("fresh state must not be restart-pending")
	}

	// 禁用模型：置位。
	handleModelsToggle(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/toggle",
		Body: []byte(`{"models":["workbuddycn:glm-5.2"],"disabled":true}`),
	})
	if !snapshotState().RestartPending {
		t.Fatal("toggle must mark restart-pending")
	}

	// 宿主重建注册表（register/reconfigure 路径）时清除。
	applyModelCatalog(cfg)
	if snapshotState().RestartPending {
		t.Fatal("applyModelCatalog must clear restart-pending")
	}

	// 别名变更同样置位，且状态负载带上标记，页面据此常驻提示。
	handleModelAlias(pluginapi.ManagementRequest{
		Method: "POST", Path: "/models/alias",
		Body: []byte(`{"model":"workbuddycn:glm-5.2","alias":"glm-max"}`),
	})
	if !snapshotState().RestartPending {
		t.Fatal("alias change must mark restart-pending")
	}
	payload := buildStatusPayload(pluginapi.ManagementRequest{})
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
	dir1 := defaultStateDir()
	want1 := filepath.Join(tempHome, ".freetier2api-plugin")
	if dir1 != want1 {
		t.Fatalf("defaultStateDir = %q, want %q", dir1, want1)
	}

	// 2. 存在 ~/.cli-proxy-api（Docker 挂载场景）→ 用其下 freetier2api 子目录
	cpaDir := filepath.Join(tempHome, ".cli-proxy-api")
	if errMkdir := os.MkdirAll(cpaDir, 0o755); errMkdir != nil {
		t.Fatalf("create temp cpa dir: %v", errMkdir)
	}
	dir2 := defaultStateDir()
	want2 := filepath.Join(cpaDir, "freetier2api")
	if dir2 != want2 {
		t.Fatalf("defaultStateDir with cpa dir = %q, want %q", dir2, want2)
	}
}
