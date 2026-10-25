package qoder

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"

	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
)

// 本文件是 Qoder 的传输与对话层：HTTP/会话基建（cosy 签名、Bearer 客户端）、
// 上游消息与增量转换、chat-completions 的出入口。

// 本文件提供「以载荷为输入输出」的对话入口。
//
// 上游 bridge 的 handler 直接写 http.ResponseWriter（它是从网关服务移植来的），
// 而本插件是 CPA 的 executor：不产生 HTTP 响应，只产出载荷交给宿主。
//
// 这里刻意**不**复用 HandleChatCompletions，而是把它的组装逻辑按载荷形态
// 重写一遍：那套 handler 把 SSE 帧直接 `fmt.Fprintf(w, "data: %s\n\n", ...)`，
// 与宿主的分帧职责重复（宿主会按声明格式自己加 `data: `）。因此这里只产出
// 裸 JSON 载荷，帧包装交给宿主。

// RunChatCompletions 执行一次非流式 chat-completions，把响应体写进 w。
func RunChatCompletions(b *Bridge, payload []byte, w io.Writer) error {
	req, errDecode := decodeChatRequest(payload)
	if errDecode != nil {
		return errDecode
	}
	reqID := "chatcmpl-" + NewRequestID()
	created := UnixSec()
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
	reqID := "chatcmpl-" + NewRequestID()
	created := UnixSec()
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
		idx := core.IntFromAny(call["index"])
		var target map[string]interface{}
		for _, item := range accumulated {
			if existing, okMap := item.(map[string]interface{}); okMap {
				if core.IntFromAny(existing["index"]) == idx {
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
			if core.IntFromAny(left["index"]) > core.IntFromAny(right["index"]) {
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

func BuildQoderMessages(templateMsgs []interface{}, incoming []interface{}, prompt string, toolsEnabled bool) []interface{} {
	var rebuilt []interface{}

	hasIncomingSystem := false
	for _, m := range incoming {
		if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
			hasIncomingSystem = true
			break
		}
	}
	if !hasIncomingSystem && templateMsgs != nil {
		for _, m := range templateMsgs {
			if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
				rebuilt = append(rebuilt, DeepCopyMap(mm))
			}
		}
	}

	for _, m := range incoming {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		// Handle multi-tool-result: a single Claude user message may contain multiple tool_results
		if role, _ := mm["role"].(string); role == "user" {
			if contentArr, ok := mm["content"].([]interface{}); ok && len(contentArr) > 0 {
				if first, ok := contentArr[0].(map[string]interface{}); ok && first["type"] == "tool_result" {
					for _, block := range contentArr {
						b, _ := block.(map[string]interface{})
						if b == nil || b["type"] != "tool_result" {
							continue
						}
						toolCallId, _ := b["tool_use_id"].(string)
						resultContent := ""
						switch c := b["content"].(type) {
						case string:
							resultContent = c
						case []interface{}:
							for _, part := range c {
								if pm, ok := part.(map[string]interface{}); ok {
									if t, ok := pm["text"].(string); ok {
										resultContent += t
									}
								}
							}
						}
						rebuilt = append(rebuilt, map[string]interface{}{
							"role":         "tool",
							"tool_call_id": toolCallId,
							"content":      resultContent,
						})
					}
					continue
				}
			}
		}
		converted := ConvertIncomingMessage(mm, toolsEnabled)
		if converted != nil {
			rebuilt = append(rebuilt, converted)
		}
	}

	if len(rebuilt) == 0 && prompt != "" {
		rebuilt = append(rebuilt, BuildUserMessage(prompt))
	}
	return rebuilt
}

func ConvertIncomingMessage(msg map[string]interface{}, toolsEnabled bool) map[string]interface{} {
	role, _ := msg["role"].(string)
	content := msg["content"]

	// Handle Claude Messages API content blocks (array of typed blocks)
	if contentArr, ok := content.([]interface{}); ok && len(contentArr) > 0 {
		firstBlock, _ := contentArr[0].(map[string]interface{})
		blockType, _ := firstBlock["type"].(string)

		// assistant message with tool_use blocks
		if role == "assistant" && HasBlockType(contentArr, "tool_use") {
			out := map[string]interface{}{"role": "assistant", "content": ""}
			var toolCalls []interface{}
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil {
					continue
				}
				switch b["type"] {
				case "tool_use":
					inputJSON, _ := json.Marshal(b["input"])
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":   b["id"],
						"type": "function",
						"function": map[string]interface{}{
							"name":      b["name"],
							"arguments": string(inputJSON),
						},
					})
				case "text":
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts != "" {
				out["content"] = textParts
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
			return out
		}

		// user message with tool_result blocks
		if role == "user" && blockType == "tool_result" {
			var results []map[string]interface{}
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil || b["type"] != "tool_result" {
					continue
				}
				toolCallId, _ := b["tool_use_id"].(string)
				resultContent := ""
				switch c := b["content"].(type) {
				case string:
					resultContent = c
				case []interface{}:
					for _, part := range c {
						if pm, ok := part.(map[string]interface{}); ok {
							if t, ok := pm["text"].(string); ok {
								resultContent += t
							}
						}
					}
				}
				results = append(results, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": toolCallId,
					"content":      resultContent,
				})
			}
			if len(results) == 1 {
				return results[0]
			}
			// Multiple tool results: return first, caller should handle multi
			// Actually we need to return all - use a special marker
			if len(results) > 0 {
				return results[0]
			}
			return nil
		}

		// Handle thinking blocks - skip them
		if blockType == "thinking" || blockType == "redacted_thinking" {
			// Extract only text blocks
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b != nil && b["type"] == "text" {
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts == "" {
				return nil
			}
			return BuildStructuredMessage(role, textParts)
		}
	}

	text := NormalizeMessageContent(msg)

	if role == "assistant" && toolsEnabled {
		if tc, ok := msg["tool_calls"]; ok {
			out := BuildStructuredMessage("assistant", text)
			out["tool_calls"] = tc
			return out
		}
	}

	if role == "tool" {
		out := BuildStructuredMessage("tool", text)
		if name, ok := msg["name"].(string); ok {
			out["name"] = name
		}
		if tcid, ok := msg["tool_call_id"].(string); ok {
			out["tool_call_id"] = tcid
		}
		return out
	}

	if text == "" {
		return nil
	}

	if role == "user" {
		return BuildUserMessage(text)
	}
	return BuildStructuredMessage(role, text)
}

func HasBlockType(blocks []interface{}, blockType string) bool {
	for _, block := range blocks {
		if b, ok := block.(map[string]interface{}); ok {
			if b["type"] == blockType {
				return true
			}
		}
	}
	return false
}

func BuildUserMessage(text string) map[string]interface{} {
	return map[string]interface{}{
		"role":    "user",
		"content": "",
		"contents": []interface{}{
			map[string]interface{}{"type": "text", "text": text},
		},
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func BuildStructuredMessage(role, text string) map[string]interface{} {
	return map[string]interface{}{
		"role":                        role,
		"content":                     text,
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func BlankResponseMeta() map[string]interface{} {
	return map[string]interface{}{
		"id": "",
		"usage": map[string]interface{}{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
			"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 0},
			"prompt_tokens_details":     map[string]interface{}{"cached_tokens": 0},
		},
	}
}

func NormalizeMessageContent(msg map[string]interface{}) string {
	c := msg["content"]
	if s, ok := c.(string); ok {
		return s
	}
	if c == nil {
		return ""
	}
	arr, ok := c.([]interface{})
	if !ok {
		data, _ := json.Marshal(c)
		return string(data)
	}
	var sb string
	for _, block := range arr {
		b, _ := block.(map[string]interface{})
		if t, ok := b["text"].(string); ok {
			if sb != "" {
				sb += "\n\n"
			}
			sb += t
		}
	}
	return sb
}

type Delta struct {
	Role         string
	Content      string
	Reasoning    string // 上游推理过程（reasoning_content）
	ToolCalls    []interface{}
	InputTokens  int
	OutputTokens int
	Err          error // 上游返回业务错误时非 nil
}

func (d Delta) isEmpty() bool {
	return d.Role == "" && d.Content == "" && d.Reasoning == "" && d.ToolCalls == nil && d.InputTokens == 0 && d.OutputTokens == 0 && d.Err == nil
}

func ExtractDelta(dataLine string) Delta {
	var wrapper map[string]interface{}
	if err := json.Unmarshal([]byte(dataLine), &wrapper); err != nil {
		logger.Debug("[delta] unmarshal wrapper failed: %v, raw=%s", err, dataLine)
		return Delta{}
	}
	// ---- 信封状态检查（第 3 步，hub iter_inner_sse 形态） ----
	// 上游帧：data:{"headers":{...},"body":"<内层 chunk>","statusCodeValue":200}
	// 关键形态：上游 HTTP200 建流后，把 provider 故障包在信封里投递
	// statusCodeValue=418/5xx（access log 记 200 而业务错的根因）。
	// 与内层业务 code（body 里的 {"code":"115"}）严格区分：信封错误带真实
	// HTTP 状态，可参与瞬时重试判定；重开闸门在 CallQoderWithOpts。
	if raw, ok := wrapper["statusCodeValue"]; ok {
		if status := toEnvelopeStatus(raw); status != 200 {
			ue := NewUpstreamError(status, envelopeDetail(wrapper))
			logger.Error("[delta] envelope statusCodeValue=%v -> %s", raw, ue.Message)
			return Delta{Err: ue}
		}
	}
	inner, _ := wrapper["body"].(string)
	if inner == "" {
		logger.Debug("[delta] no body field, keys=%v, raw=%s", mapKeys(wrapper), dataLine)
		return Delta{}
	}
	var innerJSON map[string]interface{}
	if err := json.Unmarshal([]byte(inner), &innerJSON); err != nil {
		logger.Debug("[delta] unmarshal inner failed: %v", err)
		return Delta{}
	}
	// 先捕获顶层 usage（上游可能在最后一个 chunk 中与 choices 一起返回），
	// 不提前 return：同帧若还带 choices 内容，合并进同一个 Delta，避免内容丢失
	var usageIn, usageOut int
	if usage, ok := innerJSON["usage"].(map[string]interface{}); ok {
		usageIn = int(FloatVal(usage, "prompt_tokens"))
		usageOut = int(FloatVal(usage, "completion_tokens"))
	}
	choices, _ := innerJSON["choices"].([]interface{})
	for _, ch := range choices {
		chMap, _ := ch.(map[string]interface{})
		delta, _ := chMap["delta"].(map[string]interface{})
		if delta == nil {
			continue
		}
		role, _ := delta["role"].(string)
		content, _ := delta["content"].(string)
		reasoning, _ := delta["reasoning_content"].(string)
		var toolCalls []interface{}
		if tc, ok := delta["tool_calls"].([]interface{}); ok && len(tc) > 0 {
			toolCalls = tc
		}
		if role != "" || content != "" || reasoning != "" || toolCalls != nil {
			return Delta{Role: role, Content: content, Reasoning: reasoning, ToolCalls: toolCalls,
				InputTokens: usageIn, OutputTokens: usageOut}
		}
	}
	// 上游业务错误：{"code":"115","message":"..."}
	// 流内业务错误（HTTP 200 帧内）：非内容审核不做瞬时重试指引，
	// 内容审核单独映射为 content_policy_rejected。
	// 注意：业务错误检查仍先于 usage-only 返回，错误帧不因带 usage 被误判为正常帧。
	if code, ok := innerJSON["code"].(string); ok && code != "" && code != "0" {
		msg, _ := innerJSON["message"].(string)
		err := NewStreamBusinessError(fmt.Sprintf("upstream error code=%s: %s", code, msg))
		logger.Error("[delta] %v", err)
		return Delta{Err: err}
	}
	// choices 与业务 code 均未命中且 usage > 0：维持返回 usage-only Delta（行为同修复前）
	if usageIn > 0 || usageOut > 0 {
		return Delta{InputTokens: usageIn, OutputTokens: usageOut}
	}
	logger.Debug("[delta] no valid choices found, inner=%s", inner)
	return Delta{}
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// toEnvelopeStatus 信封 statusCodeValue 可能是 int/float64/str，统一成 int
// （解析失败回 502，按瞬时故障处理）。
func toEnvelopeStatus(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, err := strconv.Atoi(x)
		if err != nil {
			return 502
		}
		return n
	}
	return 502
}

// envelopeDetail 提取信封错误详情：body 为字符串时直接用（错误详情在内层），
// 否则截断整帧 JSON 便于排查。
func envelopeDetail(wrapper map[string]interface{}) string {
	if body, ok := wrapper["body"].(string); ok && body != "" {
		return body
	}
	b, _ := json.Marshal(wrapper)
	return core.Truncate(string(b), 400)
}

func ExtractLatestUserPrompt(msgs []interface{}) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]interface{})
		if m["role"] == "user" {
			return NormalizeMessageContent(m)
		}
	}
	return ""
}

func MakeChatChunk(id string, created int64, model string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "object": "chat.completion.chunk",
		"created": created, "model": model,
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "delta": map[string]interface{}{}, "finish_reason": nil},
		},
	}
}

