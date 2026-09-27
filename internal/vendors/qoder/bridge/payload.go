package bridge

// 本文件提供「以载荷为输入输出」的对话入口。
//
// 上游 bridge 的 handler 直接写 http.ResponseWriter（它是从网关服务移植来的），
// 而本插件是 CPA 的 executor：不产生 HTTP 响应，只产出载荷交给宿主。
//
// 这里刻意**不**复用 HandleChatCompletions，而是把它的组装逻辑按载荷形态
// 重写一遍：那套 handler 把 SSE 帧直接 `fmt.Fprintf(w, "data: %s\n\n", ...)`，
// 与宿主的分帧职责重复（宿主会按声明格式自己加 `data: `）。因此这里只产出
// 裸 JSON 载荷，帧包装交给宿主。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"freetier2api-plugin/internal/vendors/qoder/cosy"
)

// RunChatCompletions 执行一次非流式 chat-completions，把响应体写进 w。
func RunChatCompletions(b *Bridge, payload []byte, w io.Writer) error {
	req, errDecode := decodeChatRequest(payload)
	if errDecode != nil {
		return errDecode
	}
	reqID := "chatcmpl-" + cosy.NewRequestID()
	created := cosy.UnixSec()
	messages := BuildQoderMessages(b.templateMessages(), req.Messages, req.Prompt, req.ToolsUsed)

	var content strings.Builder
	var toolCallBuf []interface{}
	var inputTokens, outputTokens int
	errCall := b.CallQoder(context.Background(), InferAgent(req.Model), messages, req.Model, req.Tools, func(d Delta) {
		if d.InputTokens > 0 || d.OutputTokens > 0 {
			inputTokens, outputTokens = d.InputTokens, d.OutputTokens
		}
		if d.Content != "" {
			content.WriteString(d.Content)
		}
		if d.ToolCalls != nil {
			toolCallBuf = append(toolCallBuf, d.ToolCalls...)
		}
	})
	if errCall != nil {
		return errCall
	}

	finishReason := "stop"
	message := map[string]interface{}{"role": "assistant", "content": content.String()}
	if len(toolCallBuf) > 0 {
		finishReason = "tool_calls"
		message["tool_calls"] = toolCallBuf
		if content.Len() == 0 {
			// OpenAI 语义：只有工具调用时 content 必须是 null，不是空串。
			message["content"] = nil
		}
	}
	response := map[string]interface{}{
		"id": reqID, "object": "chat.completion",
		"created": created, "model": req.Model,
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "message": message, "finish_reason": finishReason},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		},
	}
	encoded, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		return fmt.Errorf("encode chat response: %w", errMarshal)
	}
	_, errWrite := w.Write(encoded)
	return errWrite
}

// StreamChatCompletions 执行一次流式 chat-completions，逐帧交给 emit。
//
// emit 收到的是**裸 JSON 载荷**（不含 `data: ` 前缀）——帧包装由宿主按
// 声明格式完成。末尾的 [DONE] 标记同样由宿主添加，这里不发。
func StreamChatCompletions(b *Bridge, payload []byte, emit func([]byte) error) error {
	req, errDecode := decodeChatRequest(payload)
	if errDecode != nil {
		return errDecode
	}
	reqID := "chatcmpl-" + cosy.NewRequestID()
	created := cosy.UnixSec()
	messages := BuildQoderMessages(b.templateMessages(), req.Messages, req.Prompt, req.ToolsUsed)

	var toolCallBuf []interface{}
	var inputTokens, outputTokens int
	errCall := b.CallQoder(context.Background(), InferAgent(req.Model), messages, req.Model, req.Tools, func(d Delta) {
		if d.InputTokens > 0 || d.OutputTokens > 0 {
			inputTokens, outputTokens = d.InputTokens, d.OutputTokens
		}
		chunk := MakeChatChunk(reqID, created, req.Model)
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) > 0 {
			entry, _ := choices[0].(map[string]interface{})
			delta, _ := entry["delta"].(map[string]interface{})
			if delta != nil {
				if d.Content != "" {
					delta["role"] = "assistant"
					delta["content"] = d.Content
				}
				if d.Reasoning != "" {
					// reasoning_content 是 DeepSeek 系客户端的约定字段。
					delta["reasoning_content"] = d.Reasoning
				}
				if d.ToolCalls != nil {
					delta["tool_calls"] = d.ToolCalls
					toolCallBuf = append(toolCallBuf, d.ToolCalls...)
				}
			}
		}
		encoded, errMarshal := json.Marshal(chunk)
		if errMarshal != nil {
			return
		}
		_ = emit(encoded)
	})
	if errCall != nil {
		return errCall
	}

	// 收尾帧：finish_reason 与 usage。
	finishReason := "stop"
	if len(toolCallBuf) > 0 {
		finishReason = "tool_calls"
	}
	done := MakeChatChunk(reqID, created, req.Model)
	choices, _ := done["choices"].([]interface{})
	if len(choices) > 0 {
		entry, _ := choices[0].(map[string]interface{})
		entry["finish_reason"] = finishReason
		entry["delta"] = map[string]interface{}{}
	}
	if inputTokens > 0 || outputTokens > 0 {
		done["usage"] = map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		}
	}
	encoded, errMarshal := json.Marshal(done)
	if errMarshal != nil {
		return errMarshal
	}
	return emit(encoded)
}

// chatRequest 是一次对话请求的关键字段。
type chatRequest struct {
	Model     string
	Stream    bool
	Messages  []interface{}
	Tools     interface{}
	ToolsUsed bool
	Prompt    string
}

// decodeChatRequest 解析请求体。
func decodeChatRequest(payload []byte) (chatRequest, error) {
	var raw map[string]interface{}
	if errUnmarshal := json.Unmarshal(payload, &raw); errUnmarshal != nil {
		return chatRequest{}, fmt.Errorf("decode chat request: %w", errUnmarshal)
	}
	messages, _ := raw["messages"].([]interface{})
	tools := raw["tools"]
	stream, _ := raw["stream"].(bool)
	return chatRequest{
		Model:     StrValDefault(raw, "model", "auto"),
		Stream:    stream,
		Messages:  messages,
		Tools:     tools,
		ToolsUsed: tools != nil,
		Prompt:    ExtractLatestUserPrompt(messages),
	}, nil
}
