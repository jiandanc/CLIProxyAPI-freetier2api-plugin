package zcode

// 对话转发：OpenAI chat-completions ↔ Anthropic Messages 双向转换。
//
// 上游就是 Anthropic Messages 协议（模型名大小写敏感），因此这一层的职责是
// 把宿主交过来的 chat-completions 请求翻译过去、再把响应翻译回来。参考
// zcode2api/app/openai_compat.py 与 app/body_transform.py。
//
// 工具调用是必做项而非增强：插件声明只处理 chat-completions，Claude Code /
// Codex 的工具调用全部经此转换，缺了它就只剩纯文本问答。

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
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	Credential *Credential
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body   []byte
	Stream bool
}

// Chat 执行一次非流式对话，返回 OpenAI 格式的完整响应。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	httpReq, model, errBuild := buildHTTPRequest(ctx, baseURL, req)
	if errBuild != nil {
		return nil, errBuild
	}

	resp, errDo := httpx.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, transientErr("zcode chat request failed: " + errDo.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, transientErr("read zcode response: " + errRead.Error())
	}
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	// 上游有时以 HTTP 200 包装业务错误，不能当成成功响应。
	if errUpstream := classifyEmbeddedError(body); errUpstream != nil {
		return nil, errUpstream
	}
	return anthropicToOpenAICompletion(body, model)
}

// ChatStream 执行一次流式对话，逐条投递 OpenAI chunk。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest, onChunk func([]byte) error) error {
	httpReq, model, errBuild := buildHTTPRequest(ctx, baseURL, req)
	if errBuild != nil {
		return errBuild
	}

	// 流式响应总时长不可预期，超时交给上游空闲控制。
	resp, errDo := httpx.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return transientErr("zcode stream request failed: " + errDo.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Classify(resp.StatusCode, string(body))
	}

	// 上游有时忽略 stream 参数回一个完整 JSON 文档。校验错误必须在这里拦下，
	// 否则 SSE 解析器会把它当成空流——客户端收到「零内容」且看不到任何原因。
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "event-stream") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if errUpstream := classifyEmbeddedError(body); errUpstream != nil {
			return errUpstream
		}
		// 正常响应：按非流式转换，再切成单个内容分片投递。
		completion, errConvert := anthropicToOpenAICompletion(body, model)
		if errConvert != nil {
			return errConvert
		}
		return emitCollapsedCompletion(completion, model, onChunk)
	}

	converter := newStreamConverter(model)
	if errStart := converter.emitStart(onChunk); errStart != nil {
		return errStart
	}
	return parseAnthropicSSE(resp.Body, converter, onChunk)
}

