package minimaxcode

import (
	"context"
	"strings"
)

// Model 是 MiniMax Code 支持的模型元数据。
type Model struct {
	ID            string
	Name          string
	Description   string
	ContextWindow int64
	MaxOutput     int64
	SupportsTools bool
	IsReasoning   bool
	UpstreamModel string
	Variant       string
}

// BundledModels 返回 MiniMax Code 的内置模型清单。
func BundledModels() []Model {
	return []Model{
		{
			ID:            "minimax-agent",
			Name:          "MiniMax Agent",
			Description:   "通用 Agent，自动规划并调用工具",
			ContextWindow: 128000,
			MaxOutput:     8192,
			SupportsTools: true,
			UpstreamModel: "",
		},
		{
			ID:            "minimax-m3",
			Name:          "MiniMax M3",
			Description:   "对话模式，响应更快",
			ContextWindow: 1000000,
			MaxOutput:     16384,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M3",
		},
		{
			ID:            "minimax-m3-thinking",
			Name:          "MiniMax M3 Thinking",
			Description:   "深度思考模式，推理内容走 reasoning_content",
			ContextWindow: 1000000,
			MaxOutput:     16384,
			SupportsTools: true,
			IsReasoning:   true,
			UpstreamModel: "MiniMax-M3",
			Variant:       "thinking",
		},
		{
			ID:            "minimax-m3.1-flash",
			Name:          "MiniMax M3.1 Flash Preview",
			Description:   "新一代 Flash 预览版，512K/1M 上下文，响应更快",
			ContextWindow: 512000,
			MaxOutput:     16384,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M3.1-Flash-Preview",
		},
		{
			ID:            "minimax-m3.1-flash-thinking",
			Name:          "MiniMax M3.1 Flash Preview Thinking",
			Description:   "M3.1 Flash 预览版的深度思考模式",
			ContextWindow: 512000,
			MaxOutput:     16384,
			SupportsTools: true,
			IsReasoning:   true,
			UpstreamModel: "MiniMax-M3.1-Flash-Preview",
			Variant:       "thinking",
		},
		{
			ID:            "minimax-m2.7",
			Name:          "MiniMax M2.7",
			Description:   "上一代对话模型",
			ContextWindow: 128000,
			MaxOutput:     8192,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M2.7",
		},
	}
}

// FindModel 根据客户端请求的模型 ID 查找模型配置。
func FindModel(modelID string) (Model, bool) {
	clean := strings.ToLower(strings.TrimSpace(modelID))
	// 如果带有前缀，剥离前缀
	if idx := strings.Index(clean, ":"); idx >= 0 {
		clean = clean[idx+1:]
	}
	for _, m := range BundledModels() {
		if strings.EqualFold(m.ID, clean) {
			return m, true
		}
	}
	// 默认回落到 minimax-agent
	return BundledModels()[0], false
}

// FetchModels 返回供宿主注册的模型清单。
func FetchModels(ctx context.Context, region Region) ([]Model, error) {
	_ = ctx
	_ = region
	return BundledModels(), nil
}
