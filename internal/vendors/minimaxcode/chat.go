package minimaxcode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
	// upstreamErrTextRe 识别以「数字错误码:」开头的整段消息（如 50110:insufficient balance）。
	// 只用于完整消息节点，不用于流式增量——增量的切分点是任意的，锚定开头会误伤。
	upstreamErrTextRe = regexp.MustCompile(`^\d{5,8}:\s*[A-Za-z]`)
)

// ChatRequest 内部解析结构。
type chatPayload struct {
	Model      string `json:"model"`
	Messages   []any  `json:"messages"`
	Stream     bool   `json:"stream"`
	Tools      []any  `json:"tools"`
	ToolChoice any    `json:"tool_choice"`
}

// toolsEnabledFor 判断本次请求是否携带工具（决定是否启用标记协议）。
func (cp *chatPayload) toolsEnabled() bool {
	return len(cp.Tools) > 0 && !isToolChoiceNone(cp.ToolChoice)
}

// frameAggregator 汇总上游一整条 SSE 流的帧内容。
//
// 上游有三类携带文本的帧，处理方式不同：
//   - agent_message_chunk：流式增量，可直接向下游发射；
//   - agent_message：完整消息节点，其中 role=user 的是上游对用户输入的回显
//     （必须丢弃——否则会把 prompt 原样当回复吐给客户端），role=assistant 的
//     是整条回复的完整拷贝，与 chunk 增量重复，仅当没有任何 chunk 时才使用；
//   - 其它未知帧：深搜兜底，优先级最低。
type frameAggregator struct {
	chunkText, chunkThinking strings.Builder
	msgText, msgThinking     strings.Builder
	miscText, miscThinking   strings.Builder
	finishReason             string
	err                      string
}

func (a *frameAggregator) add(payload map[string]any) {
	if a.err != "" {
		return
	}
	if errMsg := extractUpstreamError(payload); errMsg != "" {
		a.err = errMsg
		return
	}
	if fr := findDeepString(payload, "finish_reason", "finishReason"); fr != "" {
		a.finishReason = fr
	}

	chunkNode, hasChunk := payload["agent_message_chunk"].(map[string]any)
	msgNode, hasMsg := payload["agent_message"].(map[string]any)

	switch {
	case hasChunk:
		a.chunkText.WriteString(findDeepString(chunkNode, contentKeys...))
		a.chunkThinking.WriteString(findDeepString(chunkNode, thinkingKeys...))
	case hasMsg:
		if isUserRoleNode(msgNode) {
			// 上游回显的用户消息：既不是回复也不是增量，直接丢弃。
			return
		}
		text := findDeepString(msgNode, contentKeys...)
		a.msgText.WriteString(text)
		a.msgThinking.WriteString(findDeepString(msgNode, thinkingKeys...))
		// 完整消息节点上的「错误码:」开头文本按上游错误处理。
		if errMsg := looksLikeUpstreamErrorText(a.msgText.String()); errMsg != "" {
			a.err = errMsg
		}
	default:
		text := findDeepStringSkipUser(payload, contentKeys...)
		a.miscText.WriteString(text)
		a.miscThinking.WriteString(findDeepStringSkipUser(payload, thinkingKeys...))
		if errMsg := looksLikeUpstreamErrorText(a.miscText.String()); errMsg != "" {
			a.err = errMsg
		}
	}
}

// chunkContent 报告是否有 chunk 增量文本。
func (a *frameAggregator) hasChunkContent() bool {
	return a.chunkText.Len() > 0
}

// finalize 在流结束后取出正文与思考内容（chunk 优先，完整消息兜底）。
func (a *frameAggregator) finalize() (text, thinking string) {
	text = a.chunkText.String()
	if text == "" {
		text = a.msgText.String()
	}
	if text == "" {
		text = a.miscText.String()
	}
	thinking = a.chunkThinking.String()
	if thinking == "" {
		thinking = a.msgThinking.String()
	}
	if thinking == "" {
		thinking = a.miscThinking.String()
	}
	return text, thinking
}

