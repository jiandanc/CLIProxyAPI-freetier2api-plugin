package zcode

// 对话转换测试：请求侧（tools / tool_calls / tool role / 多模态）与
// 响应侧（tool_use → tool_calls，含流式 input_json_delta）。

import (
	"encoding/json"
	"strings"
	"testing"
)

// anthropicBody 解码一次请求转换的结果。
func anthropicBody(t *testing.T, payload string, cred *Credential) map[string]any {
	t.Helper()
	if cred == nil {
		cred = &Credential{APIKey: "k.s"}
	}
	raw, model, errConvert := openAIToAnthropicBody([]byte(payload), false, cred)
	if errConvert != nil {
		t.Fatalf("convert request: %v", errConvert)
	}
	if model == "" {
		t.Fatal("model must not be empty")
	}
	var out map[string]any
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		t.Fatalf("decode converted body: %v", errUnmarshal)
	}
	return out
}

func TestConvertRequestMapsToolsAndToolCalls(t *testing.T) {
	body := anthropicBody(t, `{
		"model": "glm-5.3",
		"messages": [
			{"role": "user", "content": "list files"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function",
				 "function": {"name": "bash", "arguments": "{\"cmd\":\"ls\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "a.txt"}
		],
		"tools": [{"type": "function", "function": {
			"name": "bash",
			"description": "run a command",
			"parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}
		}}],
		"tool_choice": "auto"
	}`, nil)

	// 模型名归一化成官方大小写。
	if body["model"] != "GLM-5.3" {
		t.Fatalf("model = %v, want GLM-5.3", body["model"])
	}

	// tools 映射成 Anthropic 形态（name / description / input_schema）。
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 entry", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "bash" || tool["description"] != "run a command" {
		t.Fatalf("unexpected tool entry: %v", tool)
	}
	schema, _ := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("input_schema not mapped: %v", tool["input_schema"])
	}

	messages, _ := body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d entries, want 3", len(messages))
	}

	// assistant 的 tool_calls → tool_use 块，arguments 解析成对象。
	assistant, _ := messages[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant blocks = %v, want 1 tool_use", assistant["content"])
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "bash" {
		t.Fatalf("unexpected tool_use block: %v", block)
	}
	input, _ := block["input"].(map[string]any)
	if input["cmd"] != "ls" {
		t.Fatalf("tool arguments not decoded: %v", block["input"])
	}

	// role=tool → user 消息下的 tool_result 块。
	toolMsg, _ := messages[2].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Fatalf("tool result role = %v, want user", toolMsg["role"])
	}
	resultBlocks, _ := toolMsg["content"].([]any)
	result, _ := resultBlocks[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "call_1" {
		t.Fatalf("unexpected tool_result: %v", result)
	}
	if result["content"] != "a.txt" {
		t.Fatalf("tool result content = %v", result["content"])
	}
}

func TestConvertRequestExtractsSystemAndImages(t *testing.T) {
	body := anthropicBody(t, `{
		"model": "GLM-5.3-Flash",
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "developer", "content": "use tools"},
			{"role": "user", "content": [
				{"type": "text", "text": "what is this"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}},
				{"type": "image_url", "image_url": {"url": "https://example.com/a.png"}}
			]}
		]
	}`, nil)

	system, _ := body["system"].(string)
	if !strings.Contains(system, "be brief") || !strings.Contains(system, "use tools") {
		t.Fatalf("system not merged from system+developer: %q", system)
	}

	messages, _ := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("system/developer must not become messages: %v", messages)
	}
	user, _ := messages[0].(map[string]any)
	blocks, _ := user["content"].([]any)
	// 只保留 data: URL；外链图片无法回填，丢弃。
	if len(blocks) != 2 {
		t.Fatalf("blocks = %v, want text + 1 image", blocks)
	}
	image, _ := blocks[1].(map[string]any)
	if image["type"] != "image" {
		t.Fatalf("unexpected image block: %v", image)
	}
	source, _ := image["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "AAAA" {
		t.Fatalf("unexpected image source: %v", source)
	}
}

func TestConvertRequestToolChoiceMappings(t *testing.T) {
	// tool_choice=none 时不应携带 tools。
	body := anthropicBody(t, `{
		"model": "GLM-5.3",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"type": "function", "function": {"name": "bash", "parameters": {"type": "object"}}}],
		"tool_choice": "none"
	}`, nil)
	if _, exists := body["tools"]; exists {
		t.Fatal("tool_choice=none must drop tools")
	}

	// tool_choice={"type":"function"} 映射为强制指定工具。
	body = anthropicBody(t, `{
		"model": "GLM-5.3",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"type": "function", "function": {"name": "bash", "parameters": {"type": "object"}}}],
		"tool_choice": {"type": "function", "function": {"name": "bash"}}
	}`, nil)
	choice, _ := body["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "bash" {
		t.Fatalf("unexpected tool_choice: %v", choice)
	}
}

func TestConvertRequestClampsMaxTokens(t *testing.T) {
	// 超限值必须钳制，否则上游报 400/1210。
	body := anthropicBody(t, `{"model":"GLM-5.3","max_tokens":999999,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if got := int(body["max_tokens"].(float64)); got != DefaultMaxTokens {
		t.Fatalf("max_tokens = %d, want clamped to %d", got, DefaultMaxTokens)
	}

	// 合法值原样保留。
	body = anthropicBody(t, `{"model":"GLM-5.3","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if got := int(body["max_tokens"].(float64)); got != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", got)
	}
}

