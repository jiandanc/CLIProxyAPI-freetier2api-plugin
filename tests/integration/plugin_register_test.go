package tests

import (
	"encoding/json"
	"net/http"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/app"
)

// TestRegisterReturnsFullContract 验证注册响应满足宿主契约。
func TestRegisterReturnsFullContract(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)

	result := callMethod(t, pluginabi.MethodPluginRegister, app.LifecycleRequest{
		ConfigYAML:    buildConfigYAML(cfg),
		SchemaVersion: pluginabi.SchemaVersion,
	})
	var reg app.RegistrationInfo
	if errUnmarshal := json.Unmarshal(result, &reg); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}

	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Version == "" {
		t.Fatal("metadata.version must not be empty")
	}
	capabilities := reg.Capabilities
	if !capabilities.ResponseBeforeTranslator || !capabilities.ResponseBeforeTranslatorOmitRequestBodies {
		t.Fatalf("response normalizer capabilities = %+v, want schema-8 opt-out", capabilities)
	}
	if !capabilities.ModelProvider || !capabilities.AuthProvider || !capabilities.Executor ||
		!capabilities.QuotaProvider || !capabilities.ManagementAPI {
		t.Fatalf("all five capabilities must be declared: %+v", capabilities)
	}
	// model.for_auth 只在 executor_model_scope 含 oauth/both 时才被宿主调用；
	// 声明错会让 realm 隔离静默失效。反过来，声明 both 会让宿主把 static 目录
	// 另外注册成一个 model-registry 客户端，与按凭证注册的客户端重复计数——
	// 删除凭证后模型停在 refcount 1（issue #58 的幽灵模型）。
	if capabilities.ExecutorModelScope != string(pluginapi.ExecutorModelScopeOAuth) {
		t.Fatalf("executor_model_scope = %q, want %q (models are auth-bound only)",
			capabilities.ExecutorModelScope, pluginapi.ExecutorModelScopeOAuth)
	}
	// 只声明 chat-completions：多协议由宿主翻译。
	if len(capabilities.ExecutorInputFormats) != 1 || capabilities.ExecutorInputFormats[0] != app.FormatChatCompletions {
		t.Fatalf("executor_input_formats = %v, want [%s]", capabilities.ExecutorInputFormats, app.FormatChatCompletions)
	}
}

// TestReconfigureKeepsBackgroundRunning 验证热更新返回完整注册响应且不重启后台。
func TestReconfigureKeepsBackgroundRunning(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)

	callMethod(t, pluginabi.MethodPluginRegister, app.LifecycleRequest{
		ConfigYAML:    buildConfigYAML(cfg),
		SchemaVersion: pluginabi.SchemaVersion,
	})
	if !testApp.BackgroundRunning() {
		t.Fatal("background work must start on register")
	}

	result := callMethod(t, pluginabi.MethodPluginReconfigure, app.LifecycleRequest{
		ConfigYAML:    buildConfigYAML(cfg),
		SchemaVersion: pluginabi.SchemaVersion,
	})
	var reg app.RegistrationInfo
	if errUnmarshal := json.Unmarshal(result, &reg); errUnmarshal != nil {
		t.Fatalf("decode reconfigure response: %v", errUnmarshal)
	}
	if reg.Metadata.Name == "" {
		t.Fatal("reconfigure must return the full registration shape")
	}
	if reg.SchemaVersion != pluginabi.SchemaVersionResponseBeforeOmitRequestBodies || !reg.Capabilities.ResponseBeforeTranslatorOmitRequestBodies {
		t.Fatal("reconfigure must preserve the negotiated response-before opt-out")
	}
	if !testApp.BackgroundRunning() {
		t.Fatal("reconfigure must not stop the background work")
	}
}

func TestRegisterLegacyHostKeepsSchema6AndRequestBodies(t *testing.T) {
	installFakeHost(t)
	cfg := setupTestPlugin(t)

	result := callMethod(t, pluginabi.MethodPluginRegister, app.LifecycleRequest{
		ConfigYAML:    buildConfigYAML(cfg),
		SchemaVersion: pluginabi.SchemaVersionRequestInterceptorOmitBody,
	})
	var reg app.RegistrationInfo
	if errUnmarshal := json.Unmarshal(result, &reg); errUnmarshal != nil {
		t.Fatalf("decode registration: %v", errUnmarshal)
	}
	if reg.SchemaVersion != pluginabi.SchemaVersionRawManagementResponse {
		t.Fatalf("legacy schema_version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersionRawManagementResponse)
	}
	if reg.Capabilities.ResponseBeforeTranslatorOmitRequestBodies {
		t.Fatal("legacy host must not receive the response request-body opt-out capability")
	}
}

// TestUnknownMethodReturnsEnvelopeNotError 验证未知方法以信封形式报错。
func TestUnknownMethodReturnsEnvelopeNotError(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	raw, errHandle := testApp.HandleMethod("no.such.method", nil)
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
		{"provider hint", "other.json", app.ProviderKey, `{"accessToken":"at"}`, true},
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
			if response.Auth.Provider != app.ProviderKey {
				t.Fatalf("provider = %q, want %q", response.Auth.Provider, app.ProviderKey)
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

// TestManagementAutoAllRouteDispatched 验证 /tasks/auto_all 路由分发成功且不报 404。
func TestManagementAutoAllRouteDispatched(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	raw, err := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(pluginapi.ManagementRequest{
		Method: "POST",
		Path:   "/v0/management/plugins/freetier2api/tasks/auto_all",
	}))
	if err != nil {
		t.Fatalf("handleMethod error: %v", err)
	}
	var env abiEnvelope
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
