package traesolo

// 本文件实现 TRAE SOLO 对话转发：请求体改写、头装配与 SSE 事件转换。
// 参考：/Users/jiandan/Workspaces/trae2api-more/internal/upstream/

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

func ApplySOLOHeaders(req *http.Request, cred *Credential, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", ClientUA)
	at := cred.AccessToken
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if cred.UID != "" {
		req.Header.Set("X-Uid", cred.UID)
	}
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", IdeVersion)
	req.Header.Set("X-Ide-Version-Code", IdeVersionCode)
	req.Header.Set("X-App-Version-Code", IdeVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if cred.MachineID != "" {
		req.Header.Set("X-Machine-Id", cred.MachineID)
	}
	if cred.DeviceID != "" {
		req.Header.Set("X-Device-Id", cred.DeviceID)
	}
}

// PrepareBody 改写请求体为 SOLO 格式。
func PrepareBody(src []byte) ([]byte, string, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src, "glm-5.2", err
	}

	model, _ := obj["model"].(string)
	if model == "" {
		model = "glm-5.2"
	}

	obj["stream"] = true
	obj["function"] = Function
	obj["config_name"] = model
	obj["model"] = model

	// 归一化 messages 内容
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

// Chat 执行一次非流式对话（聚合并返回完整 JSON）。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	respBody, model, errDo := doSOLORequest(ctx, baseURL, req)
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
			return nil, fmt.Errorf("traesolo upstream error: code=%.0f msg=%s", code, msg)
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
	respBody, model, errDo := doSOLORequest(ctx, baseURL, req)
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
			return fmt.Errorf("traesolo stream error: code=%.0f msg=%s", code, msg)
		}
	}

	return scanner.Err()
}

func doSOLORequest(ctx context.Context, baseURL string, req ChatRequest) (io.ReadCloser, string, error) {
	if req.Credential == nil {
		return nil, "", fmt.Errorf("traesolo credential is nil")
	}

	body, model, errPrep := PrepareBody(req.Body)
	if errPrep != nil {
		return nil, "", errPrep
	}

	targetHost := strings.TrimRight(baseURL, "/")
	if targetHost == "" {
		targetHost = AgentHost
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, targetHost+EpChat, bytes.NewReader(body))
	if errReq != nil {
		return nil, "", errReq
	}
	ApplySOLOHeaders(httpReq, req.Credential, true)

	client := httpx.Client(ctx, 300*time.Second)
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return nil, "", fmt.Errorf("traesolo chat request failed: %w", errDo)
	}

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, "", Classify(resp.StatusCode, string(raw))
	}

	return resp.Body, model, nil
}
