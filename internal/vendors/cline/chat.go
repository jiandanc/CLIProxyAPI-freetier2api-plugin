package cline

// 本文件实现 Cline 的 chat-completions 转发。
//
// Cline 上游就是标准 OpenAI chat-completions，请求体**不需要改写**——
// 唯一需要动的是请求头（认证 + 客户端标识），以及补上上游要求的
// session_id 与 reasoning_effort 两个字段。
//
// 这两个字段是上游的硬要求（实测缺失会被拒），因此在转发前注入；
// 客户端已经给了值时不覆盖。

import (
	"bytes"
	"context"
	"encoding/json"
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
	// BearerToken 是可直接放进 Authorization 的令牌（含 workos: 前缀）。
	BearerToken string
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body []byte
	// Stream 报告是否流式。
	Stream bool
}

// Chat 执行一次非流式对话，返回上游的完整响应体。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	body, errPrepare := prepareBody(req.Body)
	if errPrepare != nil {
		return nil, errPrepare
	}
	resp, errDo := doChat(ctx, baseURL, req, body, chatHTTPTimeout)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	payload, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &Error{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: KindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(payload))
	}
	return payload, nil
}

// ChatStream 执行一次流式对话，返回上游响应体（调用方负责关闭）。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	body, errPrepare := prepareBody(req.Body)
	if errPrepare != nil {
		return nil, errPrepare
	}
	// 超时传 0：流式响应的总时长不可预期，一次性超时会把长回答截断。
	resp, errDo := doChat(ctx, baseURL, req, body, 0)
	if errDo != nil {
		return nil, errDo
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, string(payload))
	}
	return resp, nil
}

// doChat 发起一次对话请求。
func doChat(ctx context.Context, baseURL string, req ChatRequest, body []byte, timeout time.Duration) (*http.Response, error) {
	endpoint := BaseURL(baseURL) + ChatPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.BearerToken, NewSessionID())

	var client *http.Client
	if timeout > 0 {
		client = httpx.Client(ctx, timeout)
	} else {
		client = httpx.StreamClient(ctx, 0)
	}
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return nil, &Error{Msg: "chat: " + errDo.Error(), Kind: KindTransient}
	}
	return resp, nil
}

// prepareBody 注入上游要求的字段。
//
// 注入两个字段：
//   - session_id：上游用它做会话亲和（缺了会被当成新会话，影响缓存命中）；
//   - reasoning_effort：上游对推理档位的默认值有要求，缺失时部分模型拒绝。
//
// **不覆盖客户端已给的值**：用户显式指定了就要尊重，插件只补默认。
func prepareBody(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		// 解析不了就原样转发：上游会给出比我们更准确的错误。
		return raw, nil
	}
	changed := false
	if _, okSession := payload["session_id"]; !okSession {
		payload["session_id"] = NewSessionID()
		changed = true
	}
	if _, okEffort := payload["reasoning_effort"]; !okEffort {
		payload["reasoning_effort"] = "high"
		changed = true
	}
	if !changed {
		return raw, nil
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return raw, nil
	}
	return encoded, nil
}