// emitCollapsedCompletion 把一次非流式响应折成 OpenAI chunk 序列。
//
// 上游忽略 stream 参数时的兜底路径：客户端发的是流式请求，必须仍然收到
// 完整的 chunk 序列（含结束分片），否则会一直等后续内容。
func emitCollapsedCompletion(completion []byte, model string, onChunk func([]byte) error) error {
	var resp struct {
		Choices []struct {
			Message      map[string]any `json:"message"`
			FinishReason string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal(completion, &resp); errUnmarshal != nil || len(resp.Choices) == 0 {
		return transientErr("上游返回了非流式响应且无法解析")
	}

	converter := newStreamConverter(model)
	if errStart := converter.emitStart(onChunk); errStart != nil {
		return errStart
	}
	message := resp.Choices[0].Message
	if content, ok := message["content"].(string); ok && content != "" {
		chunk := converter.chunk(map[string]any{"content": content}, nil)
		if errEmit := onChunk(chunk); errEmit != nil {
			return errEmit
		}
	}
	if toolCalls, ok := message["tool_calls"].([]any); ok {
		for index, item := range toolCalls {
			call, _ := item.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			delta := map[string]any{"tool_calls": []any{map[string]any{
				"index": index,
				"id":    stringField(call, "id"),
				"type":  "function",
				"function": map[string]any{
					"name":      stringField(fn, "name"),
					"arguments": stringField(fn, "arguments"),
				},
			}}}
			if errEmit := onChunk(converter.chunk(delta, nil)); errEmit != nil {
				return errEmit
			}
		}
	}
	reason := firstNonEmpty(resp.Choices[0].FinishReason, "stop")
	return onChunk(converter.chunk(map[string]any{}, &reason))
}

// buildHTTPRequest 把 chat-completions 请求体转成上游 Anthropic 请求。
func buildHTTPRequest(ctx context.Context, baseURL string, req ChatRequest) (*http.Request, string, error) {
	if req.Credential == nil {
		return nil, "", fmt.Errorf("zcode credential is nil")
	}

	anthropicBody, model, errConvert := openAIToAnthropicBody(req.Body, req.Stream, req.Credential)
	if errConvert != nil {
		return nil, "", errConvert
	}

	target, errTarget := resolveTargetURL(baseURL, req.Credential)
	if errTarget != nil {
		return nil, "", errTarget
	}

	httpReq, errNew := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(anthropicBody))
	if errNew != nil {
		return nil, "", fmt.Errorf("create http request: %w", errNew)
	}
	applyChatHeaders(httpReq, req.Credential)
	return httpReq, model, nil
}

// resolveTargetURL 决定本次对话走哪条通道。
//
// 上游 Plan 通道强制校验 X-Aliyun-Captcha-Verify-Param；本插件不内置浏览器
// 验证码求解（成本与依赖都过高），因此**只走 API Key 通道**。JWT 仍保留在
// 凭证里供 billing 族使用，但不用它发对话请求。
func resolveTargetURL(baseURL string, cred *Credential) (string, error) {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = ZAIOrigin
	}
	if strings.TrimSpace(cred.APIKey) == "" {
		return "", fmt.Errorf("zcode: 该账号没有 API Key，无法走对话通道（Plan 通道需要人机验证码，本插件未启用求解）")
	}
	return base + PathAPIMessages, nil
}

// applyChatHeaders 组装出站请求头：鉴权 + 身份头 + 追踪头。
func applyChatHeaders(req *http.Request, cred *Credential) {
	headers := map[string]string{
		"Content-Type":      "application/json",
		"Accept":            "text/event-stream, application/json",
		"anthropic-version": AnthropicVersion,
		// API Key 通道用 x-api-key（免验证码）；不发送 Plan 通道的验证码头。
		"x-api-key": cred.APIKey,
	}
	BuildIdentityHeaders(cred.Profile).Apply(headers)
	for key, value := range BuildTraceHeaders() {
		headers[key] = value
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
}

// openAIToAnthropicBody 把 OpenAI 请求体转换成 Anthropic Messages 格式。
//
// 覆盖：system/developer 提取、多模态 content 分块、tool_calls → tool_use、
// role=tool → tool_result、tools/tool_choice 映射，以及 Plan 通道所需的
// body 变换（system 身份块 / cache_control / metadata.user_id）。
func openAIToAnthropicBody(src []byte, stream bool, cred *Credential) ([]byte, string, error) {
	var in struct {
		Model               string   `json:"model"`
		Messages            []any    `json:"messages"`
		MaxTokens           int      `json:"max_tokens"`
		MaxCompletionTokens int      `json:"max_completion_tokens"`
		Temperature         *float64 `json:"temperature,omitempty"`
		TopP                *float64 `json:"top_p,omitempty"`
		Stream              bool     `json:"stream"`
		Stop                any      `json:"stop"`
		Tools               []any    `json:"tools"`
		ToolChoice          any      `json:"tool_choice"`
	}
	if errUnmarshal := json.Unmarshal(src, &in); errUnmarshal != nil {
		return nil, "", fmt.Errorf("decode chat request: %w", errUnmarshal)
	}

	model := NormalizeModelName(in.Model)
	if model == "" {
		return nil, "", fmt.Errorf("zcode: model is required")
	}

	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = in.MaxCompletionTokens
	}
	if maxTokens <= 0 || maxTokens > MaxTokensLimit {
		// 客户端（如 auto-compact 续传）可能带超限值，上游会报 400/1210，统一钳制。
		maxTokens = DefaultMaxTokens
	}

	systemParts, outMessages := convertMessages(in.Messages)

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
	if stopSequences := normalizeStop(in.Stop); len(stopSequences) > 0 {
		out["stop_sequences"] = stopSequences
	}
	applyTools(out, in.Tools, in.ToolChoice)

	applyPlanBodyTransforms(out, model, cred)

	encoded, errMarshal := json.Marshal(out)
	if errMarshal != nil {
		return nil, "", fmt.Errorf("encode anthropic request: %w", errMarshal)
	}
	return encoded, model, nil
}

