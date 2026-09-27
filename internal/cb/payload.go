package cb

// 本文件实现「OpenAI 请求体 → CodeBuddy 请求体」的改写管线。
//
// **改写顺序有语义，不可调换**。每一步都对应一个曾经真实踩过的坑：
//
//	1. stream 强制 true          —— 上游拒绝非流式请求
//	2. max_completion_tokens 归一 —— 上游只认 max_tokens，别名会被静默忽略
//	3. stream_options 补默认      —— 不补就拿不到 usage（含真实扣费）
//	4. tool_choice 归一           —— 上游该字段是 string，对象形态直接 400
//	5. developer → system         —— 上游 role 白名单不含 developer，命中即 400
//	6. image_url 归一             —— 上游只接受对象形态，字符串会 400
//	7. tool 结果重排              —— 插队消息会让上游判定工具配对失败
//	8. 孤儿 tool_call 裁剪        —— 配对不全上游会顶死整条会话
//	9. 思维链注入                 —— deepseek 系不显式开思考就不返回思维链
//	10. 档位归一                  —— 非法档位 400
//	11. reasoning_content 回填    —— 部分租户要求 assistant 消息带非空 reasoning
//	12. 指纹脱敏                  —— 上游内容审核是逐字匹配

import "strings"

// PrepareOptions 是改写管线的输入。
type PrepareOptions struct {
	// Model 是目标模型裸名。非空时直接改写 payload["model"]。
	Model string
	// PromptMode 提示词模式：passthrough / custom / append。
	PromptMode string
	// PromptText 自定义或追加的提示词内容。
	PromptText string
	// Sanitize 开启指纹脱敏（第 12 步）。
	Sanitize bool
	// DefaultEfforts 是模型名 → 默认档位的表（来自模型目录）。
	DefaultEfforts map[string]string
	// SupportedEfforts 是模型名 → 支持档位列表的表（来自模型目录）。
	SupportedEfforts map[string][]string
	// CacheKey 是 prompt_cache_key；为空时跳过注入。
	CacheKey string
	// Global 表示这是 global 域请求（额外需要 system 兜底）。
	Global bool
}

// defaultDeepSeekEffort 是 deepseek 系模型缺失档位时的兜底默认档。
const defaultDeepSeekEffort = "high"

// effortRank 是档位的强度排序，用于「取不高于请求档的最高支持档」。
var effortRank = map[string]int{
	"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// PrepareBody 执行完整改写管线，返回可直接发给上游的请求体。
//
// 解析失败时原样返回输入：宁可不改写也不要让一次对话因为改写失败而中断，
// 上游的真实报错比本地的解析错误更有诊断价值。
func PrepareBody(body []byte, opts PrepareOptions) []byte {
	payload := decodeBodyMap(body)
	if payload == nil {
		return body
	}

	// 0) 出站 model 设为裸名（单 pass 改写，无需二次序列化）。
	if trimmedModel := strings.TrimSpace(opts.Model); trimmedModel != "" {
		payload["model"] = trimmedModel
	}

	// 1) 上游拒绝非流式。
	payload["stream"] = true

	// 2) max_completion_tokens → max_tokens。
	translateMaxCompletionTokens(payload)

	// 3) stream_options 缺省补 include_usage（不补就拿不到 usage）。
	if _, okOptions := payload["stream_options"]; !okOptions {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	// 4) tool_choice 归一。
	normalizeToolChoice(payload)

	// 5) developer → system。
	normalizeRoles(payload)

	// 5.5) 系统提示词处理（透传 / 替换 / 追加）。
	applyPromptMode(payload, opts.PromptMode, opts.PromptText)

	// 6) image_url 归一。
	normalizeImageURL(payload)

	if messages, okMessages := payload["messages"].([]any); okMessages {
		// 7) 工具结果重排。
		messages = repackToolResultBlocks(messages)
		// 8) 孤儿 tool_call 裁剪。
		messages = cleanupOrphanToolCalls(messages)
		payload["messages"] = messages
	}

	model, _ := payload["model"].(string)
	model = strings.TrimSpace(model)

	// 9) 思维链注入。
	injectThinking(payload, defaultEffortFor(opts.DefaultEfforts, model))

	// 10) 档位归一。
	normalizeReasoningEffort(payload, opts.SupportedEfforts[model])

	// 11) reasoning_content 回填。
	backfillReasoningContent(payload)

	// global 域的兜底 system：上游某些路径要求首条消息必须是 system。
	if opts.Global {
		ensureConsoleSystem(payload)
	}

	// prompt_cache_key：单项最大的费用优化，必须在最后注入（前面的步骤不依赖它）。
	if strings.TrimSpace(opts.CacheKey) != "" {
		if _, okKey := payload["prompt_cache_key"]; !okKey {
			payload["prompt_cache_key"] = opts.CacheKey
		}
	}

	// 12) 指纹脱敏（放最后：前面步骤可能引入需要净化的文本）。
	if opts.Sanitize {
		if messages, okMessages := payload["messages"].([]any); okMessages {
			sanitizeMessages(messages)
		}
	}

	encoded, errEncode := encodeBodyMap(payload)
	if errEncode != nil {
		return body
	}
	return encoded
}

// translateMaxCompletionTokens 把 max_completion_tokens 翻译成 max_tokens。
//
// 背景：OpenAI 规范里 max_tokens 已废弃、max_completion_tokens 是新字段，
// 但上游只认 max_tokens——透传别名会被忽略，然后回落到默认输出上限，
// 长任务被静默截断。
//
// 规则：
//   - 别名**无论是否翻译都删除**（减少体积与排障噪音）；
//   - 显式 max_tokens 存在时只删不译（显式优先）；
//   - 别名是正整数值才翻译，写成 int64 避免科学计数法（1.28e5）；
//   - 0/null/负数/非数值不翻译（0/null 语义是「未设置」，负数是非法值）。
func translateMaxCompletionTokens(payload map[string]any) {
	alias, hasAlias := payload["max_completion_tokens"]
	if !hasAlias {
		return
	}
	delete(payload, "max_completion_tokens")

	if _, hasExplicit := payload["max_tokens"]; hasExplicit {
		return
	}
	if value, okValue := positiveInt64(alias); okValue {
		payload["max_tokens"] = value
	}
}

// positiveInt64 把 JSON 数值转成正整数。
func positiveInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed <= 0 || typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case int64:
		if typed <= 0 {
			return 0, false
		}
		return typed, true
	case int:
		if typed <= 0 {
			return 0, false
		}
		return int64(typed), true
	}
	return 0, false
}

