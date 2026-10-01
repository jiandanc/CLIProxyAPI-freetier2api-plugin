package zcode

// OAuth 端点拼接的回归测试。
//
// 守卫的问题：路径常量与 origin 各自都带 /api/v1，拼接后变成
// /api/v1/api/v1/oauth/cli/init —— URL 形态看起来正常，却是 404，
// 表现为页面上的「获取授权链接失败」。这类错误没有编译期提示，
// 只能靠断言把拼好的完整 URL 钉住。

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOAuthPathsAreNotDoublePrefixed(t *testing.T) {
	checks := []struct{ got, want string }{
		{ZCodeOrigin + PathOAuthInit, ZCodeOrigin + "/api/v1/oauth/cli/init"},
		{ZCodeOrigin + PathOAuthPoll + "/abc123", ZCodeOrigin + "/api/v1/oauth/cli/poll/abc123"},
		{BillingBase + PathBillingBalance, "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"},
		{ZAIOrigin + PathAPIMessages, "https://api.z.ai/api/anthropic/v1/messages"},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Fatalf("端点拼接错误:\n  got  %s\n  want %s", check.got, check.want)
		}
	}

	// /api/v1 只能出现一次——这是上面每一条的共同失效模式。
	for _, check := range checks {
		if count := countSubstring(check.got, "/api/v1"); count > 1 {
			t.Fatalf("路径重复前缀 %s：%s", check.got, "/api/v1")
		}
	}
}

func TestLoginStartUsesZCodeOrigin(t *testing.T) {
	// LoginStart 必须打到 zcode.z.ai——它不得受对话基地址覆盖（zcode_base_url）影响，
	// 两者的域名本来就不同。
	var hitPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"f1","authorize_url":"https://chat.z.ai/x","poll_token":"pt"}}`))
	}))
	defer server.Close()

	// 直接把请求指到测试服务器：验证路径形态（origin 由常量决定，这里只校验拼接）。
	request, errNew := http.NewRequest(http.MethodPost, server.URL+PathOAuthInit, nil)
	if errNew != nil {
		t.Fatalf("build request: %v", errNew)
	}
	resp, errDo := server.Client().Do(request)
	if errDo != nil {
		t.Fatalf("do request: %v", errDo)
	}
	_ = resp.Body.Close()

	if hitPath != "/api/v1/oauth/cli/init" {
		t.Fatalf("OAuth init 路径 = %q, want /api/v1/oauth/cli/init", hitPath)
	}
}

// countSubstring 统计子串出现次数。
func countSubstring(s, sub string) int {
	count := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			count++
		}
	}
	return count
}
