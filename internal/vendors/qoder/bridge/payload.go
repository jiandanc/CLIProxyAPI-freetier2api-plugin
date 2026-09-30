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
			toolCallBuf = mergeToolCallDeltas(toolCallBuf, d.ToolCalls)
		}
	})
	if errCall != nil {
		return errCall
	}

	toolCallBuf = filterValidToolCalls(toolCallBuf)

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
// emit 收到的是裸 JSON 载荷，外层由 hostStreamSink.Emit 统一格式化为合法的 SSE 格式。
func StreamChatCompletions(b *Bridge, payload []byte, emit func([]byte) error) error {
	req, errDecode := decodeChatRequest(payload)
	if errDecode != nil {
		return errDecode
	}
	reqID := "chatcmpl-" + cosy.NewRequestID()
	created := cosy.UnixSec()
	messages := BuildQoderMessages(b.templateMessages(), req.Messages, req.Prompt, req.ToolsUsed)

	var hasToolCalls bool
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
					hasToolCalls = true
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
	if hasToolCalls {
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

// mergeToolCallDeltas 按 index 累加流式工具分片，将 arguments 增量拼成完整工具调用。
func mergeToolCallDeltas(accumulated []interface{}, incoming []interface{}) []interface{} {
	for _, raw := range incoming {
		call, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		idx := intFromAny(call["index"])
		var target map[string]interface{}
		for _, item := range accumulated {
			if existing, okMap := item.(map[string]interface{}); okMap {
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
		if fnIncoming, okFn := call["function"].(map[string]interface{}); okFn {
			fnTarget, _ := target["function"].(map[string]interface{})
			if fnTarget == nil {
				fnTarget = map[string]interface{}{}
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

// filterValidToolCalls 剔除没有有效 function.name 的残缺工具调用，并按 index 升序排序。
func filterValidToolCalls(calls []interface{}) []interface{} {
	var valid []interface{}
	for _, item := range calls {
		call, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fn, okFn := call["function"].(map[string]interface{})
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
			left, _ := valid[j-1].(map[string]interface{})
			right, _ := valid[j].(map[string]interface{})
			if intFromAny(left["index"]) > intFromAny(right["index"]) {
				valid[j-1], valid[j] = valid[j], valid[j-1]
			}
		}
	}
	return valid
}

func cloneToolCall(src map[string]interface{}) map[string]interface{} {
	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		if k == "function" {
			if fn, ok := v.(map[string]interface{}); ok {
				fnCopy := make(map[string]interface{}, len(fn))
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

func intFromAny(v interface{}) int {
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
