package main

// 本文件测试插件的 ABI 层：注册、模型注册、凭证解析、执行与流式转发。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api-plugin/cpasdk/pluginabi"
	"workbuddy2api-plugin/cpasdk/pluginapi"
	"workbuddy2api-plugin/internal/cb"
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
		// 模型目录探测：返回两个域共有的模型名。
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":1000000,"maxOutputTokens":131072}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	for _, testCase := range []struct {
		authIndex string
		wantID    string
		denyID    string
	}{
		{"1", "cn:glm-5.2", "global:glm-5.2"},
		{"2", "global:glm-5.2", "cn:glm-5.2"},
	} {
		t.Run(testCase.wantID, func(t *testing.T) {
			result := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: testCase.authIndex})
			var response pluginapi.ModelResponse
			if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
				t.Fatalf("decode model response: %v", errUnmarshal)
			}
			if response.Provider != providerKey {
				t.Fatalf("provider = %q, want %q", response.Provider, providerKey)
			}
			ids := make([]string, 0, len(response.Models))
			for _, model := range response.Models {
				ids = append(ids, model.ID)
			}
			if !containsString(ids, testCase.wantID) {
				t.Fatalf("models = %v, want to contain %q", ids, testCase.wantID)
			}
			if containsString(ids, testCase.denyID) {
				t.Fatalf("models = %v must NOT contain %q (cross-realm leak)", ids, testCase.denyID)
			}
		})
	}
}

// TestStaticModelsCarryRealmPrefixes 验证静态目录带 realm 前缀与元数据。
func TestStaticModelsCarryRealmPrefixes(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
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
	byID := map[string]pluginapi.ModelInfo{}
	for _, model := range response.Models {
		byID[model.ID] = model
	}
	for _, wantID := range []string{"cn:glm-5.2", "global:glm-5.2"} {
		model, okModel := byID[wantID]
		if !okModel {
			t.Fatalf("static models missing %q (have %v)", wantID, keysOf(byID))
		}
		if model.ContextLength != 1000000 {
			t.Fatalf("%s context_length = %d, want 1000000", wantID, model.ContextLength)
		}
		if model.Thinking == nil || len(model.Thinking.Levels) == 0 {
			t.Fatalf("%s must expose reasoning levels", wantID)
		}
		if !strings.Contains(model.Description, "credit") {
			t.Fatalf("%s description should carry the credits prefix: %q", wantID, model.Description)
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
		if strings.HasPrefix(chunk, "data:") {
			// chat-completions 必须投裸 JSON（宿主补 data: 前缀与 [DONE]）。
			t.Fatalf("chat-completions chunk must be bare JSON, got %q", chunk)
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

	_ = cb.RegionCN
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