// normalizeToolChoice 把 tool_choice 归一成上游接受的形态。
//
// 上游该字段是 Go string，对象形态会 400 code=11101。
func normalizeToolChoice(payload map[string]any) {
	raw, hasChoice := payload["tool_choice"]
	if !hasChoice {
		return
	}
	if text, okText := raw.(string); okText {
		if strings.EqualFold(strings.TrimSpace(text), "none") {
			// "none" 表示不使用工具：删掉选择器与工具定义本身。
			delete(payload, "tool_choice")
			delete(payload, "tools")
			delete(payload, "functions")
			return
		}
		return
	}
	object, okObject := raw.(map[string]any)
	if !okObject {
		delete(payload, "tool_choice")
		return
	}
	choiceType, _ := object["type"].(string)
	switch strings.ToLower(strings.TrimSpace(choiceType)) {
	case "none":
		delete(payload, "tool_choice")
		delete(payload, "tools")
		delete(payload, "functions")
	case "auto", "required":
		payload["tool_choice"] = strings.ToLower(strings.TrimSpace(choiceType))
	case "function":
		// 两种形态：{"function":{"name":"x"}} 与 {"name":"x"}。
		name := ""
		if function, okFunction := object["function"].(map[string]any); okFunction {
			name, _ = function["name"].(string)
		}
		if strings.TrimSpace(name) == "" {
			name, _ = object["name"].(string)
		}
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			payload["tool_choice"] = trimmed
			return
		}
		delete(payload, "tool_choice")
	default:
		delete(payload, "tool_choice")
	}
}

// normalizeRoles 把 developer 归一为 system。
//
// 上游的 role 白名单不含 developer，命中即 HTTP 400 code=11-128。
// 只归一这一个值，其余 role 原样保留（不合并、不重排、不删除）。
func normalizeRoles(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, okRole := message["role"].(string)
		if !okRole {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			message["role"] = "system"
		}
	}
}

// normalizeImageURL 把字符串形态的 image_url 转成对象形态。
//
// 上游只接受对象，字符串会 400 code=11101 cannot unmarshal string into ImageContent。
// 已是对象的一律原样保留（不补 detail/mime_type 默认值：让上游返回真实错误，
// 而不是被我们补的默认值掩盖）。
func normalizeImageURL(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		parts, okParts := message["content"].([]any)
		if !okParts {
			continue
		}
		for _, part := range parts {
			partMap, okPartMap := part.(map[string]any)
			if !okPartMap {
				continue
			}
			if partType, okType := partMap["type"].(string); !okType || partType != "image_url" {
				continue
			}
			if url, okURL := partMap["image_url"].(string); okURL {
				partMap["image_url"] = map[string]any{"url": url}
			}
		}
	}
}

// ensureConsoleSystem 在 global 域缺少首条 system 时补一条兜底。
//
// 上游 global 的某些路径要求 messages[0] 是 system，缺失时行为不确定。
func ensureConsoleSystem(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages || len(messages) == 0 {
		return
	}
	if first, okFirst := messages[0].(map[string]any); okFirst {
		if role, okRole := first["role"].(string); okRole && strings.EqualFold(strings.TrimSpace(role), "system") {
			return
		}
	}
	payload["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}}, messages...)
}

// defaultEffortFor 查模型的默认档位。
func defaultEffortFor(table map[string]string, model string) string {
	if len(table) == 0 {
		return ""
	}
	return strings.TrimSpace(table[strings.TrimSpace(model)])
}

// applyPromptMode 处理系统提示词：custom 替换全部 system/developer；append 在开头连续 system 块后追加。
func applyPromptMode(payload map[string]any, mode, text string) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	text = strings.TrimSpace(text)
	if mode == "" || mode == "passthrough" || text == "" {
		return
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	if mode == "custom" {
		kept := make([]any, 0, len(messages)+1)
		kept = append(kept, map[string]any{"role": "system", "content": text})
		for _, item := range messages {
			msgMap, okMap := item.(map[string]any)
			if !okMap {
				kept = append(kept, item)
				continue
			}
			role, _ := msgMap["role"].(string)
			switch strings.ToLower(strings.TrimSpace(role)) {
			case "system", "developer":
				continue
			}
			kept = append(kept, item)
		}
		payload["messages"] = kept
	} else if mode == "append" {
		insertAt := 0
		for insertAt < len(messages) {
			msgMap, okMap := messages[insertAt].(map[string]any)
			if !okMap {
				break
			}
			role, _ := msgMap["role"].(string)
			switch strings.ToLower(strings.TrimSpace(role)) {
			case "system", "developer":
				insertAt++
				continue
			}
			break
		}
		out := make([]any, 0, len(messages)+1)
		out = append(out, messages[:insertAt]...)
		out = append(out, map[string]any{"role": "system", "content": text})
		out = append(out, messages[insertAt:]...)
		payload["messages"] = out
	}
}
