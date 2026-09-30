package minimaxcode

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	systemTag    = "[系统指令]"
	assistantTag = "助手："
	userTag      = " 用户："
	textOnlyNote = "看图"
)

// Turn 代表解析后的单轮对话消息。
type Turn struct {
	Role   string
	Text   string
	Images []string
	// ToolCalls 是 assistant 消息的工具调用（OpenAI 形态）。
	ToolCalls []map[string]any
	// ToolCallID / ToolName 是 role=tool 结果消息的关联信息。
	ToolCallID string
	ToolName   string
}

// ParseTurn 从 OpenAI 格式消息解析出 Turn。
func ParseTurn(item any) *Turn {
	if item == nil {
		return nil
	}
	if s, ok := item.(string); ok {
		return &Turn{Role: "user", Text: s}
	}
	m, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	role, _ := m["role"].(string)
	role = strings.TrimSpace(role)
	if role != "system" && role != "assistant" && role != "user" && role != "tool" {
		role = "user"
	}

	var textParts []string
	var images []string

	if content, exists := m["content"]; exists && content != nil {
		switch c := content.(type) {
		case string:
			textParts = append(textParts, c)
		case []any:
			for _, part := range c {
				pm, ok := part.(map[string]any)
				if !ok {
					continue
				}
				pType, _ := pm["type"].(string)
				switch pType {
				case "text":
					if t, ok := pm["text"].(string); ok {
						textParts = append(textParts, t)
					}
				case "image_url", "input_image":
					if iu, ok := pm["image_url"]; ok {
						if uMap, ok := iu.(map[string]any); ok {
							if u, ok := uMap["url"].(string); ok && u != "" {
								images = append(images, u)
							}
						} else if uStr, ok := iu.(string); ok && uStr != "" {
							images = append(images, uStr)
						}
					}
				}
			}
		}
	}

	// 某些客户端把 images 放在顶层字段
	if rawImgs, ok := m["images"].([]any); ok {
		for _, img := range rawImgs {
			if s, ok := img.(string); ok && s != "" {
				images = append(images, s)
			}
		}
	}

	turn := &Turn{
		Role:   role,
		Text:   strings.TrimSpace(strings.Join(textParts, "")),
		Images: images,
	}

	if role == "assistant" {
		if calls, ok := m["tool_calls"].([]any); ok {
			for _, item := range calls {
				if call, ok := item.(map[string]any); ok {
					turn.ToolCalls = append(turn.ToolCalls, call)
				}
			}
		}
	}
	if role == "tool" {
		turn.ToolCallID, _ = m["tool_call_id"].(string)
		turn.ToolName, _ = m["name"].(string)
		if turn.ToolName == "" {
			turn.ToolName = firstNonEmptyStringOfMap(m, "tool_name")
		}
	}

	return turn
}

func firstNonEmptyStringOfMap(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// BuildPrompt 把多轮 OpenAI 对话消息压平为 MiniMax Agent 所需的单个 content 字符串。
func BuildPrompt(turns []*Turn) string {
	return BuildPromptWithTools(turns, nil, nil)
}

// BuildPromptWithTools 在压平对话之外，把历史工具交互与工具定义一并渲染进 prompt。
//
// tools 为 nil/空时与 BuildPrompt 完全一致（不含工具段）。
func BuildPromptWithTools(turns []*Turn, tools []any, toolChoice any) string {
	if len(turns) == 0 {
		return ""
	}

	var parts []string
	userTurnCount := 0

	for _, turn := range turns {
		if turn == nil {
			continue
		}
		text := strings.TrimSpace(turn.Text)
		images := turn.Images
		var block string

		switch turn.Role {
		case "system":
			if text != "" {
				block = systemTag + " " + text
			}
		case "assistant":
			var chunks []string
			if text != "" {
				chunks = append(chunks, assistantTag+text)
			}
			for _, call := range turn.ToolCalls {
				if rendered := renderToolCallBlock(call); rendered != "" {
					chunks = append(chunks, rendered)
				}
			}
			block = strings.Join(chunks, "\n\n")
		case "tool":
			// 带关联 id 的结果按工具结果块渲染（保留与调用的对应关系）；
			// 无 id 的孤儿子结果退回历史行为，当普通用户轮处理。
			if turn.ToolCallID != "" {
				block = renderToolResultBlock(turn.ToolCallID, turn.ToolName, text)
			} else if text != "" {
				userTurnCount++
				block = userTagFor(userTurnCount, len(turns)) + text
			}
		default: // "user" 或其它未知角色
			userTurnCount++
			prefix := userTagFor(userTurnCount, len(turns))
			body := text
			if body == "" && len(images) > 0 {
				body = textOnlyNote
			}
			if body != "" {
				block = prefix + body
			}
		}

		if len(images) > 0 {
			var imgLines []string
			if block != "" {
				imgLines = append(imgLines, block)
			}
			for n, url := range images {
				imgLines = append(imgLines, fmt.Sprintf("![已忽略的图片 %d](%s)", n, url))
			}
			block = strings.Join(imgLines, "\n")
		}

		if block != "" {
			parts = append(parts, block)
		}
	}

	prompt := strings.Join(parts, "\n\n")
	prompt += renderToolsSection(tools, toolChoice)
	return prompt
}

func userTagFor(number, total int) string {
	if total == 1 {
		return ""
	}
	return userTag + strconv.Itoa(number) + ". "
}

// EstimateTokens 估算文本 token 数。CJK 字符按 1 算，其他字符每 4 字节约合 1 token。
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	wide := 0
	for _, r := range text {
		if r > 0x2E80 {
			wide++
		}
	}
	totalChars := len([]rune(text))
	tokens := wide + (totalChars-wide)/4
	if tokens < 1 {
		return 1
	}
	return tokens
}
