package main

// 本文件测试 OAuth 登录的待授权判定。
//
// 上游用业务码 11217 + "login ing..." 表示「授权尚未完成」——这是轮询期间的
// 正常状态。早期实现把所有非 0 业务码都当失败，用户在浏览器里完成授权后
// 仍会看到"授权失败"，而实际上只差最后一次轮询。

import (
	"testing"

	"freetier2api-plugin/internal/vendors/workbuddy"
)

// TestIsLoginPendingRecognizesUpstreamCode 验证 11217 被识别为待授权。
func TestIsLoginPendingRecognizesUpstreamCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"upstream 11217 login ing", &workbuddy.Error{
			Kind: workbuddy.KindClient, Status: 200,
			Msg: `11217 11217:login ing... {"code":11217,"msg":"11217:login ing..."}`,
		}, true},
		{"plain pending wording", &workbuddy.Error{Kind: workbuddy.KindClient, Status: 200, Msg: "authorization pending"}, true},
		{"waiting wording", &workbuddy.Error{Kind: workbuddy.KindClient, Status: 200, Msg: "waiting for user"}, true},
		{"not found means not yet authorized", &workbuddy.Error{Kind: workbuddy.KindNotFound, Status: 404, Msg: "not found"}, true},
		// 真失败不能被当成待授权，否则会一直轮询到超时。
		{"invalid state is fatal", &workbuddy.Error{Kind: workbuddy.KindClient, Status: 400, Msg: "invalid state"}, false},
		{"nil error", nil, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isLoginPending(testCase.err); got != testCase.want {
				t.Fatalf("isLoginPending = %v, want %v", got, testCase.want)
			}
		})
	}
}