func (b *Bridge) templateMessages() []interface{} {
	if msgs, ok := b.templateBase["messages"].([]interface{}); ok {
		return msgs
	}
	return nil
}

type BearerClient struct {
	sess *SessionContext
}

func NewBearerClient(sess *SessionContext) *BearerClient {
	return &BearerClient{sess: sess}
}

func (c *BearerClient) buildHeaders(pathSig, body, accept string, extra map[string]string) (map[string]string, error) {
	payloadB64, err := BuildPayloadB64(c.sess.Info)
	if err != nil {
		return nil, err
	}
	date := fmt.Sprintf("%d", UnixSec())
	sig := SignRequest(payloadB64, c.sess.CosyKey, date, body, pathSig)
	bearer := ComposeBearer(payloadB64, sig)

	h := map[string]string{
		"cosy-data-policy":      "agree",
		"content-type":          "application/json",
		"cosy-machinetype":      c.sess.MachineType,
		"cosy-clienttype":       "5",
		"cosy-date":             date,
		"cosy-user":             c.sess.Identity.Uid,
		"cosy-key":              c.sess.CosyKey,
		"cache-control":         "no-cache",
		"accept":                accept,
		"authorization":         bearer,
		"cosy-version":          Version,
		"cosy-machineid":        c.sess.MachineId,
		"cosy-machinetoken":     c.sess.MachineToken,
		"login-version":         "v2",
		"user-agent":            "Go-http-client/2.0",
		"cosy-scene":            "assistant",
		"cosy-business-product": "ide",
		"cosy-business-type":    "agent",
	}
	for k, v := range extra {
		h[k] = v
	}
	return h, nil
}

func PathSigFrom(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	p := u.Path
	if strings.HasPrefix(p, "/algo") {
		p = p[len("/algo"):]
	}
	return p, nil
}

// shortResponseMaxBytes 是 Qoder 短响应（模型目录、额度、签到、换取 jobToken、
// userinfo、登录轮询）的读取上限。这些接口正常响应都在几十 KB 内，超限说明
// 上游返回了异常内容（HTML 错误页、代理注入），必须拒绝而不是无界读进内存。
const shortResponseMaxBytes int64 = 1 << 20

// readQoderResponseBody 读取 Qoder 短响应，并拒绝超过上限的正文。
// 读取 limit+1 字节用于区分「恰好达到上限」和「被截断」，避免静默解析半截 JSON。
func readQoderResponseBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, shortResponseMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > shortResponseMaxBytes {
		return nil, fmt.Errorf("qoder response body exceeds %d bytes", shortResponseMaxBytes)
	}
	return data, nil
}

// callGet 用 cosy 签名发送 GET 请求，body 部分参与签名时为空字符串。
// 用于 /algo/api/v2/model/list 之类的纯查询接口。
func (c *BearerClient) callGet(ctx context.Context, fullURL string) (map[string]interface{}, error) {
	pathSig, err := PathSigFrom(fullURL)
	if err != nil {
		return nil, err
	}
	headers, err := c.buildHeaders(pathSig, "", "application/json", nil)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read qoder response: %w", errRead)
	}
	if resp.StatusCode != 200 {
		return nil, NewUpstreamError(resp.StatusCode, string(data))
	}
	preview := string(data)
	if len(preview) > 2000 {
		preview = preview[:2000]
	}
	logger.Info("callGet %s response (%d bytes): %s", fullURL, len(data), preview)
	var result map[string]interface{}
	err = json.Unmarshal(data, &result)
	return result, err
}

