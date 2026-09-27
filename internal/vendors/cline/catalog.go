package cline

// 本文件实现 Cline 的模型清单。
//
// 上游有一个**无需认证**的推荐模型接口，返回三个数组：
// recommended（推荐）/ free（免费）/ clinePass（订阅可用）。
// 实测免费模型的 tags 可能为空，因此「是否免费」必须按**在哪个数组里**判定，
// 只看 tags 会把免费模型误判成付费。
//
// 探测失败时退回内置兜底清单：没有模型注册比注册一份可能过时的清单更糟
// （客户端会看不到任何模型）。

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
const modelsHTTPTimeout = 15 * time.Second

// Model 是一个 Cline 上游模型。
type Model struct {
	// ID 是模型标识，形如 provider/model（上游要求必须带斜杠）。
	ID string
	// Name 是展示名。
	Name string
	// Description 是说明。
	Description string
	// ContextWindow 是上下文窗口。
	ContextWindow int64
	// MaxOutputTokens 是输出上限。
	MaxOutputTokens int64
	// Cost 是计费档：free / pass。
	Cost string
	// SupportsTools 报告是否支持工具调用。
	SupportsTools bool
	// IsReasoning 报告是否是推理模型。
	IsReasoning bool
}

// 计费档常量。
const (
	CostFree = "free"
	CostPass = "pass"
)

// FetchModels 拉取上游推荐模型清单（无需认证）。
func FetchModels(ctx context.Context, baseURL string) ([]Model, error) {
	endpoint := BaseURL(baseURL) + ModelsPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build models request: %w", errReq)
	}
	req.Header.Set("Accept", "application/json")

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
	models, errParse := parseModels(body)
	if errParse != nil {
		return nil, errParse
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("models response contained no usable entries")
	}
	return models, nil
}

// rawModel 是上游模型条目的原始形态。
//
// 字段名是**上游原样的 camelCase**，不同数组里出现的字段略有差异，
// 因此全部收下后再归一。
type rawModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	DisplayName   string   `json:"displayName"`
	Description   string   `json:"description"`
	ContextWindow int64    `json:"contextWindow"`
	MaxTokens     int64    `json:"maxTokens"`
	MaxOutput     int64    `json:"maxOutputTokens"`
	Tags          []string `json:"tags"`
}

// parseModels 解析上游三个数组并归一。
func parseModels(body []byte) ([]Model, error) {
	var payload struct {
		Recommended []rawModel `json:"recommended"`
		Free        []rawModel `json:"free"`
		ClinePass   []rawModel `json:"clinePass"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("models response is not valid JSON: %w", errUnmarshal)
	}

	// 去重键用模型 ID：同一个模型可能同时出现在多个数组里。
	seen := map[string]bool{}
	out := make([]Model, 0, len(payload.Recommended)+len(payload.Free)+len(payload.ClinePass))

	appendAll := func(entries []rawModel, cost string, isFreeArray bool) {
		for _, entry := range entries {
			id := strings.TrimSpace(entry.ID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			model := Model{
				ID:              id,
				Name:            firstNonEmpty(entry.DisplayName, entry.Name, id),
				Description:     strings.TrimSpace(entry.Description),
				ContextWindow:   entry.ContextWindow,
				MaxOutputTokens: firstNonZero(entry.MaxOutput, entry.MaxTokens),
				Cost:            cost,
				SupportsTools:   true,
			}
			// 「是否免费」按数组判定：实测 free 数组里的 tags 可能为空，
			// 只看 tags 会把免费模型误判成付费。
			if isFreeArray || hasFreeTag(entry.Tags) {
				model.Cost = CostFree
			}
			model.IsReasoning = hasReasoningTag(entry.Tags)
			out = append(out, model)
		}
	}

	// 顺序有讲究：free 先入表，这样同名的模型以「免费」档为准
	// （用户更关心它免费可用）。
	appendAll(payload.Free, CostFree, true)
	appendAll(payload.ClinePass, CostPass, false)
	appendAll(payload.Recommended, CostPass, false)
	return out, nil
}

// hasFreeTag 报告 tags 里是否标了 FREE。
func hasFreeTag(tags []string) bool {
	for _, tag := range tags {
		if strings.EqualFold(strings.TrimSpace(tag), "free") {
			return true
		}
	}
	return false
}

// hasReasoningTag 报告 tags 里是否标了推理。
func hasReasoningTag(tags []string) bool {
	for _, tag := range tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "reasoning", "thinking":
			return true
		}
	}
	return false
}

// BundledClineModels 是内置的兜底模型清单。
//
// 来源：cline2api 的 builtinModels 常量（离线兜底），不是凭空构造的。
// 真实可用集合以「刷新模型」拉到的实时清单为准。
func BundledClineModels() []Model {
	return []Model{
		{ID: "cline-free/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Cost: CostFree, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
		{ID: "cline-free/gemini-3.8-flash", Name: "Gemini 3.8 Flash", Cost: CostFree, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
		{ID: "cline-free/mimo-v2.6-flash", Name: "MiMo v2.6 Flash", Cost: CostFree, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
		{ID: "cline-pass/glm-5.3", Name: "GLM-5.3", Cost: CostPass, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
		{ID: "cline-pass/kimi-k3", Name: "Kimi K3", Cost: CostPass, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
		{ID: "cline-pass/deepseek-v4-pro", Name: "DeepSeek V4 Pro", Cost: CostPass, ContextWindow: 200000, MaxOutputTokens: 32768, SupportsTools: true, IsReasoning: true},
		{ID: "cline-pass/qwen3.8-max", Name: "Qwen3.8 Max", Cost: CostPass, ContextWindow: 200000, MaxOutputTokens: 16384, SupportsTools: true},
	}
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

// firstNonZero 返回第一个非零数值。
func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
