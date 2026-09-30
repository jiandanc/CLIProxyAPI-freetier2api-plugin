package minimaxcode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
)

var (
	contentKeys  = []string{"msg_content", "msgContent", "content", "text", "delta", "answer"}
	thinkingKeys = []string{
		"reasoning_content", "thinking_content", "reason_content",
		"think_content", "reasoning", "thinking",
	}
)

// ChatRequest 内部解析结构。
type chatPayload struct {
	Model    string `json:"model"`
	Messages []any  `json:"messages"`
	Stream   bool   `json:"stream"`
}

// Chat 执行一次非流式对话。
func Chat(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string) ([]byte, error) {
	respBody, modelName, promptTokens, errDo := doMessageRequest(ctx, cred, req, baseURLOverride)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = respBody.Close() }()

	var contentSB strings.Builder
	var reasoningSB strings.Builder
	finishReason := "stop"

	scanner := bufio.NewScanner(respBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var payload map[string]any
		if errJSON := json.Unmarshal([]byte(data), &payload); errJSON != nil {
			continue
		}

		if errMsg := extractUpstreamError(payload); errMsg != "" {
			return nil, fmt.Errorf("minimax upstream error: %s", errMsg)
		}

		thinking := findDeepString(payload, thinkingKeys...)
		if thinking != "" {
			reasoningSB.WriteString(thinking)
		}

		text := findDeepString(payload, contentKeys...)
		if text != "" {
			contentSB.WriteString(text)
		}

		if fr := findDeepString(payload, "finish_reason", "finishReason"); fr != "" {
			finishReason = fr
		}
	}

	if errScan := scanner.Err(); errScan != nil && errScan != io.EOF {
		return nil, fmt.Errorf("read stream: %w", errScan)
	}

	contentStr := contentSB.String()
	reasoningStr := reasoningSB.String()
	if contentStr == "" && reasoningStr == "" {
		return nil, fmt.Errorf("minimax upstream returned empty response")
	}

	completionTokens := EstimateTokens(contentStr + reasoningStr)

	msgMap := map[string]any{
		"role":    "assistant",
		"content": contentStr,
	}
	if reasoningStr != "" {
		msgMap["reasoning_content"] = reasoningStr
	}

	out := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msgMap,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}

	return json.Marshal(out)
}

// ChatStream 执行一次流式对话，分片通过 onChunk 回调发射。
func ChatStream(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string, onChunk func([]byte) error) error {
	respBody, modelName, _, errDo := doMessageRequest(ctx, cred, req, baseURLOverride)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = respBody.Close() }()

	scanner := bufio.NewScanner(respBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	chunkID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var payload map[string]any
		if errJSON := json.Unmarshal([]byte(data), &payload); errJSON != nil {
			continue
		}

		if errMsg := extractUpstreamError(payload); errMsg != "" {
			return fmt.Errorf("minimax upstream error: %s", errMsg)
		}

		thinking := findDeepString(payload, thinkingKeys...)
		if thinking != "" {
			delta := map[string]any{"reasoning_content": thinking}
			chunk := map[string]any{
				"id":      chunkID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   modelName,
				"choices": []any{
					map[string]any{
						"index":         0,
						"delta":         delta,
						"finish_reason": nil,
					},
				},
			}
			chunkBytes, _ := json.Marshal(chunk)
			if errEmit := onChunk(chunkBytes); errEmit != nil {
				return errEmit
			}
		}

		text := findDeepString(payload, contentKeys...)
		if text != "" {
			delta := map[string]any{"content": text}
			chunk := map[string]any{
				"id":      chunkID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   modelName,
				"choices": []any{
					map[string]any{
						"index":         0,
						"delta":         delta,
						"finish_reason": nil,
					},
				},
			}
			chunkBytes, _ := json.Marshal(chunk)
			if errEmit := onChunk(chunkBytes); errEmit != nil {
				return errEmit
			}
		}
	}

	if errScan := scanner.Err(); errScan != nil && errScan != io.EOF {
		return fmt.Errorf("read stream: %w", errScan)
	}

	// 最终结束帧
	finalChunk := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
	}
	finalBytes, _ := json.Marshal(finalChunk)
	return onChunk(finalBytes)
}

func doMessageRequest(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string) (io.ReadCloser, string, int, error) {
	var cp chatPayload
	if len(req.Payload) > 0 {
		_ = json.Unmarshal(req.Payload, &cp)
	}

	modelID := req.Model
	if modelID == "" {
		modelID = cp.Model
	}
	modelCfg, _ := FindModel(modelID)

	var turns []*Turn
	for _, m := range cp.Messages {
		if t := ParseTurn(m); t != nil {
			turns = append(turns, t)
		}
	}

	prompt := BuildPrompt(turns)
	if prompt == "" {
		return nil, "", 0, fmt.Errorf("empty prompt")
	}
	promptTokens := EstimateTokens(prompt)

	sessionID, errSession := createSession(ctx, cred, baseURLOverride)
	if errSession != nil {
		return nil, "", 0, fmt.Errorf("create minimax session: %w", errSession)
	}

	bodyMap := map[string]any{
		"content":      prompt,
		"turn_id":      RandomUUID(),
		"worktreeMode": false,
	}
	if modelCfg.UpstreamModel != "" {
		sel := map[string]any{
			"model_id":    modelCfg.UpstreamModel,
			"provider_id": "minimax",
		}
		if modelCfg.Variant != "" {
			sel["variant"] = modelCfg.Variant
		}
		bodyMap["model"] = sel
	}

	bodyBytes, _ := json.Marshal(bodyMap)

	streamBase := StreamHostFor(cred.Region, baseURLOverride)
	path := fmt.Sprintf(EpMessage, sessionID)
	target := buildAgentTarget(cred, streamBase, path, http.MethodPost, string(bodyBytes), true)

	resp, errDo := doRequest(ctx, cred, target, streamBase, 120*time.Second)
	if errDo != nil {
		return nil, "", 0, fmt.Errorf("send message request: %w", errDo)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_ = resp.Body.Close()
		return nil, "", 0, core.NewPluginError("minimax_unauthorized", fmt.Sprintf("minimax HTTP %d", resp.StatusCode), http.StatusUnauthorized)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, "", 0, fmt.Errorf("minimax HTTP %d: %s", resp.StatusCode, string(raw))
	}

	return resp.Body, modelCfg.ID, promptTokens, nil
}

func extractUpstreamError(payload map[string]any) string {
	if errObj, ok := payload["error"].(map[string]any); ok {
		if msg := getString(errObj, "message"); msg != "" {
			return msg
		}
	}
	if errStr, ok := payload["error"].(string); ok && errStr != "" {
		return errStr
	}
	for _, key := range []string{"base_resp", "statusInfo"} {
		if node, ok := payload[key].(map[string]any); ok {
			code := getInt64(node, "status_code", "code")
			if code != 0 {
				msg := firstNonEmpty(getString(node, "status_msg"), getString(node, "message"))
				return fmt.Sprintf("status %d: %s", code, msg)
			}
		}
	}
	for _, key := range []string{"error_msg", "error_message", "err_msg", "status_msg"} {
		if msg := getString(payload, key); msg != "" && !strings.EqualFold(msg, "success") && !strings.EqualFold(msg, "ok") {
			return msg
		}
	}
	return ""
}
