package zcode

// 模型目录：提供可用模型列表与元数据定义。
// 参考：/Users/jiandan/Workspaces/zcode2api/app/constants.py

import (
	"context"
	"strings"
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

// DefaultModels 返回 ZCode 的默认可用模型列表。
func DefaultModels() []Model {
	return []Model{
		{
			ID:            "GLM-5.3",
			Name:          "GLM-5.3",
			Description:   "ZCode GLM-5.3 旗舰代码与通用大模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "GLM-5.3-Flash",
			Name:          "GLM-5.3-Flash",
			Description:   "ZCode GLM-5.3 Flash 高速低延迟模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "GLM-5.2",
			Name:          "GLM-5.2",
			Description:   "ZCode GLM-5.2 代码助手模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "GLM-5-Turbo",
			Name:          "GLM-5-Turbo",
			Description:   "ZCode GLM-5 Turbo 快速模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "GLM-5.1",
			Name:          "GLM-5.1",
			Description:   "ZCode GLM-5.1 大模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "GLM-4.7",
			Name:          "GLM-4.7",
			Description:   "ZCode GLM-4.7 经典代码模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
	}
}

// NormalizeModelName 归一化模型大小写（映射到官方 Pascal/Camel 命名）。
func NormalizeModelName(model string) string {
	m := strings.TrimSpace(model)
	lower := strings.ToLower(m)
	switch lower {
	case "glm-5.3", "glm-5.3-pro":
		return "GLM-5.3"
	case "glm-5.3-flash":
		return "GLM-5.3-Flash"
	case "glm-5.2":
		return "GLM-5.2"
	case "glm-5-turbo", "glm-turbo":
		return "GLM-5-Turbo"
	case "glm-5.1":
		return "GLM-5.1"
	case "glm-4.7":
		return "GLM-4.7"
	default:
		return m
	}
}

// FetchModels 返回本供应商当前可用模型。
func FetchModels(ctx context.Context) ([]Model, error) {
	return DefaultModels(), nil
}