// convertMessages 把 OpenAI messages 拆成 (system 文本, Anthropic messages)。
func convertMessages(messages []any) ([]string, []map[string]any) {
	var systemParts []string
	outMessages := make([]map[string]any, 0, len(messages))

	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content := msg["content"]

		switch strings.TrimSpace(role) {
		case "system", "developer":
			if text := textFromContent(content); strings.TrimSpace(text) != "" {
				systemParts = append(systemParts, text)
			}

		case "tool":
			// 工具结果在 Anthropic 协议里是 user 消息下的 tool_result 块。
			outMessages = append(outMessages, map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": strings.TrimSpace(stringField(msg, "tool_call_id")),
					"content":     textFromContent(content),
				}},
			})

		case "assistant":
			outMessages = append(outMessages, map[string]any{
				"role":    "assistant",
				"content": assistantBlocks(content, msg["tool_calls"]),
			})

		default:
			outMessages = append(outMessages, map[string]any{
				"role":    "user",
				"content": userBlocks(content),
			})
		}
	}
	return systemParts, outMessages
}

// assistantBlocks 组装 assistant 的 content 块（文本 + tool_use）。
func assistantBlocks(content any, rawToolCalls any) []any {
	blocks := make([]any, 0, 4)
	if text := textFromContent(content); text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	toolCalls, _ := rawToolCalls.([]any)
	for _, item := range toolCalls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name := strings.TrimSpace(stringField(fn, "name"))
		if name == "" {
			continue
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    firstNonEmpty(stringField(call, "id"), "toolu_"+NewHexID(12)),
			"name":  name,
			"input": parseToolArguments(fn["arguments"]),
		})
	}
	if len(blocks) == 0 {
		// Anthropic 要求 content 非空。
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return blocks
}

// userBlocks 组装 user 的 content 块（文本 + 图片）。
func userBlocks(content any) []any {
	switch v := content.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": v}}
	case []any:
		blocks := make([]any, 0, len(v))
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch stringField(part, "type") {
			case "text":
				blocks = append(blocks, map[string]any{"type": "text", "text": stringField(part, "text")})
			case "image_url":
				if block := imageBlock(part["image_url"]); block != nil {
					blocks = append(blocks, block)
				}
			}
		}
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		}
		return blocks
	default:
		return []any{map[string]any{"type": "text", "text": ""}}
	}
}

// imageBlock 把 OpenAI image_url 转成 Anthropic image 块。
//
// 只支持 data: URL：外链图片需要网关自行下载，上游不接受直接引用。
func imageBlock(raw any) map[string]any {
	url := ""
	switch v := raw.(type) {
	case string:
		url = v
	case map[string]any:
		url = stringField(v, "url")
	}
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	head, data, found := strings.Cut(url, ",")
	if !found || strings.TrimSpace(data) == "" {
		return nil
	}
	mediaType := strings.TrimPrefix(head, "data:")
	if idx := strings.Index(mediaType, ";"); idx >= 0 {
		mediaType = mediaType[:idx]
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		},
	}
}

// applyTools 把 OpenAI tools / tool_choice 映射到 Anthropic 形态。
func applyTools(out map[string]any, tools []any, toolChoice any) {
	if len(tools) > 0 {
		mapped := make([]any, 0, len(tools))
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tool["function"].(map[string]any)
			if fn == nil {
				fn = tool
			}
			name := strings.TrimSpace(stringField(fn, "name"))
			if name == "" {
				continue
			}
			entry := map[string]any{"name": name}
			if desc := stringField(fn, "description"); desc != "" {
				entry["description"] = desc
			}
			if schema, ok := fn["parameters"].(map[string]any); ok && schema != nil {
				entry["input_schema"] = schema
			} else {
				entry["input_schema"] = map[string]any{"type": "object"}
			}
			mapped = append(mapped, entry)
		}
		if len(mapped) > 0 {
			out["tools"] = mapped
		}
	}

	switch choice := toolChoice.(type) {
	case string:
		switch choice {
		case "none":
			delete(out, "tools")
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		}
		// "auto" / 缺省：Anthropic 默认即 auto，无需显式设置。
	case map[string]any:
		fn, _ := choice["function"].(map[string]any)
		if name := strings.TrimSpace(stringField(fn, "name")); name != "" {
			out["tool_choice"] = map[string]any{"type": "tool", "name": name}
		}
	}
}

