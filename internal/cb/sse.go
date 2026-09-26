package cb

// 本文件实现上游 SSE 流的处理：流式透传与非流式聚合。
//
// 上游返回的是 OpenAI 形态的 SSE（`data: {...}` 帧 + 结尾 `data: [DONE]`），
// 但直接原样转发给客户端是不安全的：
//   - 上游会在 `choices[].message` 里放整条消息（而非 delta），累加型客户端会重复拼接；
//   - 上游的 tool_call 分片每片都带 function.name，累加型客户端会拼成 "BashBash"；
//   - 上游可能中途漏掉 id/model 字段，客户端会把它当成新消息。
//
// 因此采用**白名单重建**：只保留已知字段，其余丢弃。

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// errEmptyStream 表示上游流里没有任何有效数据帧。
//
// 单独定义是为了让调用方区分「上游空流」（上游缺陷，该报错）
// 与「客户端断连导致的写失败」（正常现象，不该报错）。
var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError 报告错误是否为上游空流。
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// StreamStats 是一次流式转发的统计。
type StreamStats struct {
	// Frames 是转发的有效帧数。
	Frames int
	// Usage 是上游给出的 usage（可能为空）。
	Usage map[string]any
	// Model 是上游回答里的模型名。
	Model string
	// Credit 是上游给出的本次真实扣费积分（可能为 nil）。
	Credit *float64
	// FinishReason 是最后一个 finish_reason。
	FinishReason string
	// SawDone 表示是否收到了 [DONE]。
	SawDone bool
}

// StreamOptions 是流式转发的选项。
type StreamOptions struct {
	// Emit 收到一帧可下发的载荷时调用。载荷是**裸 JSON**（不含 `data: ` 前缀），
	// 因为宿主会为 chat-completions 补上前缀与结尾的 [DONE]。
	Emit func(payload []byte) error
	// Hint 为错误帧附加值（可为 nil）。
	Hint func(code string) string
}

// Stream 读取上游 SSE 并把规范化后的帧交给 emit。
//
// 行为要点：
//   - 收到 `[DONE]` 后停止读取，之后的数据一律忽略；
//   - 含 error 键的帧原样透传（错误帧不该被白名单改写）；
//   - 白名单重建其余帧；
//   - 全程零有效数据帧 → 返回 errEmptyStream。
func Stream(body io.Reader, opts StreamOptions) (StreamStats, error) {
	var stats StreamStats
	if body == nil {
		return stats, errEmptyStream
	}

	scanner := bufio.NewScanner(body)
	// 上游的单帧可能很长（工具参数、长文本），默认 64KB 会截断。
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	firstID := ""
	seenToolName := map[int]bool{}
	sawAny := false

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// 非 data: 行（注释、event: 等）原样下发，保持上游的帧结构。
		if !strings.HasPrefix(trimmed, "data:") {
			if errEmit := opts.emit([]byte(line)); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			stats.SawDone = true
			break
		}
		if payload == "" {
			continue
		}

		// 错误帧：原样透传，可选附加提示。上游的错误信息比我们重建的更有价值。
		if isErrorFrame(payload) {
			stats.Frames++
			sawAny = true
			if errEmit := opts.emit([]byte(attachHint(payload, opts.Hint))); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		var frame map[string]any
		if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
			// 无法解析的帧原样透传（可能是上游新增的帧类型）。
			stats.Frames++
			sawAny = true
			if errEmit := opts.emit([]byte(payload)); errEmit != nil {
				return stats, errEmit
			}
			continue
		}

		// tool_call 的 name 只保留在首片。
		StripToolCallNames(frame, seenToolName)

		// id 续传：上游偶发在后续帧漏 id，客户端会把它当成新消息。
		if id, _ := frame["id"].(string); strings.TrimSpace(id) != "" {
			if firstID == "" {
				firstID = id
			}
		} else if firstID != "" {
			frame["id"] = firstID
		}

		collectStats(frame, &stats)
		normalized := normalizeFrame(frame)

		encoded, errEncode := json.Marshal(normalized)
		if errEncode != nil {
			continue
		}
		stats.Frames++
		sawAny = true
		if errEmit := opts.emit(encoded); errEmit != nil {
			return stats, errEmit
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		// 扫描错误（含上游断流）：已转发的帧仍然有效，把错误交给调用方判断。
		return stats, fmt.Errorf("read upstream stream: %w", errScan)
	}
	if !sawAny {
		return stats, errEmptyStream
	}
	return stats, nil
}