func TestConvertResponseMapsToolUse(t *testing.T) {
	raw := []byte(`{
		"id": "msg_1",
		"model": "GLM-5.3",
		"stop_reason": "tool_use",
		"content": [
			{"type": "text", "text": "let me check"},
			{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": {"cmd": "ls"}}
		],
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`)
	out, errConvert := anthropicToOpenAICompletion(raw, "GLM-5.3")
	if errConvert != nil {
		t.Fatalf("convert response: %v", errConvert)
	}

	var resp map[string]any
	if errUnmarshal := json.Unmarshal(out, &resp); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}

	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}

	message, _ := choice["message"].(map[string]any)
	if message["content"] != "let me check" {
		t.Fatalf("content = %v", message["content"])
	}
	toolCalls, _ := message["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %v, want 1", message["tool_calls"])
	}
	call, _ := toolCalls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if call["id"] != "toolu_1" || call["type"] != "function" || fn["name"] != "bash" {
		t.Fatalf("unexpected tool call: %v", call)
	}
	// arguments 必须是 JSON 字符串（OpenAI 形态）。
	args, _ := fn["arguments"].(string)
	var parsed map[string]any
	if errUnmarshal := json.Unmarshal([]byte(args), &parsed); errUnmarshal != nil || parsed["cmd"] != "ls" {
		t.Fatalf("arguments not a JSON object string: %v", fn["arguments"])
	}

	usage, _ := resp["usage"].(map[string]any)
	if usage["total_tokens"] != float64(15) {
		t.Fatalf("usage = %v", usage)
	}
}

// collectChunks 驱动流式转换器并收集所有产出。
func collectChunks(t *testing.T, events []string) []map[string]any {
	t.Helper()
	converter := newStreamConverter("GLM-5.3")
	var out []map[string]any
	appendChunk := func(raw []byte) error {
		var chunk map[string]any
		if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
			t.Fatalf("decode chunk: %v", errUnmarshal)
		}
		out = append(out, chunk)
		return nil
	}
	if errStart := converter.emitStart(appendChunk); errStart != nil {
		t.Fatalf("emit start: %v", errStart)
	}
	for _, event := range events {
		var parsed map[string]any
		if errUnmarshal := json.Unmarshal([]byte(event), &parsed); errUnmarshal != nil {
			t.Fatalf("decode event: %v", errUnmarshal)
		}
		if errFeed := converter.feed(parsed, appendChunk); errFeed != nil {
			t.Fatalf("feed: %v", errFeed)
		}
	}
	return out
}

func TestStreamConverterEmitsToolCalls(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"bash"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
	})

	// 首块是 role 声明。
	first := chunks[0]["choices"].([]any)[0].(map[string]any)
	if delta := first["delta"].(map[string]any); delta["role"] != "assistant" {
		t.Fatalf("first chunk must declare role: %v", delta)
	}

	// content_block_start 给出 id/name。
	startChoice := chunks[1]["choices"].([]any)[0].(map[string]any)
	startDelta := startChoice["delta"].(map[string]any)
	startCalls := startDelta["tool_calls"].([]any)
	startCall := startCalls[0].(map[string]any)
	if startCall["id"] != "toolu_9" || startCall["index"] != float64(0) {
		t.Fatalf("unexpected tool_call start: %v", startCall)
	}
	if fn := startCall["function"].(map[string]any); fn["name"] != "bash" || fn["arguments"] != "" {
		t.Fatalf("unexpected function start: %v", startCall["function"])
	}

	// 两段 input_json_delta 累加到同一 index。
	var arguments strings.Builder
	for _, chunk := range chunks[2:4] {
		choice := chunk["choices"].([]any)[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		calls := delta["tool_calls"].([]any)
		call := calls[0].(map[string]any)
		if call["index"] != float64(0) {
			t.Fatalf("delta must target index 0: %v", call)
		}
		arguments.WriteString(call["function"].(map[string]any)["arguments"].(string))
	}
	if arguments.String() != `{"cmd":"ls"}` {
		t.Fatalf("arguments = %q, want {\"cmd\":\"ls\"}", arguments.String())
	}

	// 末块给出 finish_reason=tool_calls。
	last := chunks[len(chunks)-1]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", last["finish_reason"])
	}
}

func TestStreamConverterEmitsTextDeltas(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
	})

	var text strings.Builder
	for _, chunk := range chunks {
		choice := chunk["choices"].([]any)[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			text.WriteString(content)
		}
	}
	if text.String() != "hello world" {
		t.Fatalf("text = %q", text.String())
	}

	last := chunks[len(chunks)-1]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop", last["finish_reason"])
	}
}

func TestClassifyEmbeddedError(t *testing.T) {
	// 正常的 Anthropic 响应不应被判为错误。
	if errUpstream := classifyEmbeddedError([]byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}]}`)); errUpstream != nil {
		t.Fatalf("valid response misclassified: %v", errUpstream)
	}
	// 200 包装的业务错误必须被识别，否则错误正文会被当模型输出。
	if errUpstream := classifyEmbeddedError([]byte(`{"code":1005,"msg":"daily quota exhausted"}`)); errUpstream == nil {
		t.Fatal("embedded business error not detected")
	}
}
