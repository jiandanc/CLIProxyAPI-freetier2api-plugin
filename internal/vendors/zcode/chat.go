package zcode

// 本文件实现 ZCode 对话转发：把 OpenAI chat-completions 翻译为上游 Anthropic
// messages 协议，并把响应转回 OpenAI 格式。
// 参考：/Users/jiandan/Workspaces/zcode2api/app/openai_compat.py 与 gateway.py

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// StreamChunkHandler 是流式传输中接收每个 OpenAI chunk 的回调。
type StreamChunkHandler func(chunk []byte) error

// Chat 执行一次非流式对话。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	httpReq, model, errBuild := buildHTTPRequest(ctx, baseURL, req)
	if errBuild != nil {
		return nil, errBuild
	}

	client := httpx.Client(ctx, 120*time.Second)
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return nil, fmt.Errorf("zcode chat request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read zcode response: %w", errRead)
	}

	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}

	return anthropicToOpenAICompletion(body, model)
}

// ChatStream 执行一次流式对话。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest, onChunk StreamChunkHandler) error {
	httpReq, model, errBuild := buildHTTPRequest(ctx, baseURL, req)
	if errBuild != nil {
		return errBuild
	}

	client := httpx.Client(ctx, 300*time.Second)
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return fmt.Errorf("zcode stream request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Classify(resp.StatusCode, string(body))
	}

	return parseAnthropicSSEToOpenAIChunks(resp.Body, model, onChunk)
}

func buildHTTPRequest(ctx context.Context, baseURL string, req ChatRequest) (*http.Request, string, error) {
	if req.Credential == nil {
		return nil, "", fmt.Errorf("zcode credential is nil")
	}

	anthropicBody, model, errConv := openAIToAnthropicBody(req.Body, req.Stream)
	if errConv != nil {
		return nil, "", fmt.Errorf("convert openai request: %w", errConv)
	}

	targetURL := resolveTargetURL(baseURL, req.Credential)

	httpReq, errNew := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(anthropicBody))
	if errNew != nil {
		return nil, "", fmt.Errorf("create http request: %w", errNew)
	}

	applyHeaders(httpReq, req.Credential)
	return httpReq, model, nil
}

func resolveTargetURL(baseURL string, cred *Credential) string {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		if cred.APIKey != "" {
			base = DefaultAPIBase
		} else {
			base = DefaultZCodeOrigin
		}
	}

	if cred.APIKey != "" {
		return base + PathAPIMessages
	}
	return base + PathPlanMessages
}

func applyHeaders(req *http.Request, cred *Credential) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("User-Agent", ClientUA)
	req.Header.Set("X-ZCode-App-Version", ClientAppVersion)
	req.Header.Set("X-ZCode-Agent", "glm")
	req.Header.Set("HTTP-Referer", DefaultZCodeOrigin+"/")
	req.Header.Set("anthropic-version", AnthropicVersion)

	if cred.APIKey != "" {
		req.Header.Set("x-api-key", cred.APIKey)
	} else if cred.JWTToken != "" {
		req.Header.Set("Authorization", "Bearer "+cred.JWTToken)
	}
}

// openAIToAnthropicBody 把 OpenAI 请求体转换为 Anthropic messages 格式。
func openAIToAnthropicBody(src []byte, stream bool) ([]byte, string, error) {
	var in struct {
		Model       string   `json:"model"`
		Messages    []any    `json:"messages"`
		MaxTokens   int      `json:"max_tokens"`
		Temperature *float64 `json:"temperature,omitempty"`
		TopP        *float64 `json:"top_p,omitempty"`
		Stream      bool     `json:"stream"`
	}
	if err := json.Unmarshal(src, &in); err != nil {
		return nil, "", err
	}

	model := NormalizeModelName(in.Model)
	if model == "" {
		model = "GLM-5.3"
	}

	maxTokens := in.MaxTokens
	if maxTokens <= 0 || maxTokens > MaxTokensLimit {
		maxTokens = 8192
	}

	var systemParts []string
	var outMessages []map[string]any

	for _, mi := range in.Messages {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		content := m["content"]

		if role == "system" {
			if s, ok := content.(string); ok && strings.TrimSpace(s) != "" {
				systemParts = append(systemParts, s)
			}
			continue
		}

		if role == "developer" {
			if s, ok := content.(string); ok && strings.TrimSpace(s) != "" {
				systemParts = append(systemParts, s)
			}
			continue
		}

		outMessages = append(outMessages, map[string]any{
			"role":    role,
			"content": content,
		})
	}

	out := map[string]any{
		"model":      model,
		"messages":   outMessages,
		"max_tokens": maxTokens,
		"stream":     stream,
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}

	data, err := json.Marshal(out)
	return data, model, err
}

func anthropicToOpenAICompletion(raw []byte, model string) ([]byte, error) {
	var resp struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	var sb strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}

	out := map[string]any{
		"id":      firstNonEmpty(resp.ID, fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": sb.String(),
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}

	return json.Marshal(out)
}

func parseAnthropicSSEToOpenAIChunks(r io.Reader, model string, onChunk StreamChunkHandler) error {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	chunkID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if dataStr == "" || dataStr == "[DONE]" {
			continue
		}

		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(dataStr), &event); err != nil {
			continue
		}

		if event.Type == "content_block_delta" && event.Delta.Text != "" {
			chunk := map[string]any{
				"id":      chunkID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []any{
					map[string]any{
						"index": 0,
						"delta": map[string]any{
							"content": event.Delta.Text,
						},
						"finish_reason": nil,
					},
				},
			}
			rawChunk, _ := json.Marshal(chunk)
			if err := onChunk(rawChunk); err != nil {
				return err
			}
		} else if event.Type == "message_stop" {
			chunk := map[string]any{
				"id":      chunkID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []any{
					map[string]any{
						"index":         0,
						"delta":         map[string]any{},
						"finish_reason": "stop",
					},
				},
			}
			rawChunk, _ := json.Marshal(chunk)
			_ = onChunk(rawChunk)
		}
	}

	return scanner.Err()
}