// applyPlanBodyTransforms 施加 Plan 通道所需的请求体变换。
//
// 上游网关做内容审查，缺少官方身份块会被拒为 3012；cache_control 与
// metadata.user_id 是官方客户端的固定行为。这些变换只在具备 JWT 时施加——
// API Key 通道不是官方客户端形态，注入反而制造矛盾信号。
func applyPlanBodyTransforms(body map[string]any, model string, cred *Credential) {
	if cred == nil || !cred.UsesPlanChannel() {
		return
	}
	applyStartPlanSystem(body, model)
	applyCacheControl(body)
	if userID := firstNonEmpty(cred.UserID, UserIDFromJWT(cred.JWTToken)); userID != "" {
		metadata, _ := body["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["user_id"] = userID
		body["metadata"] = metadata
	}
}

// anthropicToOpenAICompletion 把 Anthropic 响应转成 OpenAI chat.completion。
func anthropicToOpenAICompletion(raw []byte, model string) ([]byte, error) {
	var resp struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", errUnmarshal)
	}

	var text strings.Builder
	var toolCalls []any
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			toolCalls = append(toolCalls, buildToolCall(block.ID, block.Name, block.Input))
		}
	}

	message := map[string]any{"role": "assistant"}
	if text.Len() > 0 {
		message["content"] = text.String()
	} else {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	out := map[string]any{
		"id":      firstNonEmpty(resp.ID, "chatcmpl-"+NewHexID(12)),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   firstNonEmpty(resp.Model, model),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason(resp.StopReason, len(toolCalls) > 0),
		}},
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}

// buildToolCall 组装 OpenAI 形态的工具调用。
func buildToolCall(id, name string, rawInput json.RawMessage) map[string]any {
	input := "{}"
	if len(rawInput) > 0 {
		input = string(rawInput)
	}
	return map[string]any{
		"id":   firstNonEmpty(id, "toolu_"+NewHexID(12)),
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": input,
		},
	}
}

// finishReason 把 Anthropic stop_reason 映射成 OpenAI finish_reason。
func finishReason(stopReason string, hasToolCalls bool) string {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}

// streamConverter 是有状态的 Anthropic SSE → OpenAI chunk 转换器。
//
// 工具调用在流式下分两段到达：content_block_start 给 id/name，
// 随后的 input_json_delta 分批给 arguments 片段，因此必须维护索引映射。
type streamConverter struct {
	model     string
	chunkID   string
	created   int64
	toolIndex int
}

func newStreamConverter(model string) *streamConverter {
	return &streamConverter{
		model:   model,
		chunkID: "chatcmpl-" + NewHexID(12),
		created: time.Now().Unix(),
	}
}

// emitStart 投递首个 chunk（携带 role）。
func (c *streamConverter) emitStart(onChunk func([]byte) error) error {
	return onChunk(c.chunk(map[string]any{"role": "assistant"}, nil))
}

