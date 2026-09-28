package trae

// 本文件实现 Trae 对话转发：请求体改写、头装配与 SSE 事件转换。
// 参考：/Users/jiandan/Workspaces/trae2api/src/trae-client.js

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

type StreamChunkHandler func(chunk []byte) error

func ApplyTraeHeaders(req *http.Request, cred *Credential, cfg Config, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", "Trae/"+cfg.IDEVersion)
	at := cred.AccessToken
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if cred.UID != "" {
		req.Header.Set("X-Uid", cred.UID)
	}
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-Ide-Version", cfg.IDEVersion)
	req.Header.Set("X-Device-Type", "windows")
	if cred.MachineID != "" {
		req.Header.Set("X-Machine-Id", cred.MachineID)
	}
	if cred.DeviceID != "" {
		req.Header.Set("X-Device-Id", cred.DeviceID)
	}
}

// PrepareBody 改写请求体为 Trae 格式。
func PrepareBody(src []byte, defaultModel string) ([]byte, string, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src, defaultModel, err
	}

	model, _ := obj["model"].(string)
	if model == "" {
		model = defaultModel
	}

	obj["stream"] = true
	obj["function"] = "chat_v3"
	obj["config_name"] = model
	obj["model"] = model

	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			content, present := m["content"]
			if present {
				if s, ok := content.(string); ok {
					m["content"] = []any{map[string]any{"type": "text", "text": s}}
				}
			}
		}
	}

	data, err := json.Marshal(obj)
	return data, model, err
}

// Chat 执行一次非流式对话。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	respBody, model, errDo := doTraeRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = respBody.Close() }()

	var (
		contentSB   strings.Builder
		reasoningSB strings.Builder
		usage       map[string]any
	)

	scanner := bufio.NewScanner(respBody)
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}

		var payload map[string]any
		_ = json.Unmarshal([]byte(data), &payload)

		switch currentEvent {
		case "output":
			if r, ok := payload["response"].(string); ok {
				contentSB.WriteString(r)
			}
			if rc, ok := payload["reasoning_content"].(string); ok {
				reasoningSB.WriteString(rc)
			}
		case "token_usage":
			usage = payload
		case "error":
			code, _ := payload["code"].(float64)
			msg, _ := payload["message"].(string)
			return nil, fmt.Errorf("trae upstream error: code=%.0f msg=%s", code, msg)
		}
	}

	out := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           contentSB.String(),
					"reasoning_content": reasoningSB.String(),
				},
				"finish_reason": "stop",
			},
		},
	}
	if usage != nil {
		out["usage"] = usage
	}

	return json.Marshal(out)
}

// ChatStream 执行一次流式对话。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest, onChunk StreamChunkHandler) error {
	respBody, model, errDo := doTraeRequest(ctx, baseURL, req)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = respBody.Close() }()

	scanner := bufio.NewScanner(respBody)
	var currentEvent string
	chunkID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}

		switch currentEvent {
		case "output":
			resp, _ := payload["response"].(string)
			reasoning, _ := payload["reasoning_content"].(string)
			if resp != "" || reasoning != "" {
				delta := map[string]any{}
				if resp != "" {
					delta["content"] = resp
				}
				if reasoning != "" {
					delta["reasoning_content"] = reasoning
				}
				chunk := map[string]any{
					"id":      chunkID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []any{
						map[string]any{
							"index":         0,
							"delta":         delta,
							"finish_reason": nil,
						},
					},
				}
				rawChunk, _ := json.Marshal(chunk)
				if err := onChunk(rawChunk); err != nil {
					return err
				}
			}
		case "done":
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
		case "error":
			code, _ := payload["code"].(float64)
			msg, _ := payload["message"].(string)
			return fmt.Errorf("trae stream error: code=%.0f msg=%s", code, msg)
		}
	}

	return scanner.Err()
}

func doTraeRequest(ctx context.Context, baseURL string, req ChatRequest) (io.ReadCloser, string, error) {
	if req.Credential == nil {
		return nil, "", fmt.Errorf("trae credential is nil")
	}

	cfg := ConfigFor(req.Credential.Region)
	defaultModel := "glm-5.2"
	if req.Credential.Region == RegionGlobal {
		defaultModel = "gpt-5"
	}

	body, model, errPrep := PrepareBody(req.Body, defaultModel)
	if errPrep != nil {
		return nil, "", errPrep
	}

	targetHost := strings.TrimRight(baseURL, "/")
	if targetHost == "" {
		targetHost = cfg.ChatHost
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, targetHost+EpChat, bytes.NewReader(body))
	if errReq != nil {
		return nil, "", errReq
	}
	ApplyTraeHeaders(httpReq, req.Credential, cfg, true)

	client := httpx.Client(ctx, 300*time.Second)
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return nil, "", fmt.Errorf("trae chat request failed: %w", errDo)
	}

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, "", Classify(resp.StatusCode, string(raw))
	}

	return resp.Body, model, nil
}
