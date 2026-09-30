package minimaxcode

// 本文件实现「文本协议工具桥接」：上游 agent 端点只有一个 content 字符串，
// 没有原生 tools 参数，因此 Claude Code 的工具调用必须以文本标记承载：
//
//   - 请求侧：工具定义、历史 tool_calls 与 tool 结果都渲染进扁平 prompt；
//   - 响应侧：从模型输出中解析标记块，还原成 OpenAI tool_calls。
//
// 标记选 64 字符的随机定界符而非常见 XML 标签：正文撞上的概率为零，
// 流式 hold-back 的窗口也因此只有几十字节。指令教一种格式，
// 解析另兼容 ```tool_call 围栏——模型对围栏有天然偏好，与其纠正不如接受。
// 历史里的标记块本身就是 few-shot 样本，模型下一轮照着输出同一格式。

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	toolCallStart = "<minimax-tool-call>"
	toolCallEnd   = "</minimax-tool-call>"
	toolFence     = "```tool_call"
	toolFenceEnd  = "```"
)

// ToolCall 是还原后的 OpenAI 形态工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// renderToolsSection 把工具定义与调用说明渲染成 prompt 末尾的指令段。
func renderToolsSection(tools []any, toolChoice any) string {
	if len(tools) == 0 || isToolChoiceNone(toolChoice) {
		return ""
	}

	defs := make([]map[string]any, 0, len(tools))
	for _, item := range tools {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		if fn == nil {
			fn = m
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		entry := map[string]any{"name": name}
		if desc, ok := fn["description"].(string); ok && desc != "" {
			entry["description"] = desc
		}
		if schema, ok := fn["parameters"]; ok && schema != nil {
			entry["parameters"] = schema
		}
		defs = append(defs, entry)
	}
	if len(defs) == 0 {
		return ""
	}
	defsJSON, _ := json.Marshal(defs)

	var b strings.Builder
	b.WriteString("\n\n[可用工具]\n")
	b.WriteString("以下是你可以调用的工具定义（JSON）：\n")
	b.WriteString(string(defsJSON))
	b.WriteString("\n\n[工具调用格式]\n")
	b.WriteString("需要调用工具时，输出如下标记块（标记内容必须是单行 JSON，arguments 必须符合工具的 parameters schema）。")
	b.WriteString("可连续输出多个标记块进行并行调用；标记块之外可输出简短说明文字。输出标记块后停止生成，等待工具结果。\n")
	b.WriteString(toolCallStart + "\n")
	b.WriteString(`{"name":"工具名","arguments":{...}}` + "\n")
	b.WriteString(toolCallEnd + "\n")

	switch choice := normalizeToolChoice(toolChoice).(type) {
	case string:
		if choice == "required" {
			b.WriteString("本轮回复必须至少输出一个工具调用标记块。\n")
		}
	case map[string]any:
		if name, ok := choice["name"].(string); ok && name != "" {
			b.WriteString("本轮回复必须调用工具 " + name + "。\n")
		}
	}
	return b.String()
}

// isToolChoiceNone 判断 tool_choice 是否为 "none"。
func isToolChoiceNone(toolChoice any) bool {
	s, ok := toolChoice.(string)
	return ok && strings.EqualFold(strings.TrimSpace(s), "none")
}

// normalizeToolChoice 把 OpenAI tool_choice 归一化为 string 或 {name}。
func normalizeToolChoice(toolChoice any) any {
	switch choice := toolChoice.(type) {
	case string:
		return choice
	case map[string]any:
		if t, _ := choice["type"].(string); t == "function" {
			if fn, ok := choice["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					return map[string]any{"name": name}
				}
			}
		}
		if name, ok := choice["name"].(string); ok {
			return map[string]any{"name": name}
		}
	}
	return nil
}

// renderToolCallBlock 把一次历史工具调用渲染成标记块（few-shot：历史即格式样本）。
func renderToolCallBlock(call map[string]any) string {
	fn, _ := call["function"].(map[string]any)
	if fn == nil {
		return ""
	}
	name, _ := fn["name"].(string)
	if strings.TrimSpace(name) == "" {
		return ""
	}
	args, _ := fn["arguments"].(string)
	args = strings.TrimSpace(args)
	if args == "" {
		args = "{}"
	}
	var line strings.Builder
	line.WriteString(`{"name":`)
	nameJSON, _ := json.Marshal(name)
	line.Write(nameJSON)
	line.WriteString(`,"arguments":`)
	if json.Valid([]byte(args)) {
		line.WriteString(args)
	} else {
		// 残缺参数原样字符串化，交给解析端按字符串 arguments 接收。
		escaped, _ := json.Marshal(args)
		line.Write(escaped)
	}
	line.WriteString("}")
	if id, _ := call["id"].(string); strings.TrimSpace(id) != "" {
		// id 放进块尾的独立键，保持 name/arguments 主体与协议示例一致。
		encoded := line.String()
		encoded = strings.TrimSuffix(encoded, "}") + `,"id":` + mustJSONString(id) + "}"
		return toolCallStart + "\n" + encoded + "\n" + toolCallEnd
	}
	return toolCallStart + "\n" + line.String() + "\n" + toolCallEnd
}

