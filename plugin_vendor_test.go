package main

// 跨供应商归属判定的回归测试。
//
// 这是本插件最关键的守卫：宿主把所有非内建格式的凭证依次喂给每个插件，
// 归属判定过宽会让宿主用本插件的结构覆盖对方账号（且是静默的）。
// 新增供应商时，这些用例保证「各家的凭证只被自家认领」。

import (
	"encoding/json"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
)

func TestAuthParseVendorOwnershipDiscrimination(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// 判据说明：这些用例**刻意不给 provider**（空串），模拟宿主还没把文件
	// 归属给任何插件时的判定。给 provider 会让 workbuddy 的
	// `provider == "freetier"` 兜底把一切都收下，测试就测不出判别力了。
	cases := []struct {
		name       string
		fileName   string
		provider   string
		body       string
		wantHandle bool
	}{
		// OpenCodeZEN：opencodezen 文件名 + api_key。
		{"opencodezen file name", "opencodezen-key1.json", "", `{"api_key":"sk-abc"}`, true},
		{"opencodezen zen_key field", "my-account.json", "", `{"zen_key":"sk-abc"}`, true},
		// Cline：cline 文件名 / workos 前缀 / 驼峰 refreshToken。
		{"cline file name", "cline-me@x.com.json", "", `{"accessToken":"at","refreshToken":"rt"}`, true},
		{"cline workos prefix", "acc.json", "", `{"access_token":"workos:at"}`, true},
		// 既有供应商：仍按各自约定认领。
		{"workbuddy nested", "workbuddycn-u1.json", "", `{"account":{"uid":"u1"},"auth":{"accessToken":"at","realm":"cn"}}`, true},
		{"qoder pt token", "qodercn-u1.json", "", `{"token":"pt-abc"}`, true},
		// 新增供应商：ZCode, TraeSOLO, Trae国内版/国际版, Tabbit, CodeArts
		{"zcode filename", "zcode-key1.json", "", `{"api_key":"abc.123"}`, true},
		{"traesolo filename", "traesolo-user1.json", "", `{"accessToken":"at_123","refreshToken":"rt_456"}`, true},
		{"traecn filename", "traecn-user1.json", "", `{"accessToken":"at_123"}`, true},
		{"traeglobal filename", "traeglobal-user1.json", "", `{"accessToken":"at_123","region":"global"}`, true},
		{"tabbit filename", "tabbit-user1.json", "", `{"api_key":"sk-tabbit-123"}`, true},
		{"codearts filename", "codearts-user1.json", "", `{"access_key_id":"ak","secret_access_key":"sk"}`, true},

		// 跨供应商误吞防护：通用结构必须落在**正确**的供应商头上。
		{"qoder pt token is qoder", "acc.json", "", `{"access_token":"pt-abc"}`, true},
		{"cline camel is cline", "acc.json", "", `{"accessToken":"at","refreshToken":"rt"}`, true},
		// 真正谁都不该认的：只含通用字段、没有专属特征。
		{"flat device token is nothing", "acc.json", "", `{"device_token":"dt-abc","account":{"uid":"u1"}}`, false},
		{"generic snake pair is nothing", "acc.json", "", `{"access_token":"at","refresh_token":"rt"}`, false},
		{"generic token is nothing", "acc.json", "", `{"token":"sk-abc"}`, false},
		{"generic access_token is nothing", "acc.json", "", `{"access_token":"at"}`, false},
		{"workbuddy provider hint overrides", "other.json", providerKey, `{"accessToken":"at"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
				Provider: tc.provider,
				FileName: tc.fileName,
				RawJSON:  []byte(tc.body),
			})
			var response pluginapi.AuthParseResponse
			if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
				t.Fatalf("decode parse response: %v", errUnmarshal)
			}
			if response.Handled != tc.wantHandle {
				t.Fatalf("handled = %v, want %v", response.Handled, tc.wantHandle)
			}
		})
	}
}
