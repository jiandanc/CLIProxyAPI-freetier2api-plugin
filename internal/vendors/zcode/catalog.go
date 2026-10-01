package zcode

// 模型目录。
//
// 上游的权威清单在 client/configs（免鉴权的公开端点），现行只有 GLM-5.3 与
// GLM-5.3-Flash 两个模型；早期版本的 GLM-5.x / GLM-4.7 已从套餐下线，继续
// 公布会让客户端选到必报 3006（model not allowed）的名字。因此这里以
// configs 为准，仅在拉取失败时回退内置清单。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/httpx"
)

// Model 是一个可用模型的元数据。
type Model struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	ContextWindow  int    `json:"context_window"`
	MaxTokens      int    `json:"max_tokens"`
	SupportsTools  bool   `json:"supports_tools"`
	SupportsImages bool   `json:"supports_images"`
	IsReasoning    bool   `json:"is_reasoning"`
}

// fallbackModels 是 configs 拉取失败时的内置清单（与现行上游一致）。
func fallbackModels() []Model {
	return []Model{
		{
			ID:            "GLM-5.3",
			Name:          "GLM-5.3",
			Description:   "ZCode 旗舰模型，适合复杂代码与长上下文任务",
			ContextWindow: 1000000,
			MaxTokens:     128000,
			SupportsTools: true,
			IsReasoning:   true,
		},
		{
			ID:             "GLM-5.3-Flash",
			Name:           "GLM-5.3-Flash",
			Description:    "ZCode 高速模型，支持图片输入",
			ContextWindow:  1000000,
			MaxTokens:      128000,
			SupportsTools:  true,
			SupportsImages: true,
			IsReasoning:    true,
		},
	}
}

// FetchModels 从上游拉取当前模型清单；失败时回退内置清单。
func FetchModels(ctx context.Context) ([]Model, error) {
	url := ZCodeOrigin + PathClientConfigs + "?app_version=" + ClientAppVersion
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errNew != nil {
		return fallbackModels(), nil
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, errDo := httpx.Client(ctx, 15*time.Second).Do(req)
	if errDo != nil {
		return fallbackModels(), nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil || resp.StatusCode >= 400 {
		return fallbackModels(), nil
	}

	models := parseConfigModels(body)
	if len(models) == 0 {
		return fallbackModels(), nil
	}
	return models, nil
}

// parseConfigModels 从 client/configs 响应里提取 builtinModels。
func parseConfigModels(body []byte) []Model {
	var payload struct {
		Data struct {
			BuiltinModels []struct {
				ModelID             string `json:"modelId"`
				Name                string `json:"name"`
				Description         string `json:"description"`
				ContextWindow       int    `json:"contextWindow"`
				MaxCompletionTokens int    `json:"maxCompletionTokens"`
				Capabilities        struct {
					Vision bool `json:"vision"`
				} `json:"capabilities"`
				Reasoning struct {
					Levels map[string]any `json:"levels"`
				} `json:"reasoning"`
			} `json:"builtinModels"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil
	}

	models := make([]Model, 0, len(payload.Data.BuiltinModels))
	for _, item := range payload.Data.BuiltinModels {
		id := strings.TrimSpace(item.ModelID)
		if id == "" {
			continue
		}
		models = append(models, Model{
			ID:             id,
			Name:           firstNonEmpty(item.Name, id),
			Description:    item.Description,
			ContextWindow:  item.ContextWindow,
			MaxTokens:      item.MaxCompletionTokens,
			SupportsTools:  true,
			SupportsImages: item.Capabilities.Vision,
			IsReasoning:    len(item.Reasoning.Levels) > 0,
		})
	}
	return models
}

// NormalizeModelName 归一化模型名大小写（上游对模型名大小写敏感）。
//
// 客户端常传小写别名，这里映射回官方写法；未知名字原样返回，让上游给出
// 明确的「模型不存在」而不是被静默改写。
func NormalizeModelName(model string) string {
	trimmed := strings.TrimSpace(model)
	switch strings.ToLower(trimmed) {
	case "glm-5.3", "glm-5.3-pro":
		return "GLM-5.3"
	case "glm-5.3-flash":
		return "GLM-5.3-Flash"
	default:
		return trimmed
	}
}