func (o StreamOptions) emit(payload []byte) error {
	if o.Emit == nil {
		return nil
	}
	return o.Emit(payload)
}

// isErrorFrame 判断帧是否为错误帧。
func isErrorFrame(payload string) bool {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(payload), &probe); errUnmarshal != nil {
		return false
	}
	return len(probe.Error) > 0 && string(probe.Error) != "null"
}

// attachHint 给错误帧附加提示文案（只加不改）。
func attachHint(payload string, hint func(code string) string) string {
	if hint == nil {
		return payload
	}
	var frame map[string]any
	if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
		return payload
	}
	errorField, okError := frame["error"].(map[string]any)
	if !okError {
		return payload
	}
	if _, okHint := errorField["gateway_hint"]; okHint {
		return payload
	}
	code, _ := errorField["code"].(string)
	text := hint(code)
	if strings.TrimSpace(text) == "" {
		return payload
	}
	errorField["gateway_hint"] = text
	encoded, errEncode := json.Marshal(frame)
	if errEncode != nil {
		return payload
	}
	return string(encoded)
}

// collectStats 从帧里提取统计信息。
func collectStats(frame map[string]any, stats *StreamStats) {
	if usage, okUsage := frame["usage"].(map[string]any); okUsage && len(usage) > 0 {
		stats.Usage = usage
		if credit, okCredit := toFloat64(usage["credit"]); okCredit {
			stats.Credit = &credit
		}
	}
	if model, okModel := frame["model"].(string); okModel && strings.TrimSpace(model) != "" {
		stats.Model = model
	}
	choices, okChoices := frame["choices"].([]any)
	if !okChoices || len(choices) == 0 {
		return
	}
	choice, okChoice := choices[0].(map[string]any)
	if !okChoice {
		return
	}
	if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
		stats.FinishReason = reason
	}
}

func toFloat64(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int64:
		return float64(typed), true
	case int:
		return float64(typed), true
	case json.Number:
		parsed, errParse := typed.Float64()
		return parsed, errParse == nil
	}
	return 0, false
}

// normalizeFrame 用白名单重建帧。
//
// 顶层保留：id / object / created / model / system_fingerprint / service_tier / choices / usage。
// delta 只保留：role / content / reasoning_content / refusal / tool_calls /
// function_call（空占位剔除）。
//
// 丢弃上游的私有字段（session_id、trace 等）：它们对客户端无用，
// 而且会把上游内部的会话标识暴露给下游。
func normalizeFrame(frame map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if value, okValue := frame[key]; okValue {
			out[key] = value
		}
	}
	if _, okObject := out["object"]; !okObject {
		out["object"] = "chat.completion.chunk"
	}
	if _, okID := out["id"]; !okID {
		out["id"] = "chatcmpl-wb2api"
	}

	if choices, okChoices := frame["choices"].([]any); okChoices {
		out["choices"] = normalizeChoices(choices)
	} else {
		out["choices"] = []any{}
	}

	// usage 缺失时显式给 null：客户端普遍会读这个键判断是否结束。
	if usage, okUsage := frame["usage"]; okUsage {
		out["usage"] = usage
	} else {
		out["usage"] = nil
	}
	return out
}

