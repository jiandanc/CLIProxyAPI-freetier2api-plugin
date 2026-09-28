package tabbit

// 本文件实现 Tabbit 对话请求转发与流式透传。
// 参考：/Users/jiandan/Workspaces/tabbit2api/src/openai-chat.js

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/httpx"
)

type ChatRequest struct {
	Credential *Credential
	Body       []byte
	Stream     bool
}

// Chat 执行一次非流式对话。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	resp, errDo := doTabbitRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read tabbit response: %w", errRead)
	}

	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}

	return body, nil
}

// ChatStream 执行一次流式对话。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	resp, errDo := doTabbitRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, Classify(resp.StatusCode, string(body))
	}

	return resp, nil
}

func doTabbitRequest(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	if req.Credential == nil {
		return nil, fmt.Errorf("tabbit credential is nil")
	}

	target := strings.TrimRight(baseURL, "/")
	if target == "" {
		if req.Credential.BaseURL != "" {
			target = strings.TrimRight(req.Credential.BaseURL, "/")
		} else {
			target = DefaultBaseURL
		}
	}

	endpoint := target + EpChatCompletions
	if !strings.HasSuffix(target, "/v1") && !strings.Contains(target, "/proxy/v1") {
		endpoint = target + "/v1" + EpChatCompletions
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("create tabbit http request: %w", errReq)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}

	token := firstNonEmpty(req.Credential.APIKey, req.Credential.SessionToken)
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	timeout := 120 * time.Second
	if req.Stream {
		timeout = 300 * time.Second
	}

	client := httpx.Client(ctx, timeout)
	resp, errCall := client.Do(httpReq)
	if errCall != nil {
		return nil, fmt.Errorf("tabbit request to %s failed: %w", endpoint, errCall)
	}

	return resp, nil
}