// openStreamLines sends a POST, reads SSE lines, calls onLine for each non-empty line.
// onLine 返回 false 表示消费方要求停止读取（如 deltaDispatcher 已捕获信封错误帧），
// 此时立即结束本次流读取并返回，由上层（CallQoderWithOpts 信封重开闸门）决定是否重开。
// ctx 可用于取消流式读取（例如客户端断开连接时）。
//
// 连接阶段（建流之前）带同账号瞬时重试：418/5xx/传输抖动 → 1s/2s 退避最多
// TransientMaxRetries 次。建流成功后的流内错误由上层（第 3 步信封重试）处理。
func (c *BearerClient) openStreamLines(ctx context.Context, fullURL string, jsonBody interface{}, extra map[string]string, onLine func(string) bool) error {
	pathSig, err := PathSigFrom(fullURL)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(jsonBody)
	if err != nil {
		return err
	}
	bodyStr, err := Encode(plain)
	if err != nil {
		return err
	}
	// 不设整体 Timeout，改由 context 控制生命周期，避免长流式响应被截断
	client := hostbridge.StreamClient(ctx, 0)

	var lastErr error
	queueWaits := 0
	maxQueueWaits := QueueMaxWaits()
	for attempt := 0; attempt <= TransientMaxRetries+maxQueueWaits; attempt++ {
		if attempt > 0 {
			if !core.SleepContext(ctx, RetryBackoff(attempt)) {
				return ctx.Err()
			}
		}
		headers, err := c.buildHeaders(pathSig, bodyStr, "text/event-stream", extra)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", fullURL, strings.NewReader(bodyStr))
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if IsTransientTransport(err) && attempt < TransientMaxRetries {
				logger.Info("transient transport error opening stream %s (try %d/%d): %s - retry in %ds",
					fullURL, attempt+1, TransientMaxRetries+maxQueueWaits+1, err.Error(), attempt+1)
				continue
			}
			return WrapTransportError(err)
		}
		if resp.StatusCode != 200 {
			body, errRead := readQoderResponseBody(resp.Body)
			resp.Body.Close()
			if errRead != nil {
				return fmt.Errorf("read qoder stream response: %w", errRead)
			}
			detail := string(body)
			if signal, queued := ParseQueueSignal(detail); queued && queueWaits < maxQueueWaits && ctx.Err() == nil {
				wait := queueWaitFor(signal)
				queueWaits++
				logger.Info("upstream queues model %s opening stream %s (%s); waiting %s before retry %d/%d",
					signal.ModelKey, fullURL, signal.QueueType, wait, queueWaits, maxQueueWaits)
				if !core.SleepContext(ctx, wait) {
					return ctx.Err()
				}
				continue
			}
			if signal, queued := ParseQueueSignal(detail); queued {
				return QueueSignalError(signal)
			}
			if IsTransientUpstream(resp.StatusCode, detail) && attempt < TransientMaxRetries {
				logger.Info("transient upstream HTTP %d opening stream %s (try %d/%d) - retry in %ds",
					resp.StatusCode, fullURL, attempt+1, TransientMaxRetries+maxQueueWaits+1, attempt+1)
				continue
			}
			return NewUpstreamError(resp.StatusCode, detail)
		}

		// 建流成功：进入流式读取，不再在本层重试
		defer resp.Body.Close()
		lineCh := make(chan string)
		errCh := make(chan error, 1)
		// done：主循环提前退出（onLine 返回 false）时关闭，解锁 scanner goroutine，
		// 避免其阻塞在 lineCh 发送处、仅剩 ctx.Done 一条出路造成 goroutine 泄漏
		done := make(chan struct{})
		defer close(done)
		go func() {
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
			for scanner.Scan() {
				select {
				case lineCh <- scanner.Text():
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				case <-done:
					return
				}
			}
			if err := scanner.Err(); err != nil {
				errCh <- err
				return
			}
			errCh <- nil
		}()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-errCh:
				logger.Debug("stream read complete")
				return err
			case line := <-lineCh:
				if line != "" {
					// onLine 返回 false：消费方已置位 upstreamErr（信封错误帧），
					// 立即停止读取，避免上游不关流时请求挂起；
					// 错误由 CallQoderWithOpts 的 emitted 闸门 + isRetryableStreamError 完成重开
					if !onLine(line) {
						return nil
					}
				}
			}
		}
	}
	// 防御性兜底：所有失败分支均在循环内直接 return，正常不可达
	return WrapTransportError(lastErr)
}

// ExchangeJobToken 将 PAT 或 refresh token 转换为 job token。
// 出站请求经宿主 HTTP 桥（ctx 中携带 host_callback_id）。
func ExchangeJobToken(ctx context.Context, token, machineId, machineToken, machineType string, jobTokenURL ...string) (map[string]interface{}, error) {
	date := CurrentDate()
	sig := SignLegacy(date)

	var personalToken, refreshToken string
	if strings.HasPrefix(token, "drt-") {
		refreshToken = token
		personalToken = ""
	} else {
		personalToken = token
		refreshToken = ""
	}

	inner := map[string]interface{}{
		"personalToken":      personalToken,
		"securityOauthToken": "",
		"refreshToken":       refreshToken,
		"needRefresh":        refreshToken != "",
		"authInfo":           map[string]interface{}{},
	}
	innerJSON, _ := json.Marshal(inner)

	outer := map[string]interface{}{
		"payload":       string(innerJSON),
		"encodeVersion": "1",
	}
	plain, _ := json.Marshal(outer)
	body, err := Encode(plain)
	if err != nil {
		return nil, err
	}

	endpoint := "https://center.qoder.sh/algo/api/v3/user/jobToken?Encode=1"
	if len(jobTokenURL) > 0 && jobTokenURL[0] != "" {
		endpoint = jobTokenURL[0]
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("cosy-machinetoken", machineToken)
	req.Header.Set("cosy-machinetype", machineType)
	req.Header.Set("login-version", "v2")
	req.Header.Set("appcode", AppCode)
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-encoding", "identity")
	req.Header.Set("cosy-version", Version)
	req.Header.Set("cosy-clienttype", "5")
	req.Header.Set("date", date)
	req.Header.Set("signature", sig)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("cosy-machineid", machineId)
	req.Header.Set("user-agent", "Go-http-client/2.0")

	resp, err := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read qoder job token response: %w", errRead)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d at jobToken body=%s", resp.StatusCode, string(data))
	}
	var result map[string]interface{}
	err = json.Unmarshal(data, &result)
	return result, err
}

const (
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	stdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customPad      = '$'
)

var c2s [128]int
var s2c [128]int

func init() {
	for i := range c2s {
		c2s[i] = -1
		s2c[i] = -1
	}
	for i := 0; i < 64; i++ {
		c2s[customAlphabet[i]] = int(stdAlphabet[i])
		s2c[stdAlphabet[i]] = int(customAlphabet[i])
	}
	c2s[customPad] = '='
	s2c['='] = customPad
}

func Encode(plaintext []byte) (string, error) {
	std := base64.StdEncoding.EncodeToString(plaintext)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		c := int(rearranged[i])
		if c >= 128 || s2c[c] < 0 {
			return "", fmt.Errorf("char out of alphabet: %d", c)
		}
		out[i] = byte(s2c[c])
	}
	return string(out), nil
}

func Decode(encoded string) ([]byte, error) {
	n := len(encoded)
	mapped := make([]byte, n)
	for i := 0; i < n; i++ {
		c := int(encoded[i])
		if c >= 128 || c2s[c] < 0 {
			return nil, fmt.Errorf("char out of custom alphabet: %d", c)
		}
		mapped[i] = byte(c2s[c])
	}
	a := n / 3
	std := string(mapped[n-a:]) + string(mapped[a:n-a]) + string(mapped[:a])
	return base64.StdEncoding.DecodeString(std)
}

// saltedSeed 保留接口；本实现不使用部署盐（与既有行为一致）。
func saltedSeed(seed string) string { return seed }

// FingerprintSeed 选择指纹种子：优先账号 UID；
// UID 未知时（jobToken 交换发生在拿到 uid 之前）退回凭证令牌，
// 保证同一凭证跨次调用派生结果一致、不随进程重启漂移。
func FingerprintSeed(uid, credential string) string {
	if uid != "" {
		return uid
	}
	return "cred:" + credential
}

func deriveID(seed, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + saltedSeed(seed)))
	return fmt.Sprintf("%x", sum)
}

// DeriveMachineID 派生稳定的 cosy-machineid（32 位十六进制）。
func DeriveMachineID(seed string) string {
	return deriveID(seed, "machine")
}

// DeriveMachineType 派生稳定的 18 位 machine type（与 hub derive_machine_type 同构）。
func DeriveMachineType(seed string) string {
	return deriveID(seed, "machinetype")[:18]
}

