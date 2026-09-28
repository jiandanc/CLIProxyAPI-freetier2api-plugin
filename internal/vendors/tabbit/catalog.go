package tabbit

// 模型目录：提供 Tabbit 的模型清单与离线列表。
// 参考：/Users/jiandan/Workspaces/tabbit2api/src/tabbit-bridge-core.js

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// DefaultModels 返回 Tabbit 默认推荐的可用模型列表。
func DefaultModels() []Model {
	return []Model{
		{
			ID:            "tabbit/priority",
			Name:          "Tabbit Priority Auto",
			Description:   "Tabbit 智能优选通道（自动故障重试）",
			ContextWindow: 200000,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "Claude-Opus-4.7",
			Name:          "Claude Opus 4.7",
			Description:   "Tabbit Claude-Opus-4.7 模型",
			ContextWindow: 200000,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "GPT-5.5",
			Name:          "GPT-5.5",
			Description:   "Tabbit GPT-5.5 旗舰模型",
			ContextWindow: 200000,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "Claude-Sonnet-4.6",
			Name:          "Claude Sonnet 4.6",
			Description:   "Tabbit Claude-Sonnet-4.6 模型",
			ContextWindow: 200000,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "GPT-5.4",
			Name:          "GPT-5.4",
			Description:   "Tabbit GPT-5.4 模型",
			ContextWindow: 200000,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "DeepSeek-V4-Pro",
			Name:          "DeepSeek V4 Pro",
			Description:   "Tabbit DeepSeek-V4-Pro 深度推理模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:            "GLM-5.1",
			Name:          "GLM-5.1",
			Description:   "Tabbit GLM-5.1 模型",
			ContextWindow: 131072,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
		{
			ID:            "Gemini-3.1-Pro",
			Name:          "Gemini 3.1 Pro",
			Description:   "Tabbit Gemini-3.1-Pro 模型",
			ContextWindow: 1000000,
			MaxTokens:     8192,
			SupportsTools: true,
			IsReasoning:   false,
		},
	}
}

// FetchModels 尝试从配置的 baseURL 或本地网关探测模型，失败时回退默认模型。
func FetchModels(ctx context.Context, cred *Credential) ([]Model, error) {
	baseURL := DefaultBaseURL
	if cred != nil && cred.BaseURL != "" {
		baseURL = cred.BaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+EpV1Models, nil)
	if errReq != nil {
		return DefaultModels(), nil
	}
	if cred != nil && cred.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cred.APIKey)
	}

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
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err == nil && len(result.Data) > 0 {
		var out []Model
		for _, m := range result.Data {
			if m.ID == "" {
				continue
			}
			out = append(out, Model{
				ID:            m.ID,
				Name:          firstNonEmpty(m.Name, m.ID),
				Description:   "Tabbit 模型",
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
