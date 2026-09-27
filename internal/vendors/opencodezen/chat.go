package opencodezen

// 本文件实现 OpenCode ZEN 的 chat-completions 转发。
//
// 与 WorkBuddy / Qoder 的差异：ZEN 上游就是标准 OpenAI chat-completions，
// 请求体**不需要任何改写**——没有提示词注入、没有指纹脱敏、没有工具重排。
// 因此这里只做三件事：注入认证头、发请求、把响应原样交回宿主。
//
// 不做改写的另一个理由：ZEN 上游对请求形态有校验（免费层要求 agent 形态的
// 流式请求），插件若自作主张改 body，反而会把能用的请求改坏。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"freetier2api-plugin/internal/httpx"
)

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	// APIKey 是出站凭证。
	APIKey string
	// Body 是宿主翻译好的 chat-completions 请求体（原样转发）。
	Body []byte
	// Stream 报告是否流式。
	Stream bool
}

// Chat 执行一次非流式对话，返回上游的完整响应体。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	resp, errDo := doChat(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &Error{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: KindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	return body, nil
}

// ChatStream 执行一次流式对话，返回上游响应体（调用方负责关闭）。
//
// 用 StreamClient：流式响应的总时长不可预期，一次性读取会把长回答截断。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := BaseURL(baseURL) + ChatPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytesReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat stream request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.APIKey, newSessionID(), newRequestID())

	resp, errDo := httpx.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, &Error{Msg: "chat stream: " + errDo.Error(), Kind: KindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, string(body))
	}
	return resp, nil
}

// doChat 发起一次非流式对话请求。
func doChat(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := BaseURL(baseURL) + ChatPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytesReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.APIKey, newSessionID(), newRequestID())

	resp, errDo := httpx.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, &Error{Msg: "chat: " + errDo.Error(), Kind: KindTransient}
	}
	return resp, nil
}

// newSessionID 生成一个符合上游期望形态的会话 ID。
//
// 上游对会话 ID 的形状有校验（免费层尤其严格，非规范形态会被 403 拒绝）。
// 规范形态是 ses_ + 12 位小写十六进制 + 14 位 Base62，这里按同样形状生成。
func newSessionID() string {
	return "ses_" + randomHex(6) + randomBase62(14)
}

// newRequestID 生成一个请求 ID。
func newRequestID() string {
	return "msg" + randomBase62(26)
}

// randomHex 生成 n 字节的十六进制串（2n 个字符）。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, errRead := rand.Read(buf); errRead != nil {
		// 随机源不可用时退回全零：形状仍然合法，只是不再唯一。
		// 这比 panic 好——会话 ID 只影响上游的亲和性，不影响正确性。
		return string(make([]byte, n*2))
	}
	return hex.EncodeToString(buf)
}

// base62Alphabet 是 Base62 字符集（与上游会话 ID 的字符集一致）。
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// randomBase62 生成 n 个 Base62 字符。
func randomBase62(n int) string {
	buf := make([]byte, n)
	random := make([]byte, n)
	if _, errRead := rand.Read(random); errRead != nil {
		return string(make([]byte, n))
	}
	for i, value := range random {
		buf[i] = base62Alphabet[int(value)%len(base62Alphabet)]
	}
	return string(buf)
}

// bytesReader 把字节切片包成 io.Reader。
//
// 每次请求都新建一个 Reader：http.NewRequestWithContext 会把它当成请求体
// 一次性读走，同一个 Reader 不能跨请求复用。
func bytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}