// errorText 返回结构化或文本形态的上游错误（若有）。
func (a *frameAggregator) errorText() string {
	if a.err != "" {
		return a.err
	}
	return ""
}

// isUserRoleNode 判断节点是否标记为用户角色（回显帧）。
func isUserRoleNode(node map[string]any) bool {
	role, _ := node["role"].(string)
	return strings.EqualFold(strings.TrimSpace(role), "user")
}

// findDeepStringSkipUser 深搜字符串，跳过 role=user 的节点（回显保护）。
func findDeepStringSkipUser(data any, keys ...string) string {
	switch v := data.(type) {
	case map[string]any:
		if isUserRoleNode(v) {
			return ""
		}
		for _, k := range keys {
			if s, ok := v[k].(string); ok && s != "" {
				return s
			}
		}
		for _, val := range v {
			if found := findDeepStringSkipUser(val, keys...); found != "" {
				return found
			}
		}
	case []any:
		for _, item := range v {
			if found := findDeepStringSkipUser(item, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}

// looksLikeUpstreamErrorText 判断一条完整消息是否是上游错误文本。
func looksLikeUpstreamErrorText(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || len(trimmed) > 300 {
		return ""
	}
	if upstreamErrTextRe.MatchString(trimmed) {
		return trimmed
	}
	return ""
}

// openAIToolCalls 把解析结果转成 OpenAI tool_calls 数组。
func openAIToolCalls(calls []ToolCall) []any {
	out := make([]any, 0, len(calls))
	for i, call := range calls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_%s", RandomUUID()[:8])
		}
		out = append(out, map[string]any{
			"id":   id,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
			"index": i,
		})
	}
	return out
}

// normalizeFinishReason 只放行客户端认识的取值。
//
// 上游会给出 "error" 这类非标准值（错误场景下错误文本已被拦截，
// 但防御性兜底），透传未知值会让协议转换端行为不确定。
func normalizeFinishReason(reason string) string {
	switch reason {
	case "stop", "length":
		return reason
	default:
		return "stop"
	}
}

// Chat 执行一次非流式对话。
func Chat(ctx context.Context, cred *Credential, req *core.ExecuteRequest, baseURLOverride string) ([]byte, error) {
	var cp chatPayload
	if len(req.Payload) > 0 {
		_ = json.Unmarshal(req.Payload, &cp)
	}
	toolsEnabled := cp.toolsEnabled()

	respBody, modelName, promptTokens, errDo := doMessageRequest(ctx, cred, req, baseURLOverride)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = respBody.Close() }()

	agg := &frameAggregator{}

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

		agg.add(payload)
		if errMsg := agg.errorText(); errMsg != "" {
			return nil, fmt.Errorf("minimax upstream error: %s", errMsg)
		}
	}

	if errScan := scanner.Err(); errScan != nil && errScan != io.EOF {
		return nil, fmt.Errorf("read stream: %w", errScan)
	}

	if errMsg := agg.errorText(); errMsg != "" {
		return nil, fmt.Errorf("minimax upstream error: %s", errMsg)
	}

	contentStr, reasoningStr := agg.finalize()
	var calls []ToolCall
	if toolsEnabled {
		contentStr, calls = parseToolCalls(contentStr)
	}
	if contentStr == "" && reasoningStr == "" && len(calls) == 0 {
		return nil, fmt.Errorf("minimax upstream returned empty response")
	}

	finishReason := normalizeFinishReason(agg.finishReason)

	msgMap := map[string]any{
		"role":    "assistant",
		"content": contentStr,
	}
	if reasoningStr != "" {
		msgMap["reasoning_content"] = reasoningStr
	}
	if len(calls) > 0 {
		finishReason = "tool_calls"
		msgMap["tool_calls"] = openAIToolCalls(calls)
		if contentStr == "" {
			// OpenAI 语义：只有工具调用时 content 是 null，不是空串。
			msgMap["content"] = nil
		}
	}

	completionTokens := EstimateTokens(contentStr + reasoningStr)

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
	var cp chatPayload
	if len(req.Payload) > 0 {
		_ = json.Unmarshal(req.Payload, &cp)
	}
	toolsEnabled := cp.toolsEnabled()

	respBody, modelName, _, errDo := doMessageRequest(ctx, cred, req, baseURLOverride)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = respBody.Close() }()

	agg := &frameAggregator{}
	parser := &toolStreamParser{}

	chunkID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	emitDelta := func(delta map[string]any) error {
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
		return onChunk(chunkBytes)
	}
	emitContent := func(text string) error {
		if text == "" {
			return nil
		}
		return emitDelta(map[string]any{"content": text})
	}

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

		// 结构化错误、回显过滤与错误码文本识别都在 add 内完成。
		agg.add(payload)
		if errMsg := agg.errorText(); errMsg != "" {
			return fmt.Errorf("minimax upstream error: %s", errMsg)
		}

		// chunk 增量立即发射；完整消息节点的文本到流结束再按优先级取舍。
		if chunkNode, ok := payload["agent_message_chunk"].(map[string]any); ok {
			thinking := findDeepString(chunkNode, thinkingKeys...)
			if thinking != "" {
				if errEmit := emitDelta(map[string]any{"reasoning_content": thinking}); errEmit != nil {
					return errEmit
				}
			}
			text := findDeepString(chunkNode, contentKeys...)
			if text != "" {
				outText := text
				if toolsEnabled {
					outText = parser.push(text)
				}
				if errEmit := emitContent(outText); errEmit != nil {
					return errEmit
				}
			}
		}
	}

	if errScan := scanner.Err(); errScan != nil && errScan != io.EOF {
		return fmt.Errorf("read stream: %w", errScan)
	}
	if errMsg := agg.errorText(); errMsg != "" {
		return fmt.Errorf("minimax upstream error: %s", errMsg)
	}

	// 没有任何 chunk 增量时，完整消息就是回复正文（错误检测已在 add 内触发）。
	var calls []ToolCall
	if !agg.hasChunkContent() {
		finalText, _ := agg.finalize()
		if finalText != "" {
			if toolsEnabled {
				if errEmit := emitContent(parser.push(finalText)); errEmit != nil {
					return errEmit
				}
			} else if errEmit := emitContent(finalText); errEmit != nil {
				return errEmit
			}
		}
		// chunk 没有思考增量时，用完整消息上的思考兜底。
		if agg.chunkThinking.Len() == 0 {
			if thinking := agg.msgThinking.String(); thinking != "" {
				if errEmit := emitDelta(map[string]any{"reasoning_content": thinking}); errEmit != nil {
					return errEmit
				}
			} else if thinking := agg.miscThinking.String(); thinking != "" {
				if errEmit := emitDelta(map[string]any{"reasoning_content": thinking}); errEmit != nil {
					return errEmit
				}
			}
		}
	}

	if toolsEnabled {
		rest, parsedCalls := parser.flush()
		if errEmit := emitContent(rest); errEmit != nil {
			return errEmit
		}
		calls = parsedCalls
		for i, call := range calls {
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("call_%s", RandomUUID()[:8])
			}
			// 一次性发出完整调用：宿主按 index 累加 arguments，
			// 分多帧发送会造成重复拼接。
			errEmit := emitDelta(map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": i,
						"id":    id,
						"type":  "function",
						"function": map[string]any{
							"name":      call.Name,
							"arguments": call.Arguments,
						},
					},
				},
			})
			if errEmit != nil {
				return errEmit
			}
		}
	}

	// 最终结束帧
	finish := normalizeFinishReason(agg.finishReason)
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	finalChunk := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finish,
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

	prompt := BuildPromptWithTools(turns, cp.Tools, cp.ToolChoice)
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
