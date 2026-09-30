package opencodezen

// 本文件实现 OpenCode ZEN 的 chat-completions 转发。
//
// 与 WorkBuddy / Qoder 的差异：ZEN 上游是标准 OpenAI chat-completions。
// 特殊要求（参考 opencode2api 与 OpenCode 官方实现）：
//   1. 会话 ID 必须是 canonical session 格式：`ses_` + 12位十六进制 + 14位 Base62。
//   2. 免费层模型（ID 包含 "free"）要求完整的 Agent 请求形态：强制使用 stream=true 流式
//      以及携带核心工具声明（bash, edit, glob, grep, read）。
//      非流式调用在插件内会自动开启 stream 并在上游返回后通过 CollapseSSEToJSON 合并为标准 JSON 响应。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"freetier2api-plugin/internal/httpx"
)

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	// APIKey 是出站凭证。
	APIKey string
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body []byte
	// Stream 报告是否流式。
	Stream bool
}

// Chat 执行一次非流式对话，返回上游的完整响应体。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	modelName := extractModelName(req.Body)
	isFree := isFreeTierModel(modelName)

	if isFree {
		// 免费层强制以流式发送，再将 SSE 聚合回完整 JSON 文档
		streamReq := req
		streamReq.Stream = true
		streamReq.Body = prepareFreeTierBody(req.Body)

		resp, errStream := ChatStream(ctx, baseURL, streamReq)
		if errStream != nil {
			return nil, errStream
		}
		defer func() { _ = resp.Body.Close() }()

		return CollapseSSEToJSON(resp.Body, modelName)
	}

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
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := BaseURL(baseURL) + ChatPath
	body := req.Body
	if isFreeTierModel(extractModelName(body)) {
		body = prepareFreeTierBody(body)
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytesReader(body))
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
	body := req.Body
	if isFreeTierModel(extractModelName(body)) {
		body = prepareFreeTierBody(body)
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytesReader(body))
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

// extractModelName 从请求体中提取 model 字符串。
func extractModelName(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model
}

// isFreeTierModel 判断是否为 free 免费层模型。
func isFreeTierModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "free")
}

// coreAgentTools 是 OpenCode 官方客户端默认包含的 Agent 工具集。
var coreAgentTools = []string{"bash", "edit", "glob", "grep", "read"}

// prepareFreeTierBody 规范化免费层请求体（开启 stream，补充 core agent tools）。
func prepareFreeTierBody(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	payload["stream"] = true

	// 保证 stream_options.include_usage = true
	if opt, ok := payload["stream_options"].(map[string]any); ok {
		opt["include_usage"] = true
	} else {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	// 补充缺失的 Agent 核心工具定义
	existingTools := make(map[string]bool)
	if rawTools, ok := payload["tools"].([]any); ok {
		for _, item := range rawTools {
			if toolMap, okTool := item.(map[string]any); okTool {
				if fnMap, okFn := toolMap["function"].(map[string]any); okFn {
					if name, okName := fnMap["name"].(string); okName {
						existingTools[name] = true
					}
				}
			}
		}
	}

	var toolsToAppend []any
	for _, name := range coreAgentTools {
		if !existingTools[name] {
			toolsToAppend = append(toolsToAppend, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": "Agent tool " + name,
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			})
		}
	}

	if len(toolsToAppend) > 0 {
		if currentList, ok := payload["tools"].([]any); ok {
			payload["tools"] = append(currentList, toolsToAppend...)
		} else {
			payload["tools"] = toolsToAppend
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// chatCompletionResponse 用于将 SSE 流合并为一个完整的 chat.completion JSON。
type chatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type chatMessage struct {
	Role             string `json:"role"`
	Content          any    `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	ToolCalls        []any  `json:"tool_calls,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type sseDeltaChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []any  `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

// CollapseSSEToJSON 将上游 SSE 流消费并组装成一个完整的 OpenAI 格式 chat.completion JSON 响应。
func CollapseSSEToJSON(r io.Reader, defaultModel string) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	resp := chatCompletionResponse{
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   defaultModel,
		Choices: []chatChoice{
			{
				Index: 0,
				Message: chatMessage{
					Role: "assistant",
				},
				FinishReason: "stop",
			},
		},
	}

	var contentBuilder strings.Builder
	var reasoningBuilder strings.Builder
	var toolCalls []any

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk sseDeltaChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.ID != "" {
			resp.ID = chunk.ID
		}
		if chunk.Model != "" {
			resp.Model = chunk.Model
		}
		if chunk.Created > 0 {
			resp.Created = chunk.Created
		}
		if chunk.Usage != nil {
			resp.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				contentBuilder.WriteString(choice.Delta.Content)
			}
			if choice.Delta.Reasoning != "" {
				reasoningBuilder.WriteString(choice.Delta.Reasoning)
			} else if choice.Delta.ReasoningContent != "" {
				reasoningBuilder.WriteString(choice.Delta.ReasoningContent)
			}
			if len(choice.Delta.ToolCalls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, choice.Delta.ToolCalls)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				resp.Choices[0].FinishReason = *choice.FinishReason
			}
		}
	}

	contentStr := contentBuilder.String()
	resp.Choices[0].Message.Content = contentStr
	resp.Choices[0].Message.ReasoningContent = reasoningBuilder.String()

	toolCalls = filterValidToolCalls(toolCalls)
	if len(toolCalls) > 0 {
		resp.Choices[0].Message.ToolCalls = toolCalls
		resp.Choices[0].FinishReason = "tool_calls"
		if contentStr == "" {
			resp.Choices[0].Message.Content = nil
		}
	}

	return json.Marshal(resp)
}

