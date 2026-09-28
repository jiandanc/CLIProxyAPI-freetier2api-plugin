package codearts

// 模型目录：提供 CodeArts 的内置与福利模型列表。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/upstream/models.go

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"freetier2api-plugin/internal/httpx"
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

// DefaultModels 返回 CodeArts 的默认模型清单。
func DefaultModels() []Model {
	return []Model{
		{
			ID:            "GLM-5.2",
			Name:          "GLM-5.2",
			Description:   "CodeArts 盘古代码大模型 GLM-5.2",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "glm-5.2",
			Name:          "GLM-5.2 (小写别名)",
			Description:   "CodeArts 盘古代码大模型 GLM-5.2",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "Qwen3-VL-235B",
			Name:          "Qwen3-VL-235B",
			Description:   "CodeArts 通义千问超大视觉模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "snap-chat",
			Name:          "Snap Chat",
			Description:   "CodeArts 默认代码对话模型",
			ContextWindow: 65536,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
	}
}

// FetchModels 尝试从上游拉取模型列表。
func FetchModels(ctx context.Context, cred *Credential) ([]Model, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return DefaultModels(), nil
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, SnapEngineApiHost+EpModelBuiltin, nil)
	if errReq != nil {
		return DefaultModels(), nil
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := httpx.Client(ctx, 15*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return DefaultModels(), nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return DefaultModels(), nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var result struct {
		Models []struct {
			ModelName string `json:"model_name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err == nil && len(result.Models) > 0 {
		var out []Model
		for _, m := range result.Models {
			if m.ModelName == "" {
				continue
			}
			out = append(out, Model{
				ID:            m.ModelName,
				Name:          m.ModelName,
				Description:   "CodeArts 内置模型",
				ContextWindow: 131072,
				MaxTokens:     8192,
				SupportsTools: true,
			})
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	return DefaultModels(), nil
}