// renderToolResultBlock 把 role=tool 的结果渲染成历史块。
func renderToolResultBlock(id, name, content string) string {
	var b strings.Builder
	b.WriteString("[工具结果")
	if id != "" {
		b.WriteString(" id=" + id)
	}
	if name != "" {
		b.WriteString(" name=" + name)
	}
	b.WriteString("]\n")
	b.WriteString(content)
	return b.String()
}

func mustJSONString(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

// parseToolCalls 从模型输出文本中还原工具调用，并返回剥离标记后的正文。
func parseToolCalls(text string) (string, []ToolCall) {
	if text == "" {
		return text, nil
	}
	if !strings.Contains(text, toolCallStart) && !strings.Contains(text, toolFence) {
		// 快速路径：无标记时仅探测「裸 JSON 工具对象」这一个特例。
		if trimmed := strings.TrimSpace(text); looksLikeToolCallObject(trimmed) {
			if call, ok := decodeToolCallJSON([]byte(trimmed)); ok {
				return "", []ToolCall{call}
			}
		}
		return text, nil
	}

	var calls []ToolCall
	var b strings.Builder
	rest := text
	for {
		startIdx, startLen := indexToolCallStart(rest)
		if startIdx < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:startIdx])
		after := rest[startIdx+startLen:]
		endIdx := indexToolCallEnd(after)
		var blockBody string
		if endIdx >= 0 {
			blockBody = after[:endIdx]
			// 闭标记两种形态都剥（开闭可以不成对，如围栏开裸闭）。
			rest = after[endIdx:]
			if strings.HasPrefix(rest, toolCallEnd) {
				rest = rest[len(toolCallEnd):]
			} else if strings.HasPrefix(rest, toolFenceEnd) {
				rest = rest[len(toolFenceEnd):]
			}
		} else {
			// 未闭合：剩余部分当作块体尽力解析。
			blockBody = after
			rest = ""
		}
		if call, ok := decodeToolCallBlock(blockBody); ok {
			calls = append(calls, call)
		} else {
			// 解析失败的块原样保留为正文，不吞内容。
			if startLen == len(toolFence) {
				b.WriteString(toolFence)
			} else {
				b.WriteString(toolCallStart)
			}
			b.WriteString(blockBody)
			if endIdx >= 0 {
				b.WriteString(toolCallEnd)
			}
		}
	}
	return b.String(), calls
}

// indexToolCallStart 返回最近的开标记位置（裸标记优先于围栏）。
func indexToolCallStart(s string) (int, int) {
	bare := strings.Index(s, toolCallStart)
	fence := strings.Index(s, toolFence)
	switch {
	case bare >= 0 && (fence < 0 || bare <= fence):
		return bare, len(toolCallStart)
	case fence >= 0:
		return fence, len(toolFence)
	default:
		return -1, 0
	}
}

// indexToolCallEnd 返回闭标记位置；两种形态的闭标记都识别。
func indexToolCallEnd(s string) int {
	idxBare := strings.Index(s, toolCallEnd)
	idxFence := strings.Index(s, toolFenceEnd)
	switch {
	case idxBare >= 0 && (idxFence < 0 || idxBare <= idxFence):
		return idxBare
	case idxFence >= 0:
		return idxFence
	default:
		return -1
	}
}

// looksLikeToolCallObject 判断文本是否恰为一个工具调用 JSON 对象。
func looksLikeToolCallObject(trimmed string) bool {
	return strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") &&
		strings.Contains(trimmed, `"name"`)
}

// decodeToolCallBlock 从标记块体中解出一次工具调用。
func decodeToolCallBlock(body string) (ToolCall, bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ToolCall{}, false
	}
	// 块体可能被围栏或说明文字包着：截取第一个 { 到最后一个 }。
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end <= start {
		return ToolCall{}, false
	}
	return decodeToolCallJSON([]byte(trimmed[start : end+1]))
}

// decodeToolCallJSON 解析 {"name":...,"arguments":...} 形态。
func decodeToolCallJSON(raw []byte) (ToolCall, bool) {
	var obj map[string]any
	if errUnmarshal := json.Unmarshal(raw, &obj); errUnmarshal != nil {
		return ToolCall{}, false
	}
	name, _ := obj["name"].(string)
	if strings.TrimSpace(name) == "" {
		return ToolCall{}, false
	}
	call := ToolCall{Name: name}
	call.ID, _ = obj["id"].(string)
	switch args := obj["arguments"].(type) {
	case string:
		call.Arguments = args
	case map[string]any:
		encoded, errMarshal := json.Marshal(args)
		if errMarshal != nil {
			return ToolCall{}, false
		}
		call.Arguments = string(encoded)
	default:
		call.Arguments = "{}"
	}
	if strings.TrimSpace(call.Arguments) == "" {
		call.Arguments = "{}"
	}
	return call, true
}

