package cb

// 本文件处理 DeepSeek 系的思维链。
//
// 上游对 deepseek 系模型标记了 thinkingFormat:"deepseek" 与
// requiresReasoningContentOnAssistantMessages，带来两个硬性要求：
//   - 开启思考必须显式带 thinking:{type:"enabled"} **且有档位**，
//     否则上游默认按「不思考」应答（思维链完全不返回）；
//   - assistant 消息必须带非空 reasoning，部分租户校验 len(reasoning) > 0，
//     缺失/null/空串一律 400（空白串可以，说明校验的是长度而非内容）。

import "strings"

// isDeepSeekModel 判断是否为 deepseek 系模型。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectThinking 为 deepseek 系模型注入思维链开关。
//
//	输入                           行为
//	thinking.type == "disabled"    尊重，删除档位字段
//	thinking.type == "enabled"     补默认档（若缺）
//	无 thinking / 非对象 / type 空  注入 enabled 并补默认档
func injectThinking(payload map[string]any, defaultEffort string) {
	model, _ := payload["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	thinking, okThinking := payload["thinking"].(map[string]any)
	if !okThinking {
		payload["thinking"] = map[string]any{"type": "enabled"}
		ensureDeepSeekEffort(payload, defaultEffort)
		return
	}
	thinkingType, _ := thinking["type"].(string)
	switch strings.ToLower(strings.TrimSpace(thinkingType)) {
	case "disabled":
		// 用户明确要关：尊重它，同时删掉档位（开着档位却关思考是矛盾组合）。
		delete(payload, "reasoning_effort")
		delete(payload, "reasoningEffort")
	case "enabled":
		ensureDeepSeekEffort(payload, defaultEffort)
	default:
		thinking["type"] = "enabled"
		ensureDeepSeekEffort(payload, defaultEffort)
	}
}

// ensureDeepSeekEffort 保证 deepseek 请求带一个档位。
//
// snake_case 优先、camelCase 兜底；两者都没有才写默认档。
func ensureDeepSeekEffort(payload map[string]any, defaultEffort string) {
	if _, okSnake := payload["reasoning_effort"]; okSnake {
		return
	}
	if _, okCamel := payload["reasoningEffort"]; okCamel {
		return
	}
	effort := strings.TrimSpace(defaultEffort)
	if effort == "" {
		effort = defaultDeepSeekEffort
	}
	payload["reasoning_effort"] = effort
}

// normalizeReasoningEffort 把档位降到模型支持的范围。
//
// 规则（支持表为空时一律透传，不做任何猜测）：
//   - 请求档位在支持列表里 → 原样透传；
//   - 否则取**不高于**请求档的最高支持档；
//   - 若所有支持档都高于请求档 → 取最低支持档（偏离最小）。
//
// 上游收到非法档位直接 400，而降级到最近的合法档位能让请求成功。
func normalizeReasoningEffort(payload map[string]any, supported []string) {
	if len(supported) == 0 {
		return
	}
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		raw, okRaw := payload[key].(string)
		if !okRaw {
			continue
		}
		requested := strings.ToLower(strings.TrimSpace(raw))
		if requested == "" {
			continue
		}
		if containsString(supported, requested) {
			continue
		}
		payload[key] = nearestEffort(supported, effortRank[requested])
	}
}

// nearestEffort 在支持列表里找与请求档位最接近的档位。
func nearestEffort(supported []string, requestedRank int) string {
	lowest := ""
	lowestRank := 99
	nearest := ""
	nearestRank := -1
	for _, item := range supported {
		rank, okRank := effortRank[item]
		if !okRank {
			continue
		}
		if rank < lowestRank {
			lowestRank = rank
			lowest = item
		}
		// 不高于请求档的最高者。
		if rank <= requestedRank && rank > nearestRank {
			nearestRank = rank
			nearest = item
		}
	}
	if nearest != "" {
		return nearest
	}
	// 所有支持档都高于请求档：取最低支持档（偏离最小）。
	if lowest != "" {
		return lowest
	}
	return supported[0]
}

// backfillReasoningContent 为 assistant 消息补齐 reasoning 字段。
//
// 门控：请求开了思考，或历史里已经有思维链痕迹。
// 只处理 assistant 消息：
//   - reasoning_content 已是字符串 → 不覆盖；
//   - 否则若 reasoning 是字符串 → 复制其值；
//   - 否则补空串。
//
// 同时镜像保证 reasoning 字段非空：缺失/null/空串归一为单空格
// （实测部分租户校验 len(reasoning) > 0，空白串可通过）。
func backfillReasoningContent(payload map[string]any) {
	if !reasoningGateOpen(payload) {
		return
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}

		if _, okString := message["reasoning_content"].(string); !okString {
			if reasoning, okReasoning := message["reasoning"].(string); okReasoning {
				message["reasoning_content"] = reasoning
			} else {
				message["reasoning_content"] = ""
			}
		}

		// 镜像字段：非空优先，皆无补一个空格。
		reasoning, _ := message["reasoning"].(string)
		if strings.TrimSpace(reasoning) == "" {
			if filled, okFilled := message["reasoning_content"].(string); okFilled && strings.TrimSpace(filled) != "" {
				message["reasoning"] = filled
			} else {
				message["reasoning"] = " "
			}
		}
	}
}

// reasoningGateOpen 判断是否需要回填思维链字段。
//
// 开思考时必须回填（上游会把历史里的 assistant 消息一起校验）；
// 历史里已经出现过思维链痕迹时也要回填，否则后续轮次会突然缺失字段。
func reasoningGateOpen(payload map[string]any) bool {
	if thinking, okThinking := payload["thinking"].(map[string]any); okThinking {
		if thinkingType, okType := thinking["type"].(string); okType &&
			strings.EqualFold(strings.TrimSpace(thinkingType), "enabled") {
			return true
		}
	}
	messages, _ := payload["messages"].([]any)
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		if reasoning, okReasoning := message["reasoning"].(string); okReasoning && strings.TrimSpace(reasoning) != "" {
			return true
		}
		if _, okContent := message["reasoning_content"]; okContent {
			return true
		}
	}
	return false
}
