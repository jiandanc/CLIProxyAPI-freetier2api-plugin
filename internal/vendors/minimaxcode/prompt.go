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

	return &Turn{
		Role:   role,
		Text:   strings.TrimSpace(strings.Join(textParts, "")),
		Images: images,
	}
}

// BuildPrompt 把多轮 OpenAI 对话消息压平为 MiniMax Agent 所需的单个 content 字符串。
func BuildPrompt(turns []*Turn) string {
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
			if text != "" {
				block = assistantTag + text
			}
		default: // "user", "tool", 或其它未知角色
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

	return strings.Join(parts, "\n\n")
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