// DeriveMachineToken 派生稳定的 machine token。
//
// 形态对齐官方组件输出：88 字符 base64url（65 字节熵）。SHA-512 只有 64
// 字节，因此再追加一轮带不同前缀的摘要凑满——仍是确定性的，同一种子不变。
func DeriveMachineToken(seed string) string {
	primary := sha512.Sum512([]byte("machinetoken:" + saltedSeed(seed)))
	secondary := sha512.Sum512([]byte("machinetoken2:" + saltedSeed(seed)))
	raw := make([]byte, 66)
	copy(raw, primary[:])
	raw[64] = secondary[0]
	raw[65] = secondary[1]
	return base64.RawURLEncoding.EncodeToString(raw)[:88]
}

const Version = "1.0.10"

const ServerPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

type AuthIdentity struct {
	Name               string
	Aid                string
	Uid                string
	YxUid              string
	OrganizationId     string
	OrganizationName   string
	UserType           string
	SecurityOauthToken string
	RefreshToken       string
}

type SessionContext struct {
	TempKey      []byte
	CosyKey      string
	Info         string
	Identity     AuthIdentity
	MachineId    string
	MachineToken string
	MachineType  string
}

func NewSession(id AuthIdentity, machineId, machineToken, machineType string) (*SessionContext, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	tempKey := []byte(fmt.Sprintf("%x", raw)[:16])

	cosyKeyBytes, err := rsaEncrypt(tempKey)
	if err != nil {
		return nil, err
	}
	cosyKey := base64.StdEncoding.EncodeToString(cosyKeyBytes)

	payloadBytes, err := authPayloadJSON(id)
	if err != nil {
		return nil, err
	}
	encPayload, err := aesEncrypt(payloadBytes, tempKey)
	if err != nil {
		return nil, err
	}
	info := base64.StdEncoding.EncodeToString(encPayload)

	return &SessionContext{
		TempKey:      tempKey,
		CosyKey:      cosyKey,
		Info:         info,
		Identity:     id,
		MachineId:    machineId,
		MachineToken: machineToken,
		MachineType:  machineType,
	}, nil
}

func SignRequest(payloadB64, cosyKey, cosyDate, body, pathWithoutAlgo string) string {
	s := payloadB64 + "\n" + cosyKey + "\n" + cosyDate + "\n" + body + "\n" + pathWithoutAlgo
	return Md5Hex(s)
}

func BuildPayloadB64(info string) (string, error) {
	m := map[string]string{
		"cosyVersion": Version,
		"ideVersion":  "",
		"info":        info,
		"requestId":   NewUUID(),
		"version":     "v1",
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func ComposeBearer(payloadB64, sig string) string {
	return "Bearer COSY." + payloadB64 + "." + sig
}

func authPayloadJSON(id AuthIdentity) ([]byte, error) {
	m := map[string]string{
		"name":                 id.Name,
		"aid":                  id.Aid,
		"uid":                  id.Uid,
		"yx_uid":               id.YxUid,
		"organization_id":      id.OrganizationId,
		"organization_name":    id.OrganizationName,
		"user_type":            id.UserType,
		"security_oauth_token": id.SecurityOauthToken,
		"refresh_token":        id.RefreshToken,
	}
	return json.Marshal(m)
}

func rsaEncrypt(plaintext []byte) ([]byte, error) {
	pemData := strings.ReplaceAll(ServerPubKeyPEM, "\r", "")
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not RSA public key")
	}
	return rsa.EncryptPKCS1v15(rand.Reader, rsaPub, plaintext)
}

func aesEncrypt(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	pad := bs - len(plain)%bs
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	out := make([]byte, len(padded))
	iv := key[:bs]
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(out, padded)
	return out, nil
}

func NewUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func NewRequestID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)[:24]
}
func UnixSec() int64 {
	return time.Now().Unix()
}

func UnixMs() int64 {
	return time.Now().UnixMilli()
}

func FloatVal(m map[string]interface{}, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// FloatPtr 取可选数值：缺字段返回 nil，显式 0 返回指向 0 的指针。
func FloatPtr(m map[string]interface{}, key string) *float64 {
	v, ok := m[key].(float64)
	if !ok {
		return nil
	}
	return &v
}

const (
	AppCode   = "cosy"
	SecretB64 = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==" // base64("war, war never changes")
	sep       = "&"
)

func CurrentDate() string {
	return time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
}

func SignLegacy(date string) string {
	return Md5Hex(AppCode + sep + SecretB64 + sep + date)
}

func Md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}

// ==================== 专有协议 Bridge 引擎与端点 ====================

// 本文件是 Qoder 的协议层：区域与端点表、专有协议引擎（Bridge 建链、
// 模型列举、CallQoderWithOpts）、排队信号与请求模板。

// baseprompt.json 是 Qoder 上游 agent_chat_generation 的请求模板（移植自 qoder2api）。
// 模板里保留了 {UUIDn}/{TIME1} 占位符：每次建 Bridge 时替换为实际值再解析成 JSON。
//
//go:embed assets/baseprompt.json
var basePromptRaw []byte

// TemplateBase 构造一次请求模板。
//
// 与 qoder2api 的差异：这里对解析失败返回错误而不是静默忽略。模板解析失败意味着
// 所有上游请求都会失真，静默继续会让问题表现成"模型答非所问"这种难以定位的现象。
func TemplateBase() (map[string]interface{}, error) {
	template := string(basePromptRaw)
	for _, placeholder := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
		template = strings.ReplaceAll(template, placeholder, NewUUID())
	}
	template = strings.ReplaceAll(template, "{TIME1}", fmt.Sprintf("%d", UnixMs()))

	var templateBase map[string]interface{}
	if errUnmarshal := json.Unmarshal([]byte(template), &templateBase); errUnmarshal != nil {
		return nil, fmt.Errorf("baseprompt.json 模板解析失败（内嵌资源损坏或占位符未替换）: %w", errUnmarshal)
	}
	if templateBase == nil {
		return nil, fmt.Errorf("baseprompt.json 模板为空")
	}
	return templateBase, nil
}

// BridgeSecret 返回要交给 NewBridge 的凭证串。
//
// 上游 NewBridge 内部会自己调 ParseOAuthSecret：传 dt- 明文会丢掉
// refresh_token（会话无法续期），所以有 refresh token 时必须按 qoder2api
// 导出的 JSON 形态传，与上游桌面端行为保持一致。
func BridgeSecret(cred *Credential) string {
	if cred == nil {
		return ""
	}
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return cred.Token
	}
	payload, errMarshal := json.Marshal(map[string]string{
		"device_token":  cred.Token,
		"refresh_token": cred.RefreshToken,
	})
	if errMarshal != nil {
		return cred.Token
	}
	return string(payload)
}

// CredentialFingerprint 生成凭证指纹，用于 Bridge 缓存键。
//
// 不直接用 token 明文做缓存键：缓存键会进日志与内存 dump。
func CredentialFingerprint(cred *Credential) string {
	if cred == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(cred.Token + "|" + cred.RefreshToken + "|" + string(cred.Region)))
	return hex.EncodeToString(sum[:16])
}

func qoderChatStreamURL(region Region) string {
	return GetEndpoints(region).ChatStreamURL
}

func qoderModelListURL(region Region) string {
	return GetEndpoints(region).ModelListURL
}

func ParseOAuthSecret(secret string) (deviceToken, refreshToken string) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", ""
	}
	if strings.HasPrefix(secret, "dt-") || strings.HasPrefix(secret, "drt-") {
		return secret, ""
	}
	var payload struct {
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(secret), &payload); err != nil {
		return secret, ""
	}
	return payload.DeviceToken, payload.RefreshToken
}

func ResolveOAuthUserID(userInfo map[string]interface{}) string {
	if id := StrVal(userInfo, "id"); id != "" {
		return id
	}
	if id := StrVal(userInfo, "userId"); id != "" {
		return id
	}
	return StrVal(userInfo, "uid")
}

