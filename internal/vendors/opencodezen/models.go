package opencodezen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// 本文件实现 OpenCode ZEN 的模型清单获取。
//
// ZEN 的模型清单**完全动态**：上游 GET {base}/v1/models 返回可用模型
// （实测只有 ID，没有元数据），官方展示用的上下文/输出/推理档位来自
// models.dev 目录，按 ID 合并 models.dev 目录。上游网关项目没有任何
// 硬编码清单，本插件也不编一份——编出来的清单一旦与上游不一致，用户
// 会看到调用必失败的模型。
//
// 代价是探测需要一把有效的 API key。探测失败时返回空清单而不是兜底表：
// 空清单只会让 /v1/models 少几个条目（页面会提示去刷新），而一份编造的
// 清单会让用户以为模型可用。

// modelsHTTPTimeout 是模型清单请求的上限。
const modelsHTTPTimeout = 30 * time.Second

// Model 是一个 ZEN 上游模型。
type Model struct {
	// ID 是模型标识（上游原样，形如 claude-opus-5 / gpt-6-astra）。
	ID string
	// Name 是展示名（上游没给时用 ID）。
	Name string
	// Description 是模型描述（来自 models.dev 目录）。
	Description string
	// ContextWindow 是上下文窗口（上游给了才有）。
	ContextWindow int64
	// MaxOutputTokens 是输出上限（上游给了才有）。
	MaxOutputTokens int64
	// SupportsTools 报告是否支持工具调用。
	SupportsTools bool
	// IsReasoning 报告是否是推理模型。
	IsReasoning bool
	// SupportsImages 报告是否支持图片输入（来自 models.dev 目录）。
	SupportsImages bool
	// Efforts 是可选的推理档位（来自 models.dev 目录）。
	Efforts []string
	// Credits 是计费展示文案。
	//
	// 只在上游目录明确「输入与输出单价都是 0」时记为「免费」；价格不是倍率，
	// 有价格的模型不进倍率列（显示「—」），不把美元价伪装成折扣倍数。
	Credits string
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

	resp, errDo := hostbridge.Client(ctx, modelsHTTPTimeout).Do(req)
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
		name := core.FirstNonEmpty(entry.DisplayName, entry.Name, id)
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

// 本文件实现 OpenCode ZEN 的模型元数据增强。
//
// 上游 GET {base}/v1/models 只返回模型 ID（实测无任何元数据字段），而
// 官方 OpenCode 客户端展示「上下文窗口 / 输出上限 / 推理档位」的数据源
// 是 models.dev 公开目录（https://models.dev/api.json）：其中 provider
// "opencode" 的模型 ID 与 ZEN /v1/models 一一对应，且带完整元数据。
//
// 策略：
//   - ZEN /v1/models 仍然决定「哪些模型可用」（动态探测，需要 key）；
//   - models.dev 只补「长什么样」（元数据，公开、无需认证）；
//   - 目录拉取失败不影响模型列表：保留原字段，由 adapter 走兜底值。
//   - 目录按 TTL 缓存：ModelsForAuth 会按凭证逐个触发探测，不打缓存会
//     让每次凭证刷新都去拉一遍几 MB 的目录。

const (
	// modelsDevURL 是 models.dev 的公开模型目录。
	modelsDevURL = "https://models.dev/api.json"
	// modelsDevTimeout 是目录请求的上限。
	modelsDevTimeout = 15 * time.Second
	// modelsDevCacheTTL 是成功结果的缓存时长。
	modelsDevCacheTTL = time.Hour
	// modelsDevNegativeTTL 是失败结果的缓存时长：探测按凭证逐个触发，
	// 短负缓存避免上游抖动时把失败放大成目录接口的请求风暴。
	modelsDevNegativeTTL = 5 * time.Minute
	// modelsDevProvider 是目录中对应 ZEN 的 provider ID。
	modelsDevProvider = "opencode"
)

// modelsDevEntry 是 models.dev 目录里的单个模型条目。
type modelsDevEntry struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	Reasoning        bool   `json:"reasoning"`
	ToolCall         bool   `json:"tool_call"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Limit struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	// Cost 是 models.dev 的价格（美元/百万 token，不是倍率）。指针用于区分
	// 「没有 cost 块」与「cost 全为 0」——前者不能当成免费。
	Cost *modelsDevCost `json:"cost"`
}

// modelsDevCost 是 models.dev 的单价（美元/百万 token）。
//
// 它不是倍率：换算成折扣倍数没有意义，只用来判定「是否免费」。
type modelsDevCost struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// modelsDevCatalog 是 provider 下的模型 ID → 条目表。
type modelsDevCatalog map[string]modelsDevEntry

// modelsDevCache 是目录缓存（成功与失败分开计时）。
//
// 属于 adapter 实例而不是包级变量：多个 App 实例各缓存各的，互不影响。
// 零值可用；nil 接收者表示不缓存（一次性调用直接打上游）。
type modelsDevCache struct {
	mu         sync.Mutex
	catalog    modelsDevCatalog
	validUntil time.Time
	failedNext time.Time
}

// fetch 拉取（或命中缓存）models.dev 目录。
func (c *modelsDevCache) fetch(ctx context.Context) (modelsDevCatalog, error) {
	now := time.Now()
	if c != nil {
		c.mu.Lock()
		if c.catalog != nil && now.Before(c.validUntil) {
			catalog := c.catalog
			c.mu.Unlock()
			return catalog, nil
		}
		failing := now.Before(c.failedNext)
		c.mu.Unlock()
		if failing {
			return nil, fmt.Errorf("models.dev catalog recently failed; negative cache")
		}
	}

	catalog, errFetch := loadModelsDevCatalog(ctx)
	if c != nil {
		now = time.Now()
		c.mu.Lock()
		if errFetch != nil {
			c.failedNext = now.Add(modelsDevNegativeTTL)
		} else {
			c.catalog = catalog
			c.validUntil = now.Add(modelsDevCacheTTL)
		}
		c.mu.Unlock()
	}
	return catalog, errFetch
}

// loadModelsDevCatalog 发起一次目录请求并解析出 ZEN 对应的 provider。
func loadModelsDevCatalog(ctx context.Context) (modelsDevCatalog, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build models.dev request: %w", errReq)
	}
	req.Header.Set("Accept", "application/json")

	resp, errDo := hostbridge.Client(ctx, modelsDevTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch models.dev: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read models.dev response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models.dev status %d", resp.StatusCode)
	}

	return parseModelsDevCatalog(body)
}

// parseModelsDevCatalog 解析目录响应并取出 ZEN 对应的 provider。
func parseModelsDevCatalog(body []byte) (modelsDevCatalog, error) {
	var payload map[string]struct {
		Models modelsDevCatalog `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("parse models.dev response: %w", errUnmarshal)
	}
	provider, okProvider := payload[modelsDevProvider]
	if !okProvider || len(provider.Models) == 0 {
		// 目录里还没有 ZEN provider 是可恢复状态：返回空表而不是报错，
		// 让调用方按「无元数据」继续走兜底。
		return modelsDevCatalog{}, nil
	}
	return provider.Models, nil
}