// normalizeChoices 重建 choices。
func normalizeChoices(choices []any) []any {
	out := make([]any, 0, len(choices))
	for _, item := range choices {
		choice, okChoice := item.(map[string]any)
		if !okChoice {
			continue
		}
		normalized := map[string]any{}
		if index, okIndex := choice["index"]; okIndex {
			normalized["index"] = index
		}
		if delta, okDelta := choice["delta"].(map[string]any); okDelta {
			normalized["delta"] = normalizeDelta(delta)
		}
		// finish_reason 空串归一为 null（客户端靠它判断结束）。
		if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
			normalized["finish_reason"] = reason
		} else {
			normalized["finish_reason"] = nil
		}
		out = append(out, normalized)
	}
	return out
}

// normalizeDelta 用白名单重建 delta。
func normalizeDelta(delta map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"role", "content", "reasoning_content", "refusal"} {
		if value, okValue := delta[key]; okValue {
			out[key] = value
		}
	}
	if calls, okCalls := delta["tool_calls"].([]any); okCalls && len(calls) > 0 {
		out["tool_calls"] = calls
	}
	// function_call 是旧字段：空对象占位会被客户端当成一次调用，必须剔除。
	if functionCall, okFunctionCall := delta["function_call"].(map[string]any); okFunctionCall && len(functionCall) > 0 {
		out["function_call"] = functionCall
	}
	return out
}

// Aggregate 把上游 SSE 流聚合成一个非流式响应。
//
// 上游强制流式，因此非流式客户端必须由我们在网关侧聚合。
func Aggregate(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, errEmptyStream
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var (
		id           string
		model        string
		created      int64
		content      strings.Builder
		reasoning    strings.Builder
		toolCalls    []any
		usage        map[string]any
		finishReason string
		sawDone      bool
		validEvents  int
		gotContent   bool
	)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		if payload == "" {
			continue
		}
		var frame map[string]any
		if errUnmarshal := json.Unmarshal([]byte(payload), &frame); errUnmarshal != nil {
			continue
		}
		if isErrorFrame(payload) {
			return nil, fmt.Errorf("upstream error: %s", truncate(payload, 512))
		}
		validEvents++

		if got := stringField(frame, "id"); got != "" && id == "" {
			id = got
		}
		if got := stringField(frame, "model"); got != "" {
			model = got
		}
		if value, okValue := toFloat64(frame["created"]); okValue {
			created = int64(value)
		}
		if payloadUsage, okUsage := frame["usage"].(map[string]any); okUsage && len(payloadUsage) > 0 {
			usage = payloadUsage
		}

		choices, okChoices := frame["choices"].([]any)
		if !okChoices || len(choices) == 0 {
			continue
		}
		choice, okChoice := choices[0].(map[string]any)
		if !okChoice {
			continue
		}
		if reason, okReason := choice["finish_reason"].(string); okReason && strings.TrimSpace(reason) != "" {
			finishReason = reason
		}

		// 部分上游帧把内容放在 message（而非 delta）里，两种都要处理。
		// gotContent 闩锁防止同一份内容被 delta 与 message 各算一次。
		if delta, okDelta := choice["delta"].(map[string]any); okDelta {
			appendContent(&content, delta["content"])
			appendContent(&reasoning, delta["reasoning_content"])
			if calls, okCalls := delta["tool_calls"].([]any); okCalls && len(calls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, calls)
				gotContent = true
			}
		}
		if message, okMessage := choice["message"].(map[string]any); okMessage && !gotContent {
			if text, okText := message["content"].(string); okText && text != "" {
				content.WriteString(text)
			}
			if text, okText := message["reasoning_content"].(string); okText && text != "" {
				reasoning.WriteString(text)
			}
		}
		if content.Len() > 0 {
			gotContent = true
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, fmt.Errorf("read upstream stream: %w", errScan)
	}
	if validEvents == 0 {
		return nil, errEmptyStream
	}

	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		// 输出被截断或流没正常结束时，工具参数必然不完整，丢弃它们。
		if finishReason == "length" || !sawDone {
			toolCalls = DropTruncatedToolCalls(toolCalls)
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = sortToolCalls(toolCalls)
		}
	}
	if strings.TrimSpace(finishReason) == "" {
		finishReason = "stop"
	}

	response := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		response["usage"] = ensureUsageTotal(usage)
	}
	return json.Marshal(response)
}

