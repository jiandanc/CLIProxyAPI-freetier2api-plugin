package opencodezen

// 本文件实现 OpenCode ZEN 的模型清单获取。
//
// ZEN 的模型清单**完全动态**：上游 GET {base}/v1/models 返回可用模型。
// 上游网关项目没有任何硬编码清单，本插件也不编一份——编出来的清单一旦
// 与上游不一致，用户会看到调用必失败的模型。
//
// 代价是探测需要一把有效的 API key。探测失败时返回空清单而不是兜底表：
// 空清单只会让 /v1/models 少几个条目（页面会提示去刷新），而一份编造的
// 清单会让用户以为模型可用。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/httpx"
)

// modelsHTTPTimeout 是模型清单请求的上限。
const modelsHTTPTimeout = 30 * time.Second

// Model 是一个 ZEN 上游模型。
type Model struct {
	// ID 是模型标识（上游原样，形如 claude-opus-5 / gpt-6-astra）。
	ID string
	// Name 是展示名（上游没给时用 ID）。
	Name string
	// ContextWindow 是上下文窗口（上游给了才有）。
	ContextWindow int64
	// MaxOutputTokens 是输出上限（上游给了才有）。
	MaxOutputTokens int64
	// SupportsTools 报告是否支持工具调用。
	SupportsTools bool
	// IsReasoning 报告是否是推理模型。
	IsReasoning bool
}

// FetchModels 拉取上游模型清单。
func FetchModels(ctx context.Context, baseURL, apiKey string) ([]Model, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("api key is required to list models")
	}
	endpoint := BaseURL(baseURL) + ModelsPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build models request: %w", errReq)
	}
	ApplyModelsHeaders(req, apiKey)

	resp, errDo := httpx.Client(ctx, modelsHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch models: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read models response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	return parseModels(body)
}

// parseModels 解析上游模型清单。
//
// 上游返回 OpenAI 风格的 {"data":[{"id":...}]}，但历史上也出现过裸数组
// 与 {"models":[...]} 两种形态，因此三种都接受。
func parseModels(body []byte) ([]Model, error) {
	type rawModel struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		DisplayName   string `json:"display_name"`
		ContextLength int64  `json:"context_length"`
		ContextWindow int64  `json:"context_window"`
		MaxOutput     int64  `json:"max_output_tokens"`
		MaxTokens     int64  `json:"max_tokens"`
		// 能力字段在不同形态里名字不同，全部收下后在下面归一。
		SupportsTools   *bool `json:"supports_tools"`
		ToolCall        *bool `json:"tool_call"`
		FunctionCalling *bool `json:"function_calling"`
		IsReasoning     *bool `json:"is_reasoning"`
		Reasoning       *bool `json:"reasoning"`
	}

	var entries []rawModel
	// 形态一：{"data":[...]}
	var wrapper struct {
		Data   []rawModel `json:"data"`
		Models []rawModel `json:"models"`
	}
	if errWrapper := json.Unmarshal(body, &wrapper); errWrapper == nil {
		switch {
		case len(wrapper.Data) > 0:
			entries = wrapper.Data
		case len(wrapper.Models) > 0:
			entries = wrapper.Models
		}
	}
	// 形态二：裸数组。
	if len(entries) == 0 {
		if errArray := json.Unmarshal(body, &entries); errArray != nil && !json.Valid(body) {
			return nil, fmt.Errorf("models response is not valid JSON: %w", errArray)
		}
	}

	out := make([]Model, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		// 只展示模型ID带 "free" 字符串的模型
		if !strings.Contains(strings.ToLower(id), "free") {
			continue
		}
		contextWindow := entry.ContextWindow
		if contextWindow == 0 {
			contextWindow = entry.ContextLength
		}
		maxOutput := entry.MaxOutput
		if maxOutput == 0 {
			maxOutput = entry.MaxTokens
		}
		name := firstNonEmpty(entry.DisplayName, entry.Name, id)
		out = append(out, Model{
			ID:              id,
			Name:            name,
			ContextWindow:   contextWindow,
			MaxOutputTokens: maxOutput,
			SupportsTools:   boolFrom(entry.SupportsTools, entry.ToolCall, entry.FunctionCalling),
			IsReasoning:     boolFrom(entry.IsReasoning, entry.Reasoning),
		})
	}
	return out, nil
}

// boolFrom 返回第一个非 nil 的布尔值（全 nil 时 false）。
func boolFrom(values ...*bool) bool {
	for _, value := range values {
		if value != nil {
			return *value
		}
	}
	return false
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
