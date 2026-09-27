package workbuddy

// 本文件处理 tool_call 与 tool 结果的配对问题。
//
// 上游对工具配对的校验很严：调用与结果必须**双侧齐全**，否则返回
// 11148 "tool calls and tool results do not match" 并顶死整条会话。
//
// 两个真实来源的破坏：
//   - 客户端（如 Codex）会在并行调用的两份结果之间插入非 tool 消息；
//   - 客户端会发送「调用没有结果」或「结果没有调用」的残缺历史。

import (
	"encoding/json"
	"strings"
)

// repackToolResultBlocks 把插在 tool_calls 与其结果之间的非 tool 消息挪到整组之后。
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	→ assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// 只调顺序不改内容。无插入消息时返回原 slice（零改动零分配）。
func repackToolResultBlocks(messages []any) []any {
	if len(messages) < 3 {
		return messages
	}
	out := make([]any, 0, len(messages))
	changed := false

	for index := 0; index < len(messages); index++ {
		message, okMessage := messages[index].(map[string]any)
		if !okMessage {
			out = append(out, messages[index])
			continue
		}
		if !hasToolCalls(message) {
			out = append(out, messages[index])
			continue
		}

		// 收集这一组：调用消息本身 + 紧随其后的 tool 结果（中间可能夹着别的消息）。
		group := []any{messages[index]}
		var displaced []any
		cursor := index + 1
		for ; cursor < len(messages); cursor++ {
			next, okNext := messages[cursor].(map[string]any)
			if !okNext {
				break
			}
			role, _ := next["role"].(string)
			role = strings.ToLower(strings.TrimSpace(role))
			if role == "tool" {
				group = append(group, messages[cursor])
				continue
			}
			// 遇到下一组带 tool_calls 的 assistant 必须交还外层循环：
			// 否则它那批结果永远得不到重排（真实会话里正是这样漏掉的）。
			if hasToolCalls(next) {
				break
			}
			displaced = append(displaced, messages[cursor])
		}
		if len(displaced) > 0 {
			changed = true
			out = append(out, group...)
			out = append(out, displaced...)
		} else {
			out = append(out, group...)
		}
		index = cursor - 1
	}
	if !changed {
		return messages
	}
	return out
}

// cleanupOrphanToolCalls 裁剪配对不全的工具调用与结果。
//
// 规则：调用与结果的交集才保留。
//   - assistant.tool_calls 按交集对称裁剪；裁空则删除该键；
//   - role=tool 只在对应调用被保留时保留，否则整条删除。
//
// 历史上的错误做法是「批内全齐才整批保留，否则删掉整个 tool_calls 键」，
// 那会留下「无 tool_calls 的 assistant + 孤儿 tool」，正好命中上游的 11148。
func cleanupOrphanToolCalls(messages []any) []any {
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}

	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "assistant":
			for _, id := range toolCallIDs(message) {
				callIDs[id] = true
			}
		case "tool":
			if id, okID := message["tool_call_id"].(string); okID && strings.TrimSpace(id) != "" {
				resultIDs[strings.TrimSpace(id)] = true
			}
		}
	}
	if len(callIDs) == 0 && len(resultIDs) == 0 {
		return messages
	}

	keep := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keep[id] = true
		}
	}

	out := make([]any, 0, len(messages))
	changed := false
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			out = append(out, item)
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "assistant":
			if !hasToolCalls(message) {
				out = append(out, message)
				continue
			}
			filtered := filterToolCalls(message, keep)
			if len(filtered) == 0 {
				delete(message, "tool_calls")
				changed = true
			} else {
				message["tool_calls"] = filtered
			}
			out = append(out, message)
		case "tool":
			id, _ := message["tool_call_id"].(string)
			if !keep[strings.TrimSpace(id)] {
				changed = true
				continue
			}
			out = append(out, message)
		default:
			out = append(out, message)
		}
	}
	if !changed {
		return messages
	}
	return out
}

// toolCallIDs 取出 assistant 消息里的全部 tool_call id。
func toolCallIDs(message map[string]any) []string {
	calls, okCalls := message["tool_calls"].([]any)
	if !okCalls {
		return nil
	}
	out := make([]string, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		if id, okID := call["id"].(string); okID && strings.TrimSpace(id) != "" {
			out = append(out, strings.TrimSpace(id))
		}
	}
	return out
}

// hasToolCalls 报告消息是否带非空 tool_calls。
func hasToolCalls(message map[string]any) bool {
	calls, okCalls := message["tool_calls"].([]any)
	return okCalls && len(calls) > 0
}

// filterToolCalls 按 keep 集合裁剪 tool_calls。
func filterToolCalls(message map[string]any, keep map[string]bool) []any {
	calls, _ := message["tool_calls"].([]any)
	out := make([]any, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			continue
		}
		id, _ := call["id"].(string)
		if keep[strings.TrimSpace(id)] {
			out = append(out, call)
		}
	}
	return out
}

// IsTruncatedArguments 判断工具调用参数是否为残缺 JSON。
//
// 空串是合法的（无参工具）；非空但解析不了说明响应被截断。
func IsTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var value any
	return json.Unmarshal([]byte(trimmed), &value) != nil
}

// DropTruncatedToolCalls 丢弃参数残缺的工具调用。
//
// 在 finish_reason 为 length（输出被截断）或流没有正常结束时调用：
// 那种情况下的工具参数必然是不完整的 JSON，交给客户端只会得到解析错误。
// 能解析但类型不对的（标量/数组）不丢，留给客户端的 schema 校验处理。
func DropTruncatedToolCalls(calls []any) []any {
	out := make([]any, 0, len(calls))
	for _, item := range calls {
		call, okCall := item.(map[string]any)
		if !okCall {
			out = append(out, item)
			continue
		}
		function, okFunction := call["function"].(map[string]any)
		if !okFunction {
			out = append(out, call)
			continue
		}
		arguments, _ := function["arguments"].(string)
		if IsTruncatedArguments(arguments) {
			continue
		}
		out = append(out, call)
	}
	return out
}

// StripToolCallNames 让每个 tool_call 的 function.name 只出现在首片。
//
// 累加型客户端会做 name += delta.name，若每片都带 name 就会拼成
// "BashBashBash"；覆盖型客户端（name ?? state.name）则不会被空值清空。
// 因此「首片保留、后续删除该键」是同时满足两类客户端的做法。
func StripToolCallNames(frame map[string]any, seen map[int]bool) {
	choices, okChoices := frame["choices"].([]any)
	if !okChoices {
		return
	}
	for _, item := range choices {
		choice, okChoice := item.(map[string]any)
		if !okChoice {
			continue
		}
		delta, okDelta := choice["delta"].(map[string]any)
		if !okDelta {
			continue
		}
		calls, okCalls := delta["tool_calls"].([]any)
		if !okCalls {
			continue
		}
		for _, callItem := range calls {
			call, okCall := callItem.(map[string]any)
			if !okCall {
				continue
			}
			index := intFromAny(call["index"])
			function, okFunction := call["function"].(map[string]any)
			if !okFunction {
				continue
			}
			if seen[index] {
				delete(function, "name")
				continue
			}
			if _, okName := function["name"]; okName {
				seen[index] = true
			}
		}
	}
}

// intFromAny 把 JSON 数值转 int（缺失或非法返回 0）。
func intFromAny(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	}
	return 0
}