// feed 处理一条上游事件，产出零到多条 chunk。
func (c *streamConverter) feed(event map[string]any, onChunk func([]byte) error) error {
	switch stringField(event, "type") {
	case "message_start":
		if msg, ok := event["message"].(map[string]any); ok {
			if id := stringField(msg, "id"); id != "" {
				c.chunkID = id
			}
			if model := stringField(msg, "model"); model != "" {
				c.model = model
			}
		}

	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		if block != nil && stringField(block, "type") == "tool_use" {
			index := c.toolIndex
			c.toolIndex++
			return onChunk(c.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": index,
					"id":    stringField(block, "id"),
					"type":  "function",
					"function": map[string]any{
						"name":      stringField(block, "name"),
						"arguments": "",
					},
				}},
			}, nil))
		}

	case "content_block_delta":
		delta, _ := event["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		switch stringField(delta, "type") {
		case "text_delta":
			if text := stringField(delta, "text"); text != "" {
				return onChunk(c.chunk(map[string]any{"content": text}, nil))
			}
		case "input_json_delta":
			if partial := stringField(delta, "partial_json"); partial != "" {
				return onChunk(c.chunk(map[string]any{
					"tool_calls": []any{map[string]any{
						"index":    maxInt(c.toolIndex-1, 0),
						"function": map[string]any{"arguments": partial},
					}},
				}, nil))
			}
		}

	case "message_delta":
		delta, _ := event["delta"].(map[string]any)
		reason := finishReason(stringField(delta, "stop_reason"), c.toolIndex > 0)
		return onChunk(c.chunk(map[string]any{}, &reason))

	case "error":
		errObj, _ := event["error"].(map[string]any)
		return fmt.Errorf("上游流式错误: %s", firstNonEmpty(stringField(errObj, "message"), "unknown"))
	}
	return nil
}

// chunk 组装一条 OpenAI chat.completion.chunk。
func (c *streamConverter) chunk(delta map[string]any, finishReason *string) []byte {
	var finish any
	if finishReason != nil {
		finish = *finishReason
	}
	payload := map[string]any{
		"id":      c.chunkID,
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

// parseAnthropicSSE 逐行解析上游 SSE 并交给转换器。
func parseAnthropicSSE(r io.Reader, converter *streamConverter, onChunk func([]byte) error) error {
	scanner := bufio.NewScanner(r)
	// 单条事件可能很大（大段 text_delta / base64 图片），放宽到 8MB。
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if errUnmarshal := json.Unmarshal([]byte(data), &event); errUnmarshal != nil {
			continue
		}
		if errFeed := converter.feed(event, onChunk); errFeed != nil {
			return errFeed
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return transientErr("read zcode stream: " + errScan.Error())
	}
	return nil
}

// classifyEmbeddedError 识别 HTTP 200 包装的业务错误。
//
// 上游对部分错误仍回 200 + {"code":N,...}，当成成功响应会把错误正文
// 当作模型输出吐给客户端。
func classifyEmbeddedError(body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var envelope struct {
		Type  string `json:"type"`
		Code  int    `json:"code"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(trimmed, &envelope); errUnmarshal != nil {
		return nil
	}
	// Anthropic 错误信封：{"type":"error","error":{...}}
	if envelope.Type == "error" && envelope.Error.Type != "" {
		return Classify(http.StatusBadRequest, string(trimmed))
	}
	// 业务码信封：有 code 且非 0，且不含 Anthropic 响应标志（content）。
	if envelope.Code != codeSuccess && envelope.Code != 0 && !bytes.Contains(trimmed, []byte(`"content"`)) {
		return Classify(http.StatusOK, string(trimmed))
	}
	return nil
}

// parseToolArguments 把工具参数解析成对象；非法 JSON 包成 _raw 保留原文。
func parseToolArguments(raw any) any {
	switch v := raw.(type) {
	case map[string]any:
		return v
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return map[string]any{}
		}
		var parsed map[string]any
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &parsed); errUnmarshal == nil {
			return parsed
		}
		return map[string]any{"_raw": v}
	default:
		return map[string]any{}
	}
}

// normalizeStop 归一化 stop 参数为 Anthropic 的 stop_sequences。
func normalizeStop(stop any) []string {
	switch v := stop.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// textFromContent 从 OpenAI content（字符串或分块数组）提取纯文本。
func textFromContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if stringField(part, "type") == "text" {
				if text := stringField(part, "text"); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// transientErr 把本地/传输层失败包装成共享的上游错误类型。
//
// 与 Classify 产出的业务错误的区别：Status 为 0，HTTPStatus() 因此回落到
// KindServer 的 502——宿主会换号重试，但不会因为一次半截响应把凭证标记为坏。
// 复用 workbuddy.Error 是刻意的：根层的 errorToPluginError 只认这套词汇表，
// 自建类型会被降级成通用 502 并丢失全部语义（见 errors.go 的说明）。
func transientErr(msg string) *workbuddy.Error {
	return &workbuddy.Error{Kind: workbuddy.KindServer, Status: 0, Msg: msg}
}
