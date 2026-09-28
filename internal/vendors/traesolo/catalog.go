package traesolo

// 模型目录：提供可用模型列表与元数据定义。
// 参考：/Users/jiandan/Workspaces/trae2api-more/internal/upstream/constants.go

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

// DefaultModels 返回 TRAE SOLO 的默认可用模型列表。
func DefaultModels() []Model {
	return []Model{
		{
			ID:            "glm-5.3",
			Name:          "GLM-5.3",
			Description:   "TRAE SOLO GLM-5.3 代码模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "glm-5.2",
			Name:          "GLM-5.2",
			Description:   "TRAE SOLO GLM-5.2 默认推理模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "deepseek-v3",
			Name:          "DeepSeek-V3",
			Description:   "TRAE SOLO DeepSeek-V3 模型",
			ContextWindow: 65536,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "claude-3-5-sonnet",
			Name:          "Claude 3.5 Sonnet",
			Description:   "TRAE SOLO Claude 3.5 Sonnet 模型",
			ContextWindow: 200000,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
	}
}

// FetchModels 从上游尝试探测模型清单，若失败则退回默认模型。
func FetchModels(ctx context.Context, cred *Credential) ([]Model, error) {
	if cred == nil || cred.AccessToken == "" {
		return DefaultModels(), nil
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, AgentHost+EpModels, nil)
	if errReq != nil {
		return DefaultModels(), nil
	}
	ApplySOLOHeaders(req, cred, false)

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
		Data struct {
			Models []struct {
				Name string `json:"model_name"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err == nil && len(result.Data.Models) > 0 {
		var out []Model
		for _, m := range result.Data.Models {
			if m.Name == "" {
				continue
			}
			out = append(out, Model{
				ID:            m.Name,
				Name:          m.Name,
				Description:   "TRAE SOLO 上游模型",
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
