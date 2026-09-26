package httpx

// 本文件测试宿主 HTTP 桥的契约兼容性。
//
// 重点是 host.http.do 的响应键名：宿主把无 json tag 的 pluginapi.HTTPResponse
// 直接丢进信封，线上键名是 Go 字段名。只认一种键名会让状态码静默变 0，
// 后果是所有非流式上游请求全部被判成失败，而错误体里又带着真实响应，
// 看起来像「凭证被拒」——极难排查。因此两种形态都必须能解。

import (
	"encoding/json"
	"testing"
)

// TestHostHTTPResponseAcceptsGoFieldNames 验证宿主主路径的键名形态。
func TestHostHTTPResponseAcceptsGoFieldNames(t *testing.T) {
	raw := []byte(`{"StatusCode":200,"Headers":{"Content-Type":["application/json"]},"Body":"aGVsbG8="}`)
	var response rpcHostHTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if !response.statusSeen {
		t.Fatal("StatusCode present but reported missing")
	}
	if response.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", response.StatusCode)
	}
	if string(response.Body) != "hello" {
		t.Fatalf("Body = %q, want hello", response.Body)
	}
}

// TestHostHTTPResponseAcceptsSnakeCase 验证带 tag 的中间形态也能解。
func TestHostHTTPResponseAcceptsSnakeCase(t *testing.T) {
	raw := []byte(`{"status_code":429,"headers":{"Retry-After":["30"]},"body":"cmF0ZQ=="}`)
	var response rpcHostHTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if response.StatusCode != 429 {
		t.Fatalf("StatusCode = %d, want 429", response.StatusCode)
	}
	if response.Headers.Get("Retry-After") != "30" {
		t.Fatalf("headers not decoded: %v", response.Headers)
	}
}

// TestHostHTTPResponseFlagsMissingStatus 验证缺状态码被显式标记。
//
// 不能静默当 0：那会让上游 200 也被判成失败。
func TestHostHTTPResponseFlagsMissingStatus(t *testing.T) {
	var response rpcHostHTTPResponse
	if errUnmarshal := json.Unmarshal([]byte(`{"headers":{},"body":""}`), &response); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if response.statusSeen {
		t.Fatal("absent status field must be flagged so the caller can report an ABI mismatch")
	}
}

// TestHostHTTPStreamResponseUsesSnakeCase 验证流式响应的键名（这个结构体有 tag）。
func TestHostHTTPStreamResponseUsesSnakeCase(t *testing.T) {
	raw := []byte(`{"status_code":200,"stream_id":"s1","headers":{"X-A":["b"]}}`)
	var response rpcHostHTTPStreamResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if response.StatusCode != 200 || response.StreamID != "s1" {
		t.Fatalf("stream response = %+v", response)
	}
}

// TestWithCallbackID 验证回调 ID 的绑定与读取。
func TestWithCallbackID(t *testing.T) {
	if got := callbackIDFrom(WithCallbackID(nil, "cb-1")); got != "cb-1" {
		t.Fatalf("callback id = %q, want cb-1", got)
	}
	// 空 ID 不写入 context（保持零值语义）。
	if got := callbackIDFrom(WithCallbackID(nil, "   ")); got != "" {
		t.Fatalf("blank callback id must not be stored, got %q", got)
	}
}

// TestConfigureIsRequired 验证未注入宿主回调时明确报错而不是静默失败。
func TestConfigureIsRequired(t *testing.T) {
	previous, _ := currentHostCaller()
	Configure(nil)
	if _, errCaller := currentHostCaller(); errCaller == nil {
		t.Fatal("unconfigured host caller must return an error")
	}
	Configure(previous)
}
