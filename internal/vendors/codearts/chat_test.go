package codearts

import (
	"encoding/json"
	"testing"
)

// TestClampMaxTokens 锁定请求侧的输出上限夹取。
//
// 元数据侧把目录上限夹到 65536 只解决一半：客户端自己带 max_tokens 时仍会
// 触发上游平台级校验（65537 起整条请求 400）。这里覆盖两种键名、边界值，
// 以及「不该动」的输入。
func TestClampMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{name: "超限回夹", in: `{"model":"glm-5.2","max_tokens":131072}`, want: maxOutputTokenCap},
		{name: "completion 键同样夹", in: `{"max_completion_tokens":393216}`, want: maxOutputTokenCap},
		{name: "恰好在顶保留", in: `{"max_tokens":65536}`, want: 65536},
		{name: "未超限原样", in: `{"max_tokens":4096}`, want: 4096},
	}
	for _, tc := range cases {
		out := clampMaxTokens([]byte(tc.in))
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("%s: 输出不是合法 JSON: %v (%s)", tc.name, err, out)
		}
		got, ok := payload["max_tokens"]
		if !ok {
			got = payload["max_completion_tokens"]
		}
		num, ok := got.(float64)
		if !ok || int(num) != tc.want {
			t.Fatalf("%s: max_tokens = %v, want %d", tc.name, got, tc.want)
		}
	}
}

// TestClampMaxTokensPassesThroughUntouched 验证不该改写的情况原样放行。
func TestClampMaxTokensPassesThroughUntouched(t *testing.T) {
	for _, body := range []string{
		`not json at all`,
		`{"model":"glm-5.2"}`,
		`{"max_tokens":"131072"}`, // 非数字：不猜语义
		`{"max_tokens":4096}`,
	} {
		if got := string(clampMaxTokens([]byte(body))); got != body {
			t.Fatalf("body 应原样返回，实际 %q", got)
		}
	}
}