func mergeToolCalls(accumulated []any, incoming []any) []any {
	for _, raw := range incoming {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		idx := intFromAny(call["index"])
		var target map[string]any
		for _, item := range accumulated {
			if existing, okMap := item.(map[string]any); okMap {
				if intFromAny(existing["index"]) == idx {
					target = existing
					break
				}
			}
		}
		if target == nil {
			target = cloneToolCall(call)
			target["index"] = idx
			accumulated = append(accumulated, target)
			continue
		}
		if id, okID := call["id"].(string); okID && id != "" {
			target["id"] = id
		}
		if tp, okTp := call["type"].(string); okTp && tp != "" {
			target["type"] = tp
		}
		if fnIncoming, okFn := call["function"].(map[string]any); okFn {
			fnTarget, _ := target["function"].(map[string]any)
			if fnTarget == nil {
				fnTarget = map[string]any{}
				target["function"] = fnTarget
			}
			if name, okName := fnIncoming["name"].(string); okName && name != "" {
				fnTarget["name"] = name
			}
			if args, okArgs := fnIncoming["arguments"].(string); okArgs && args != "" {
				prevArgs, _ := fnTarget["arguments"].(string)
				fnTarget["arguments"] = prevArgs + args
			}
		}
	}
	return accumulated
}

func filterValidToolCalls(calls []any) []any {
	var valid []any
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, okFn := call["function"].(map[string]any)
		if !okFn {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		valid = append(valid, call)
	}
	for i := 1; i < len(valid); i++ {
		for j := i; j > 0; j-- {
			left, _ := valid[j-1].(map[string]any)
			right, _ := valid[j].(map[string]any)
			if intFromAny(left["index"]) > intFromAny(right["index"]) {
				valid[j-1], valid[j] = valid[j], valid[j-1]
			}
		}
	}
	return valid
}

func cloneToolCall(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		if k == "function" {
			if fn, ok := v.(map[string]any); ok {
				fnCopy := make(map[string]any, len(fn))
				for fk, fv := range fn {
					fnCopy[fk] = fv
				}
				dst[k] = fnCopy
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// newSessionID 生成一个符合上游 OpenCode 官方规范的 Canonical Session ID。
// 格式为：ses_ + 12位小写十六进制 + 14位 Base62 字符。
func newSessionID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "ses_00000000000000000000000000"
	}
	timePart := hex.EncodeToString(buf[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(buf[6:16]), 14)
	return "ses_" + timePart + randomPart
}

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	remainder := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out)
}

// newRequestID 生成一个请求 ID。
func newRequestID() string {
	return "req_" + randomHex(8)
}

// randomHex 生成 n 字节的十六进制串（2n 个字符）。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, errRead := rand.Read(buf); errRead != nil {
		return string(make([]byte, n*2))
	}
	return hex.EncodeToString(buf)
}

// base62Alphabet 是 Base62 字符集。
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// bytesReader 把字节切片包成 io.Reader。
func bytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}
