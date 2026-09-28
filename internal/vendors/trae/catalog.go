package trae

// 模型目录：提供 Trae 国内版与国际版的模型列表。
// 参考：/Users/jiandan/Workspaces/trae2api/src/realms.js

import (
	"context"
)

type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	ContextWindow int    `json:"context_window"`
	MaxTokens     int    `json:"max_tokens"`
	SupportsTools bool   `json:"supports_tools"`
	IsReasoning   bool   `json:"is_reasoning"`
}

// DefaultModelsFor 返回某区域的默认模型列表。
func DefaultModelsFor(r Region) []Model {
	if r == RegionGlobal {
		return []Model{
			{
				ID:            "gpt-5",
				Name:          "GPT-5",
				Description:   "Trae 国际版 GPT-5 模型",
				ContextWindow: 200000,
				MaxTokens:     8192,
				SupportsTools: true,
				IsReasoning:   true,
			},
			{
				ID:            "claude-3-5-sonnet",
				Name:          "Claude 3.5 Sonnet",
				Description:   "Trae 国际版 Claude 3.5 Sonnet 模型",
				ContextWindow: 200000,
				MaxTokens:     8192,
				SupportsTools: true,
				IsReasoning:   false,
			},
			{
				ID:            "gemini-1.5-pro",
				Name:          "Gemini 1.5 Pro",
				Description:   "Trae 国际版 Gemini 1.5 Pro 模型",
				ContextWindow: 1000000,
				MaxTokens:     8192,
				SupportsTools: true,
				IsReasoning:   false,
			},
			{
				ID:            "deepseek-v3",
				Name:          "DeepSeek-V3",
				Description:   "Trae 国际版 DeepSeek-V3 模型",
				ContextWindow: 65536,
				MaxTokens:     8192,
				SupportsTools: true,
				IsReasoning:   false,
			},
		}
	}

	return []Model{
		{
			ID:            "glm-5.3",
			Name:          "GLM-5.3",
			Description:   "Trae 国内版 GLM-5.3 代码模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "glm-5.2",
			Name:          "GLM-5.2",
			Description:   "Trae 国内版 GLM-5.2 默认推理模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "kimi-k3",
			Name:          "Kimi-K3",
			Description:   "Trae 国内版 Kimi-K3 长文本模型",
			ContextWindow: 200000,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "doubao-seed",
			Name:          "Doubao-Seed",
			Description:   "Trae 国内版 豆包模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "deepseek-v4",
			Name:          "DeepSeek-V4",
			Description:   "Trae 国内版 DeepSeek-V4 深度推理模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
	}
}

// FetchModels 返回本实例可用模型清单。
func FetchModels(ctx context.Context, r Region) ([]Model, error) {
	return DefaultModelsFor(r), nil
}