// enrich 用 models.dev 目录补全模型元数据。
//
// 只在字段为空时填充（ZEN /v1/models 未来若返回元数据则优先）；目录不可
// 用时只记一条 info，模型列表本身照常返回。
func (c *modelsDevCache) enrich(ctx context.Context, models []Model) {
	catalog, errCatalog := c.fetch(ctx)
	if errCatalog != nil {
		logger.Info("opencodezen: models.dev catalog unavailable, keep fallback metadata: %v", errCatalog)
		return
	}
	applyModelsDevCatalog(models, catalog)
}

// applyModelsDevCatalog 把目录条目合并进模型清单（纯函数，只填空字段）。
func applyModelsDevCatalog(models []Model, catalog modelsDevCatalog) {
	for i := range models {
		entry, okEntry := catalog[models[i].ID]
		if !okEntry {
			continue
		}
		model := &models[i]
		if model.ContextWindow == 0 && entry.Limit.Context > 0 {
			model.ContextWindow = entry.Limit.Context
		}
		if model.MaxOutputTokens == 0 && entry.Limit.Output > 0 {
			model.MaxOutputTokens = entry.Limit.Output
		}
		if !model.SupportsTools && entry.ToolCall {
			model.SupportsTools = true
		}
		if !model.IsReasoning && entry.Reasoning {
			model.IsReasoning = true
		}
		if len(model.Efforts) == 0 {
			model.Efforts = effortsFromModelsDev(entry)
		}
		if !model.SupportsImages {
			model.SupportsImages = hasImageInput(entry.Modalities.Input)
		}
		if (model.Name == "" || model.Name == model.ID) && entry.Name != "" {
			model.Name = entry.Name
		}
		if model.Description == "" && entry.Description != "" {
			model.Description = entry.Description
		}
		// 免费信号来自目录的 price 全 0；只做这一种判定，价格本身不进倍率列。
		if model.Credits == "" && entry.Cost != nil && entry.Cost.Input == 0 && entry.Cost.Output == 0 {
			model.Credits = "免费"
		}
	}
}

// effortsFromModelsDev 从目录的 reasoning_options 里取 effort 档位。
//
// type=effort 的 values 即官方展示的推理档位（如 low/high/max），顺序保留。
func effortsFromModelsDev(entry modelsDevEntry) []string {
	var efforts []string
	seen := make(map[string]bool)
	for _, option := range entry.ReasoningOptions {
		if option.Type != "effort" {
			continue
		}
		for _, value := range option.Values {
			trimmed := strings.TrimSpace(value)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			efforts = append(efforts, trimmed)
		}
	}
	return efforts
}

// hasImageInput 判断目录的输入模态是否含图片。
func hasImageInput(modalities []string) bool {
	for _, modality := range modalities {
		if strings.EqualFold(modality, "image") {
			return true
		}
	}
	return false
}
