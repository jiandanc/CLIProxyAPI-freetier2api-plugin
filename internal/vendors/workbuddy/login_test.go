package workbuddy

// 本文件测试登录的待授权判定。
//
// 上游用业务码 11217 + "login ing..." 表示「授权尚未完成」——这是轮询期间的
// 正常状态。早期实现把所有非 0 业务码都当失败，用户在浏览器里完成授权后
// 仍会看到"授权失败"，而实际上只差最后一次轮询。

import "testing"

// TestIsLoginPendingRecognizesUpstreamCode 验证 11217 被识别为待授权。
func TestIsLoginPendingRecognizesUpstreamCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"upstream 11217 login ing", &Error{
			Kind: KindClient, Status: 200,
			Msg: `11217 11217:login ing... {"code":11217,"msg":"11217:login ing..."}`,
		}, true},
		{"plain pending wording", &Error{Kind: KindClient, Status: 200, Msg: "authorization pending"}, true},
		{"waiting wording", &Error{Kind: KindClient, Status: 200, Msg: "waiting for user"}, true},
		{"not found means not yet authorized", &Error{Kind: KindNotFound, Status: 404, Msg: "not found"}, true},
		// 真失败不能被当成待授权，否则会一直轮询到超时。
		{"invalid state is fatal", &Error{Kind: KindClient, Status: 400, Msg: "invalid state"}, false},
		{"nil error", nil, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := IsLoginPending(testCase.err); got != testCase.want {
				t.Fatalf("IsLoginPending = %v, want %v", got, testCase.want)
			}
		})
	}
}