// maxToolMarkerSpan 是开标记的最大长度，流式 hold-back 至少保留的尾部字节数。
var maxToolMarkerSpan = maxInt(len(toolCallStart), len(toolFence))

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// toolStreamParser 增量解析流式文本中的工具标记。
//
// 工作方式：持有尾部若干字节（不足以判定标记是否出现），可安全判定的部分
// 作为正文发射；一旦看到开标记进入捕获模式，捕获到闭标记时解析出工具调用
// 并把块从正文中剔除。flush 在流结束时交出剩余正文。
type toolStreamParser struct {
	hold      strings.Builder
	capturing bool
	capture   strings.Builder
	calls     []ToolCall
	// rejected 记录解析失败后被退回正文的块（用于 flush 兜底）。
	pendingEmit strings.Builder
}

// push 送入一段文本，返回可以安全发给客户端的正文增量。
func (p *toolStreamParser) push(delta string) string {
	p.hold.WriteString(delta)
	var out strings.Builder
	for {
		hold := p.hold.String()
		if !p.capturing {
			idx, markerLen := indexToolCallStart(hold)
			if idx >= 0 {
				pending := hold[:idx]
				out.WriteString(pending)
				p.hold.Reset()
				p.hold.WriteString(hold[idx+markerLen:])
				p.capturing = true
				p.capture.Reset()
				continue
			}
			// 无标记：发送安全前缀，保留可能跨分片的尾部。
			safe := len(hold) - (maxToolMarkerSpan - 1)
			// 切点必须落在 rune 边界上，否则多字节字符会被截断成非法 UTF-8。
			for safe > 0 && safe < len(hold) && !utf8.RuneStart(hold[safe]) {
				safe--
			}
			if safe > 0 {
				out.WriteString(hold[:safe])
				p.hold.Reset()
				p.hold.WriteString(hold[safe:])
			}
			return out.String()
		}
		// 捕获模式：找闭标记。标记可能横跨 capture 与 hold 的边界
		// （开标记字节先落入 capture，闭标记的后半截才到达），因此在拼接体上搜。
		combined := p.capture.String() + hold
		endIdx := indexToolCallEnd(combined)
		if endIdx < 0 {
			// 防御：捕获体异常膨胀时放弃解析，整体退回正文。
			if len(combined) > 1<<20 {
				p.capturing = false
				p.pendingEmit.WriteString(toolCallStart)
				p.pendingEmit.WriteString(combined)
				p.hold.Reset()
				p.capture.Reset()
				continue
			}
			p.capture.Reset()
			p.capture.WriteString(combined)
			p.hold.Reset()
			return out.String()
		}
		blockBody := combined[:endIdx]
		rest := combined[endIdx:]
		// 按开标记形态剥闭标记。
		// 由于进入捕获时已消费开标记，这里两种闭标记都可能。
		if strings.HasPrefix(rest, toolCallEnd) {
			rest = rest[len(toolCallEnd):]
		} else if strings.HasPrefix(rest, toolFenceEnd) {
			rest = rest[len(toolFenceEnd):]
		}
		p.capturing = false
		p.capture.Reset()
		p.hold.Reset()
		p.hold.WriteString(rest)
		if call, ok := decodeToolCallBlock(blockBody); ok {
			p.calls = append(p.calls, call)
		} else {
			// 解析失败：块退回正文（作为普通文本发给客户端）。
			p.pendingEmit.WriteString(toolCallStart)
			p.pendingEmit.WriteString(blockBody)
			p.pendingEmit.WriteString(toolCallEnd)
		}
	}
}

// flush 流结束时交出剩余正文与已还原的工具调用。
func (p *toolStreamParser) flush() (string, []ToolCall) {
	var out strings.Builder
	out.WriteString(p.pendingEmit.String())
	p.pendingEmit.Reset()
	rest := p.hold.String()
	if p.capturing {
		// 未闭合的块：能解析就当作调用，不能就退回正文。
		blockBody := p.capture.String() + rest
		if call, ok := decodeToolCallBlock(blockBody); ok {
			p.calls = append(p.calls, call)
		} else {
			out.WriteString(toolCallStart)
			out.WriteString(blockBody)
		}
	} else if rest != "" {
		// 正文尾部若恰是裸工具对象也兜一层。
		if trimmed := strings.TrimSpace(rest); looksLikeToolCallObject(trimmed) && len(p.calls) == 0 && strings.TrimSpace(out.String()) == "" {
			if call, ok := decodeToolCallJSON([]byte(trimmed)); ok {
				p.calls = append(p.calls, call)
			} else {
				out.WriteString(rest)
			}
		} else {
			out.WriteString(rest)
		}
	}
	p.hold.Reset()
	p.capture.Reset()
	p.capturing = false
	return out.String(), p.calls
}