func stringField(frame map[string]any, key string) string {
	value, _ := frame[key].(string)
	return strings.TrimSpace(value)
}

func appendContent(builder *strings.Builder, value any) {
	if text, okText := value.(string); okText && text != "" {
		builder.WriteString(text)
	}
}

// mergeToolCalls 按 index 合并工具调用分片。
//
// 上游的分片形态：首片带 id/name/arguments 开头，后续片只带 arguments 增量。
// 缺 index 的分片需要容错（id 已见过则延续，否则开新序号）。
func mergeToolCalls(existing []any, incoming []any) []any {
	byIndex := make(map[int]map[string]any, len(existing))
	order := make([]int, 0, len(existing)+len(incoming))
	for _, item := range existing {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		index := intFromAny(call["index"])
		byIndex[index] = call
		order = append(order, index)
	}

	nextIndex := len(order)
	for _, item := range incoming {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		index, okIndex := call["index"]
		var key int
		if okIndex {
			key = intFromAny(index)
		} else {
			// 缺 index：延续最近一次调用（上游偶发行为）。
			if len(order) > 0 {
				key = order[len(order)-1]
			} else {
				key = nextIndex
				nextIndex++
			}
		}
		target, okTarget := byIndex[key]
		if !okTarget {
			target = map[string]any{"index": key}
			byIndex[key] = target
			order = append(order, key)
		}
		mergeToolCallFields(target, call)
	}

	out := make([]any, 0, len(order))
	for _, key := range order {
		if call, okCall := byIndex[key]; okCall {
			out = append(out, call)
		}
	}
	return out
}

// mergeToolCallFields 把分片字段合并进目标调用。
func mergeToolCallFields(target, incoming map[string]any) {
	if id, okID := incoming["id"].(string); okID && strings.TrimSpace(id) != "" {
		target["id"] = id
	}
	if callType, okType := incoming["type"].(string); okType && strings.TrimSpace(callType) != "" {
		target["type"] = callType
	}
	incomingFunction, okFunction := incoming["function"].(map[string]any)
	if !okFunction {
		return
	}
	targetFunction, okTargetFunction := target["function"].(map[string]any)
	if !okTargetFunction {
		targetFunction = map[string]any{}
		target["function"] = targetFunction
	}
	if name, okName := incomingFunction["name"].(string); okName && strings.TrimSpace(name) != "" {
		targetFunction["name"] = name
	}
	if arguments, okArgs := incomingFunction["arguments"].(string); okArgs && arguments != "" {
		previous, _ := targetFunction["arguments"].(string)
		targetFunction["arguments"] = previous + arguments
	}
}

// sortToolCalls 按 index 排序输出（客户端依赖顺序）。
func sortToolCalls(calls []any) []any {
	out := append([]any(nil), calls...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			left, _ := out[j-1].(map[string]any)
			right, _ := out[j].(map[string]any)
			if intFromAny(left["index"]) <= intFromAny(right["index"]) {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// ensureUsageTotal 补齐缺失的 total_tokens。
//
// 不修改原 map：上游的 usage 可能被其他路径复用，就地改会有隐性耦合。
func ensureUsageTotal(usage map[string]any) map[string]any {
	if _, okTotal := usage["total_tokens"]; okTotal {
		return usage
	}
	prompt, okPrompt := toFloat64(usage["prompt_tokens"])
	completion, okCompletion := toFloat64(usage["completion_tokens"])
	if !okPrompt || !okCompletion {
		return usage
	}
	out := make(map[string]any, len(usage)+1)
	for key, value := range usage {
		out[key] = value
	}
	out["total_tokens"] = int64(prompt + completion)
	return out
}