func FetchUserInfoWithToken(ctx context.Context, token string, region Region) (map[string]interface{}, error) {
	ep := GetEndpoints(region)
	req, err := http.NewRequestWithContext(ctx, "GET", ep.UserinfoBase, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, errRead := readQoderResponseBody(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("read qoder userinfo: %w", errRead)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	// DEBUG 用途排查原始 userinfo 响应结构：降为 Debug 级并截断至 500 字节，
	// 默认日志级别不再落盘用户名/组织等 PII，保留低级别排查能力
	logger.Debug("userinfo raw (%d bytes): %s", len(raw), core.Truncate(string(raw), 500))
	return result, nil
}

type Bridge struct {
	sess         *SessionContext
	client       *BearerClient
	region       Region
	templateBase map[string]interface{}
	// models 是所属 adapter 实例的模型索引：把对外 slug 还原成上游 key。
	// 为 nil 时（仅测试可构造）退回内置映射表。
	models *modelIndex
	// chatStreamURL 测试注入用：非空时覆盖 region 对应的上游 chat 流地址
	chatStreamURL string
}

// Model 是返回给前端的精简模型条目（仅保留下拉选择必要字段）
type Model struct {
	Key             string `json:"key"`
	DisplayName     string `json:"display_name"`
	Enable          bool   `json:"enable"`
	IsDefault       bool   `json:"is_default"`
	IsReasoning     bool   `json:"is_reasoning,omitempty"`
	ContextWindow   int    `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	MaxInputTokens  int    `json:"max_input_tokens,omitempty"`
	// PriceFactor 是上游计费倍率；用指针区分「没给」与「显式 0」（限免）。
	PriceFactor *float64 `json:"price_factor,omitempty"`
}

// NewBridge 创建 API 转换桥接。
// 支持两种认证方式：
//  1. OAuth device token (dt-xxx): 直接使用，调用 /api/v1/userinfo 获取用户信息
//  2. Personal Access Token (PAT): 调用 ExchangeJobToken 转换为 session token
//
// models 是所属 adapter 实例的模型索引，用于出站时把对外 slug 还原成上游 key；
// 传 nil 时 Bridge 退回内置映射表（尚无索引的测试场景）。
func NewBridge(ctx context.Context, pat string, region Region, templateBase map[string]interface{}, models *modelIndex) (*Bridge, error) {
	// 注意：不要在这里打印凭证（哪怕是前缀）。插件日志会进入 CPA 进程日志，
	// 账号标识只允许出现在成功建立会话后的非敏感字段里。
	var identity AuthIdentity
	var name, id string

	deviceToken, refreshToken := ParseOAuthSecret(pat)
	if strings.HasPrefix(deviceToken, "dt-") {
		userInfo, err := FetchUserInfoWithToken(ctx, deviceToken, region)
		if err != nil {
			return nil, fmt.Errorf("fetch user info: %w", err)
		}
		name = StrVal(userInfo, "name")
		id = ResolveOAuthUserID(userInfo)
		identity = AuthIdentity{
			Name:               name,
			Aid:                id,
			Uid:                id,
			OrganizationId:     StrVal(userInfo, "organization_id"),
			OrganizationName:   StrVal(userInfo, "organization_name"),
			UserType:           StrValDefault(userInfo, "userType", "personal_standard"),
			SecurityOauthToken: deviceToken,
			RefreshToken:       refreshToken,
		}
	} else {
		// jobToken 交换发生在拿到 uid 之前：机器头用凭证种子稳定派生
		// （hub 的交换请求不带机器头，此处保留 QCCG 原有行为但消除随机漂移）
		seed := FingerprintSeed("", pat)
		jt, err := ExchangeJobToken(ctx, pat, DeriveMachineID(seed), DeriveMachineToken(seed), DeriveMachineType(seed), GetEndpoints(region).JobTokenURL)
		if err != nil {
			return nil, fmt.Errorf("exchangeJobToken: %w", err)
		}
		name = StrVal(jt, "name")
		id = StrVal(jt, "id")
		identity = AuthIdentity{
			Name:               name,
			Aid:                id,
			Uid:                id,
			UserType:           StrValDefault(jt, "userType", "personal_standard"),
			SecurityOauthToken: StrVal(jt, "securityOauthToken"),
			RefreshToken:       StrVal(jt, "refreshToken"),
		}
	}

	logger.Info("Bridge session for %s (%s)", name, id)
	// 稳定设备指纹：优先 uid 派生（双区统一、重启不变），uid 缺失退回凭证种子
	seed := FingerprintSeed(id, pat)
	mid := DeriveMachineID(seed)
	mtoken := DeriveMachineToken(seed)
	mtype := DeriveMachineType(seed)
	logger.Info("Bridge fingerprint machineid=%s (stable derive)", mid)
	sess, err := NewSession(identity, mid, mtoken, mtype)
	if err != nil {
		return nil, err
	}
	client := NewBearerClient(sess)

	return &Bridge{
		sess:         sess,
		client:       client,
		region:       region,
		templateBase: templateBase,
		models:       models,
	}, nil
}

// ListAvailableModels 通过 cosy 签名调用 /algo/api/v2/model/list 拉取上游模型清单。
// 返回顶层 assistant 数组中 enable=true 的模型，按 is_default desc + display_name asc 排序。
func (b *Bridge) ListAvailableModels(ctx context.Context) ([]Model, error) {
	modelListURL := qoderModelListURL(b.region)
	resp, err := b.client.callGet(ctx, modelListURL)
	if err != nil {
		return nil, err
	}
	// debug: 打印响应顶层 key
	keys := make([]string, 0, len(resp))
	for k := range resp {
		keys = append(keys, k)
	}
	logger.Info("model list response keys: %v (region=%s)", keys, b.region)
	models := parseModels(resp)
	if len(models) == 0 {
		return nil, fmt.Errorf("%s -> empty model list, keys=%v", modelListURL, keys)
	}
	return models, nil
}

func parseModels(resp map[string]interface{}) []Model {
	// 按优先级尝试多个分类
	categories := []string{"assistant", "developer", "chat"}
	for _, cat := range categories {
		rawList, _ := resp[cat].([]interface{})
		out := extractModels(rawList)
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func extractModels(rawList []interface{}) []Model {
	out := make([]Model, 0, len(rawList))
	for _, it := range rawList {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		enable, _ := m["enable"].(bool)
		model := Model{
			Key:            StrVal(m, "key"),
			DisplayName:    StrVal(m, "display_name"),
			Enable:         enable,
			IsDefault:      func() bool { v, _ := m["is_default"].(bool); return v }(),
			IsReasoning:    func() bool { v, _ := m["is_reasoning"].(bool); return v }(),
			MaxInputTokens: int(FloatVal(m, "max_input_tokens")),
			PriceFactor:    FloatPtr(m, "price_factor"),
		}
		if cc, ok := m["context_config"].(map[string]interface{}); ok {
			var defaultCfg map[string]interface{}
			for _, cfg := range cc {
				cfgMap, _ := cfg.(map[string]interface{})
				if cfgMap == nil {
					continue
				}
				isDefault, _ := cfgMap["is_default"].(bool)
				if isDefault {
					defaultCfg = cfgMap
					break
				}
			}
			if defaultCfg == nil {
				for _, cfg := range cc {
					cfgMap, _ := cfg.(map[string]interface{})
					if cfgMap != nil {
						defaultCfg = cfgMap
						break
					}
				}
			}
			if defaultCfg != nil {
				model.ContextWindow = int(FloatVal(defaultCfg, "token_count"))
			}
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = model.MaxInputTokens
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = model.MaxInputTokens
		}
		if model.IsReasoning {
			model.MaxOutputTokens = 32768
		} else {
			model.MaxOutputTokens = 16384
		}
		out = append(out, model)
	}
	return out
}

// DeepCopyMap does a JSON round-trip deep copy.
func DeepCopyMap(m map[string]interface{}) map[string]interface{} {
	data, _ := json.Marshal(m)
	var out map[string]interface{}
	json.Unmarshal(data, &out)
	return out
}

// CallOpts 控制单次 CallQoder 的可选行为。
type CallOpts struct {
	IsReasoning bool // 是否启用推理模式（对应上游 model_config.is_reasoning）
	MaxTokens   int  // 客户端请求的 max_tokens，0 表示使用模板默认值
}

func (b *Bridge) CallQoder(ctx context.Context, agent string, messages []interface{}, model string, tools interface{}, onDelta func(Delta)) error {
	return b.CallQoderWithOpts(ctx, agent, messages, model, tools, CallOpts{}, onDelta)
}

// deltaDispatcher 把上游 SSE 行分发为 Delta 回调，返回值供 openStreamLines
// 的 onLine 使用：false 表示停止读取上游流。
// 约束（review P1）：一旦捕获到流内业务错误（*upstreamErr 非 nil），
// 立即停止向 onDelta 分发任何后续 delta，且从置位错误的那一行起返回 false，
// 让 openStreamLines 及时退出——上游发出错误帧后若不关流，否则重开闸门
// 永不触发、请求挂起；错误由 CallQoder 在流结束后统一返回。
func deltaDispatcher(onDelta func(Delta), upstreamErr *error) func(string) bool {
	return func(line string) bool {
		if *upstreamErr != nil {
			return false
		}
		if !strings.HasPrefix(line, "data:") {
			return true
		}
		dataPayload := strings.TrimSpace(line[5:])
		if dataPayload == "[DONE]" {
			return true
		}
		delta := ExtractDelta(dataPayload)
		if delta.Err != nil {
			*upstreamErr = delta.Err
			return false
		}
		if !delta.isEmpty() {
			onDelta(delta)
		}
		return true
	}
}

func (b *Bridge) CallQoderWithOpts(ctx context.Context, agent string, messages []interface{}, model string, tools interface{}, opts CallOpts, onDelta func(Delta)) error {
	// 客户端发来的是插件注册的对外 slug（deepseek-v4-pro），先还原成 Qoder 上游
	// 内部 model.key（dmodel）。上游对未知 key 会走兜底返回内容但不计 quota，
	// 所以这一步失败即等于「请求成功、dashboard 无用量」。
	originalModel := model
	model = b.resolveModel(agent, model)
	if model != originalModel {
		logger.Debug("mapModel %s/%s -> %s", agent, originalModel, model)
	}
	body := DeepCopyMap(b.templateBase)

	nid := NewUUID()
	body["request_id"] = nid
	body["chat_record_id"] = nid
	body["request_set_id"] = NewUUID()
	body["session_id"] = NewUUID()
	body["stream"] = true
	body["aliyun_user_type"] = b.sess.Identity.UserType

	if mc, ok := body["model_config"].(map[string]interface{}); ok {
		mc["key"] = model
		if opts.IsReasoning {
			mc["is_reasoning"] = true
		}
	}

	if opts.MaxTokens > 0 {
		if params, ok := body["parameters"].(map[string]interface{}); ok {
			params["max_tokens"] = opts.MaxTokens
		}
	}

	// Extract summary prompt from last user message for chat_context and business.name
	prompt := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if mm, ok := messages[i].(map[string]interface{}); ok && mm["role"] == "user" {
			prompt = NormalizeMessageContent(mm)
			if prompt == "" {
				if contents, ok := mm["contents"].([]interface{}); ok {
					for _, block := range contents {
						if b, ok := block.(map[string]interface{}); ok {
							if t, ok := b["text"].(string); ok {
								prompt = t
								break
							}
						}
					}
				}
			}
			break
		}
	}

	if biz, ok := body["business"].(map[string]interface{}); ok {
		biz["id"] = NewUUID()
		biz["begin_at"] = UnixMs()
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}

	if cc, ok := body["chat_context"].(map[string]interface{}); ok {
		if txt, ok := cc["text"].(map[string]interface{}); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]interface{}); ok {
			if oc, ok := extra["originalContent"].(map[string]interface{}); ok {
				oc["text"] = prompt
			}
		}
	}

	body["messages"] = messages
	if tools != nil {
		body["tools"] = tools
	}

	mcSource := "system"
	if mc, ok := body["model_config"].(map[string]interface{}); ok {
		if s, ok := mc["source"].(string); ok {
			mcSource = s
		}
	}

	qurl := qoderChatStreamURL(b.region)
	if b.chatStreamURL != "" {
		qurl = b.chatStreamURL
	}
	extra := map[string]string{
		"x-model-key":    model,
		"x-model-source": mcSource,
	}

	preview := prompt
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}
	logger.Info("callQoder model=%s prompt=%s", model, preview)
	logger.Debug("callQoder request body: %s", func() string { d, _ := json.Marshal(body); return string(d) }())

	// ---- SSE 信封重试闸门（第 3 步，参照 hub should_retry_envelope /
	// aggregate_with_envelope_retry）------------------------------------------
	// 关键形态：上游 HTTP200 建流后才在信封里投 418/5xx，连接层重试覆盖不到。
	// 闸门规则：只要「尚未向 onDelta 发出任何非空 delta」且错误属瞬时类
	// （信封 418/5xx/provider_error、传输层 TLS EOF 等），就重开上游重试，
	// 对调用方无感；已发出内容则直接上抛，避免客户端收到重复内容。
	// 客户端参数错 / 内容审核 / 401/429 经 IsTransientUpstream 判定为
	// 不可重试，快速失败。闸门做在此处，chat/claude/codex 三协议的
	// 流式与非流式路径全部自动覆盖。
	// 说明：连接阶段错误已在 openStreamLines 内部重试过；此处重开是
	// 第二层防护（有界：最多 TransientMaxRetries 次），主要覆盖流内信封错误。
	var lastErr error
	// 排队等待单独计数：免费模型的上游排队信号（isQueued/serviceAvailable=false）
	// 不受瞬时重试次数限制，但要受“等几次”的上限约束，且必须等上游建议的时长
	// （而不是 1s/2s 的瞬时退避——等不到位等于没等）。
	queueWaits := 0
	maxQueueWaits := QueueMaxWaits()
	for attempt := 0; attempt <= TransientMaxRetries+maxQueueWaits; attempt++ {
		if attempt > 0 {
			wait := RetryBackoff(attempt)
			if signal, queued := QueueSignalFromError(lastErr); queued {
				if queueWaits >= maxQueueWaits {
					return QueueSignalError(signal)
				}
				queueWaits++
				wait = queueWaitFor(signal)
				logger.Info("upstream queues model %s (%s); waiting %s before reopening (queue wait %d/%d)",
					signal.ModelKey, signal.QueueType, wait, queueWaits, maxQueueWaits)
			}
			if !core.SleepContext(ctx, wait) {
				return lastErr
			}
			logger.Info("reopen upstream after in-stream error (try %d/%d): %v",
				attempt+1, TransientMaxRetries+maxQueueWaits+1, lastErr)
		}
		emitted := false
		var upstreamErr error
		onDeltaWrapped := func(d Delta) {
			if !d.isEmpty() {
				emitted = true
			}
			onDelta(d)
		}
		streamErr := b.client.openStreamLines(ctx, qurl, body, extra,
			deltaDispatcher(onDeltaWrapped, &upstreamErr))
		if upstreamErr == nil && streamErr == nil {
			// 上游正常关流但未发出任何有效 delta/usage 帧：对齐 hub
			// "empty upstream stream" 显式报错（不纳入重开闸门——
			// isRetryableStreamError 对哨兵错误自然返回 false，不重试）
			if !emitted {
				return ErrEmptyStream
			}
			return nil
		}
		if upstreamErr != nil {
			lastErr = upstreamErr
		} else {
			lastErr = streamErr
		}
		if emitted || attempt >= TransientMaxRetries+maxQueueWaits || !isRetryableStreamError(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// isRetryableStreamError 判定流内/连接错误是否值得重开上游：
// 结构化上游错误按瞬时分类判定；裸传输错误（TLS EOF 等）按传输层判定；
// ctx 取消、业务错误、内容审核等一律不可重试。
func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		// 排队信号只有在我们会“等”的时候才值得重试：queue_max_waits=0（快速失败）时
		// 不能退化成 1s/2s 的瞬时重试（那样只会白打上游几次，仍拿不到结果）。
		// 判定按 ErrType 与信号两路走：友好化后的错误文本里可能已无排队标记。
		if ue.ErrType == ErrTypeModelBusy {
			return QueueMaxWaits() > 0
		}
		if _, queued := QueueSignalFromError(err); queued {
			return QueueMaxWaits() > 0
		}
		return IsTransientUpstream(ue.Status, ue.Detail)
	}
	return IsTransientTransport(err)
}

// sensitiveFieldNames 命中后其字符串子值会被脱敏（递归进数组/对象继续处理）
var sensitiveFieldNames = map[string]bool{
	"content":      true,
	"text":         true,
	"system":       true,
	"input":        true,
	"instructions": true,
	"prompt":       true,
	"description":  true,
}

func redactValue(v interface{}, parentField string) {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, child := range x {
			if s, ok := child.(string); ok {
				if (sensitiveFieldNames[k] || sensitiveFieldNames[parentField]) && len(s) > 32 {
					x[k] = fmt.Sprintf("<redacted len=%d>", len(s))
				}
				continue
			}
			redactValue(child, k)
		}
	case []interface{}:
		for i, item := range x {
			if s, ok := item.(string); ok {
				if sensitiveFieldNames[parentField] && len(s) > 32 {
					x[i] = fmt.Sprintf("<redacted len=%d>", len(s))
				}
				continue
			}
			redactValue(item, parentField)
		}
	}
}

func StrVal(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func StrValDefault(m map[string]interface{}, key, def string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return def
}

// InferAgent 根据模型名启发式推断 agent 类型，用于 chat/completions 这种多 agent 共用 endpoint
// 时选择正确的映射桶。模型名命中关键字按 gemini > claude > 默认 codex。
func InferAgent(model string) string {
	low := strings.ToLower(model)
	switch {
	case strings.Contains(low, "gemini"):
		return "gemini"
	case strings.Contains(low, "claude"), strings.Contains(low, "sonnet"), strings.Contains(low, "opus"), strings.Contains(low, "haiku"):
		return "claude"
	default:
		return "codex"
	}
}

// defaultModelMapping 是 bridge 的内置静态映射表。
// 采用「家族关键字 → Qoder model.key」形式，依赖下面 lookupMapping 的双向 substring 模糊匹配，
// 一条 "sonnet" 即可覆盖 claude-sonnet-4-6 / claude-sonnet-4-20250514 等所有变体。
//
// 设计原则：默认表只负责让请求落到合法 SKU 不出错。
// Qoder 上游合法 key 见 /v1/models 返回，随上游变化动态调整。
// GPT / Gemini 没有专属 SKU，统一映射到主力档 performance —— 既不浪费 ultimate 高 price_factor，
// 也不被 lite 限频；想分档（如 gpt-5→ultimate / gpt-5-mini→efficient）请在 UI 配置。
var defaultModelMapping = map[string]string{
	// Claude 三档
	"opus":   "qmodel_38max",
	"sonnet": "gmodel",
	"haiku":  "qfmodel",
	// 非 Claude 家族兜底
	"gpt":    "dmodel",
	"gemini": "gmodel",
}

// resolveModel 把客户端传来的模型名解析成上游 model_config.key。
//
// 解析顺序：
//  1. 本实例的模型索引精确命中（对外 slug "deepseek-v4-pro"，或兼容的旧上游
//     key "dmodel"）；
//  2. MapModel 处理「客户端发的是别家模型名」的场景（claude-sonnet-4-6 → gmodel，
//     以及中文档位 display_name 的大小写归一）。
//
// 未命中时 MapModel 会 ToLower 原样返回——上游照常回内容但不计 quota，
// 因此这里留一条 Debug，便于排查「dashboard 没用量」。
func (b *Bridge) resolveModel(agent, model string) string {
	if b.models != nil {
		if key, okKey := b.models.UpstreamKey(model); okKey {
			return key
		}
	}
	return MapModel(agent, model)
}

// MapModel 把外部模型名映射成上游 key 的兜底规则：
//  1. defaultModelMapping 精确命中
//  2. defaultModelMapping 双向 substring 模糊匹配（按 source 长度倒序，最长优先）
//  3. ToLower 兜底（如客户端传 "Performance" 这种 display_name 大小写）
//
// 只处理「索引未命中」的情况；本插件自己注册的模型由 Bridge.resolveModel
// 先走 modelIndex 精确解析。
//
// agent 由 handler 入口传入并保留用于未来按 agent 区分的扩展；当前不参与映射。
//
// 双向 substring 含义：source 包含 model（短关键字匹配长模型名，如 "sonnet" → "claude-sonnet-4-6"）
// 或 model 包含 source（长别名匹配短模型名，反向也能命中）。
func MapModel(agent, model string) string {
	if model == "" {
		return model
	}

	// 内置默认映射兜底
	if mapped := lookupMapping(model, defaultModelMapping); mapped != "" {
		return mapped
	}

	// 大小写归一化兜底（客户端可能直接传 Qoder key 如 "Performance"/"dmodel"）
	return strings.ToLower(model)
}

// lookupMapping 在单张映射表内执行：精确匹配 → 长度倒序 + 双向 substring 模糊匹配。
// 未命中返回空串。
func lookupMapping(model string, table map[string]string) string {
	if len(table) == 0 {
		return ""
	}
	// 1. 精确命中
	if v, ok := table[model]; ok && v != "" {
		return v
	}
	// 2. 长度倒序，确保 "claude-sonnet-4-6" 优先于 "sonnet" 命中
	type kv struct{ src, dst string }
	pairs := make([]kv, 0, len(table))
	for k, v := range table {
		if k == "" || v == "" {
			continue
		}
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].src) > len(pairs[j].src) })
	mLow := strings.ToLower(model)
	for _, p := range pairs {
		sLow := strings.ToLower(p.src)
		if strings.Contains(mLow, sLow) || strings.Contains(sLow, mLow) {
			return p.dst
		}
	}
	return ""
}

// 上游的排队/服务未就绪信号。
//
// 免费模型（如 Qwen3.8-Flash / qfmodel）会被上游放进排队队列，返回 HTTP 403 +
// 形如下面的载荷（message 里套了两层 JSON 字符串，且是转义后的）：
//
//	{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,
//	 \\\"modelKey\\\":\\\"qfmodel\\\",\\\"queueCount\\\":0,\\\"queueType\\\":\\\"p3\\\",
//	 \\\"retryAfterSeconds\\\":30,\\\"serviceAvailable\\\":false,\\\"waitTime\\\":30}\"}"}
//
// 这不是额度/凭证错误：等一会儿重试就能成功。旧实现把它当成 403 权限错误，
// 于是免费模型永远用不了（宿主还会显示成 insufficient_quota，误导用户去充值）。
type QueueSignal struct {
	Queued            bool
	ServiceAvailable  bool
	ModelKey          string
	QueueType         string
	QueueCount        int
	RetryAfterSeconds int
	WaitTimeSeconds   int
}

// WaitDuration 返回按上游建议应等待的时长（配置覆盖的值不经过这里）。
func (s QueueSignal) WaitDuration() time.Duration {
	if s.retryAfter() > 0 {
		return s.cappedWait()
	}
	return time.Duration(DefaultQueueWaitSeconds) * time.Second
}

// retryAfter 是上游给的等待秒数（retryAfterSeconds 优先，退化到 waitTime）。
func (s QueueSignal) retryAfter() int {
	if s.RetryAfterSeconds > 0 {
		return s.RetryAfterSeconds
	}
	if s.WaitTimeSeconds > 0 {
		return s.WaitTimeSeconds
	}
	return 0
}

func (s QueueSignal) cappedWait() time.Duration {
	d := time.Duration(s.retryAfter()) * time.Second
	if d > MaxQueueWait {
		return MaxQueueWait
	}
	return d
}

const (
	// DefaultQueueWaitSeconds 是上游没给建议等待时长时的兜底（秒）。
	DefaultQueueWaitSeconds = 15
	// DefaultQueueMaxWaits 是一次请求内默认最多为排队等待几次。
	DefaultQueueMaxWaits = 2
	// MaxQueueWait 是单次排队等待的上限，避免上游给个离谱数字把请求挂死。
	MaxQueueWait = 90 * time.Second
)

// QueueMaxWaits 返回当前允许的排队等待次数。
func QueueMaxWaits() int { return DefaultQueueMaxWaits }

// queueWaitFor 计算这次排队该等多久：配置覆盖优先，否则用上游建议值（受 MaxQueueWait 约束）。
func queueWaitFor(signal QueueSignal) time.Duration {
	if signal.retryAfter() > 0 {
		return signal.cappedWait()
	}
	return time.Duration(DefaultQueueWaitSeconds) * time.Second
}

// quoted 匹配可能被转义的 JSON 引号：正文里常见 \"isQueued\" 这种形态。
const quoted = `(?:\\?")?`

var (
	queueIntPattern    = regexp.MustCompile(quoted + `(?:retryAfterSeconds|waitTime)` + quoted + `\s*:\s*\\?"?\s*(\d+)`)
	queueStringPattern = regexp.MustCompile(quoted + `(?:modelKey|queueType)` + quoted + `\s*:\s*\\?"?\s*([A-Za-z0-9_.\-]+)`)
	queueBoolPattern   = regexp.MustCompile(quoted + `(?:isQueued|serviceAvailable)` + quoted + `\s*:\s*(true|false)`)
	queueQueuedPattern = regexp.MustCompile(quoted + `isQueued` + quoted + `\s*:\s*(true|false)`)
	queueAvailPattern  = regexp.MustCompile(quoted + `serviceAvailable` + quoted + `\s*:\s*(true|false)`)
)

// ParseQueueSignal 从上游响应正文里识别排队信号。
//
// 正文可能是多层 JSON 字符串（宿主/上游习惯把错误体再包一层），所以先逐层解包，
// 再做一次正则兜底扫描——只依赖少数字段存在，不假设整体结构。
func ParseQueueSignal(detail string) (QueueSignal, bool) {
	trimmed := strings.TrimSpace(detail)
	if trimmed == "" {
		return QueueSignal{}, false
	}
	if !strings.Contains(trimmed, "isQueued") && !strings.Contains(trimmed, "serviceAvailable") &&
		!strings.Contains(trimmed, "queueType") {
		return QueueSignal{}, false
	}

	signal := QueueSignal{ServiceAvailable: true}
	// seenBool 记录是否从正文里真的读到了排队布尔字段（避免把无关错误误判成排队）。
	seenBool := false
	applyQueueFields := func(segment string) {
		// 逐层解包：当前段本身是 JSON 就取 message/msg/detail 继续，同时读本层字段。
		for depth := 0; depth < 6 && segment != ""; depth++ {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(segment), &parsed) != nil {
				return
			}
			if queued, ok := parsed["isQueued"].(bool); ok {
				signal.Queued = queued
				seenBool = true
			}
			if available, ok := parsed["serviceAvailable"].(bool); ok {
				signal.ServiceAvailable = available
				seenBool = true
			}
			if key, ok := parsed["modelKey"].(string); ok && strings.TrimSpace(key) != "" {
				signal.ModelKey = strings.TrimSpace(key)
			}
			if queueType, ok := parsed["queueType"].(string); ok && strings.TrimSpace(queueType) != "" {
				signal.QueueType = strings.TrimSpace(queueType)
			}
			signal.RetryAfterSeconds = firstPositiveInt(signal.RetryAfterSeconds, parsed["retryAfterSeconds"])
			signal.WaitTimeSeconds = firstPositiveInt(signal.WaitTimeSeconds, parsed["waitTime"])
			signal.QueueCount = firstPositiveInt(signal.QueueCount, parsed["queueCount"])

			next := ""
			for _, key := range []string{"message", "msg", "detail", "error"} {
				switch value := parsed[key].(type) {
				case string:
					if strings.TrimSpace(value) != "" {
						next = strings.TrimSpace(value)
					}
				case map[string]interface{}:
					if encoded, errMarshal := json.Marshal(value); errMarshal == nil {
						next = string(encoded)
					}
				}
				if next != "" {
					break
				}
			}
			segment = next
		}
	}
	applyQueueFields(trimmed)

	// 正则兜底：载荷可能被转义得连 json.Unmarshal 都吃不下（例如上游直接拼字符串）。
	if signal.RetryAfterSeconds == 0 {
		if match := queueIntPattern.FindStringSubmatch(trimmed); len(match) == 2 {
			signal.RetryAfterSeconds, _ = strconv.Atoi(match[1])
		}
	}
	if signal.ModelKey == "" {
		if match := queueStringPattern.FindStringSubmatch(trimmed); len(match) == 2 {
			signal.ModelKey = match[1]
		}
	}
	// 布尔字段的兜底：正文可能被转义到 json.Unmarshal 都吃不下（上游拼字符串的情形）。
	if match := queueQueuedPattern.FindStringSubmatch(trimmed); len(match) == 2 {
		if !signal.Queued {
			signal.Queued = match[1] == "true"
			seenBool = true
		}
	}
	if match := queueAvailPattern.FindStringSubmatch(trimmed); len(match) == 2 {
		if signal.ServiceAvailable {
			signal.ServiceAvailable = match[1] == "true"
			seenBool = true
		}
	}
	if !seenBool {
		// 一个布尔字段都没读到，也没有任何排队痕迹：不是排队信号。
		if !queueBoolPattern.MatchString(trimmed) {
			return QueueSignal{}, false
		}
	}
	if signal.ServiceAvailable && !signal.Queued {
		return QueueSignal{}, false
	}
	return signal, true
}

func firstPositiveInt(current int, value interface{}) int {
	if current > 0 {
		return current
	}
	switch typed := value.(type) {
	case float64:
		if typed > 0 {
			return int(typed)
		}
	case json.Number:
		if parsed, errParse := typed.Int64(); errParse == nil && parsed > 0 {
			return int(parsed)
		}
	case string:
		if parsed, errParse := strconv.Atoi(strings.TrimSpace(typed)); errParse == nil && parsed > 0 {
			return parsed
		}
	}
	return current
}

// QueueSignalError 是排队等待预算耗尽后的错误：等待一段时间后重试即可，
// 与额度/凭证无关。
func QueueSignalError(signal QueueSignal) *UpstreamError {
	detail := "上游排队中"
	if signal.ModelKey != "" {
		detail = "上游模型 " + signal.ModelKey + " 排队中"
	}
	if signal.QueueType != "" {
		detail += "（队列 " + signal.QueueType + "）"
	}
	detail += "，等待后重试即可"
	return &UpstreamError{
		Status:  503,
		Detail:  detail,
		ErrType: ErrTypeModelBusy,
		Message: detail + "（不是额度或凭证问题；插件已按上游建议等待过，仍不可用）",
		// 必须把信号本身带上：友好化后的文本里已经没有 isQueued 之类标记，
		// 上层（isRetryableStreamError / 执行器）要靠这里判断“这是排队，不是瞬时故障”。
		Queue: &signal,
	}
}

// QueueSignalFromError 从错误里取出排队信号：优先用结构化字段，
// 退化到在消息/详情文本里重新识别（错误可能被包装过）。
func QueueSignalFromError(err error) (QueueSignal, bool) {
	if err == nil {
		return QueueSignal{}, false
	}
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		if upstream.Queue != nil {
			return *upstream.Queue, true
		}
		if signal, queued := ParseQueueSignal(upstream.Detail); queued {
			return signal, true
		}
	}
	return ParseQueueSignal(err.Error())
}

// Region 标识 Qoder 站点区域
type Region string

const (
	RegionGlobal Region = "global"
	RegionCN     Region = "cn"
)

// Endpoints 包含某个 region 对应的所有 API 端点
type Endpoints struct {
	DeviceLoginBase string // OAuth 登录页
	PollEndpoint    string // deviceToken/poll
	UserinfoBase    string // /api/v1/userinfo
	PlanEndpoint    string // /api/v2/user/plan
	QuotaEndpoint   string // /api/v2/quota/usage
	ChatStreamURL   string // SSE chat
	ModelListURL    string // model list
	JobTokenURL     string // PAT exchange
	// SashBase 是签到/活动接口的域名（/sash/api/v1/...），仅域名无路径。
	// 必须按区域选：把国际版 token 打到国内域名会得到 401 TOKEN_EXPIRE。
	SashBase string
}

var endpointsGlobal = Endpoints{
	DeviceLoginBase: "https://qoder.com/device/selectAccounts",
	PollEndpoint:    "https://openapi.qoder.sh/api/v1/deviceToken/poll",
	UserinfoBase:    "https://openapi.qoder.sh/api/v1/userinfo",
	PlanEndpoint:    "https://openapi.qoder.sh/api/v2/user/plan",
	QuotaEndpoint:   "https://openapi.qoder.sh/api/v2/quota/usage",
	ChatStreamURL:   "https://api1.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
	ModelListURL:    "https://api2.qoder.sh/algo/api/v2/model/list?Encode=1",
	JobTokenURL:     "https://center.qoder.sh/algo/api/v3/user/jobToken?Encode=1",
	SashBase:        "https://openapi.qoder.sh",
}

var endpointsCN = Endpoints{
	DeviceLoginBase: "https://qoder.com.cn/device/selectAccounts",
	PollEndpoint:    "https://openapi.qoder.com.cn/api/v1/deviceToken/poll",
	UserinfoBase:    "https://openapi.qoder.com.cn/api/v1/userinfo",
	PlanEndpoint:    "https://openapi.qoder.com.cn/api/v2/user/plan",
	QuotaEndpoint:   "https://openapi.qoder.com.cn/api/v2/quota/usage",
	ChatStreamURL:   "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
	ModelListURL:    "https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1",
	JobTokenURL:     "https://gateway.qoder.com.cn/algo/api/v3/user/jobToken?Encode=1",
	SashBase:        "https://openapi.qoder.com.cn",
}

// GetEndpoints 根据 region 返回对应端点配置，空值或未知值视为 global
func GetEndpoints(region Region) Endpoints {
	if region == RegionCN {
		return endpointsCN
	}
	return endpointsGlobal
}

// NormalizeRegion 将字符串归一化为合法 Region 值
func NormalizeRegion(s string) Region {
	if s == "cn" || s == "CN" {
		return RegionCN
	}
	return RegionGlobal
}
