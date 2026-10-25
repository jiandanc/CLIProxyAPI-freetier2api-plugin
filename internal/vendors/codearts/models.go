package codearts

// 模型目录：提供 CodeArts 的内置与福利模型列表。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/upstream/models.go

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	ContextWindow int    `json:"context_window"`
	MaxTokens     int    `json:"max_tokens"`
	SupportsTools bool   `json:"supports_tools"`
	// SupportsImages 来自内置目录的 supports_images（视觉模型才有这个位）。
	SupportsImages bool `json:"supports_images,omitempty"`
	IsReasoning    bool `json:"is_reasoning"`
	// Credits 是上游 credit[].ratio_display 的基础档文案（形如 "0.7x"）。
	// 空串表示上游没给——不能当成 0（0 在控制台的含义是「免费」）。
	Credits string `json:"credits,omitempty"`
	// Benefit 标记该模型走每日免费福利额度（请求头 maas_type: benefit）。
	Benefit bool `json:"benefit,omitempty"`
}

// gatewayEnvelope 是福利网关接口的统一信封。
type gatewayEnvelope struct {
	ErrorCode string          `json:"error_code"`
	ErrorMsg  string          `json:"error_msg"`
	Result    json.RawMessage `json:"result"`
}

// FetchBenefitModels 拉取每日免费福利通道的模型清单。
//
// 上游把「每日免费额度」与「订阅积分」分成两条通道：同一个模型名在两条通道里
// 都可能出现，区别只在请求头 maas_type。哪些模型能用免费额度完全由这份清单决定
// （积分通道的模型不在本插件支持范围内）。
func FetchBenefitModels(ctx context.Context, cred *Credential) ([]Model, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return nil, nil
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, BenefitHost+EpBenefitConfig, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 20*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts benefit models failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	var envelope gatewayEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode benefit gateway config: %w", err)
	}
	if envelope.ErrorCode != "" && envelope.ErrorCode != "0000" {
		return nil, fmt.Errorf("benefit gateway %s: %s", envelope.ErrorCode, envelope.ErrorMsg)
	}
	var config struct {
		Models []struct {
			ModelID       string `json:"model_id"`
			ModelName     string `json:"model_name"`
			ContextWindow int    `json:"context_window"`
			MaxTokens     int    `json:"max_tokens"`
		} `json:"models"`
	}
	if err := json.Unmarshal(envelope.Result, &config); err != nil {
		return nil, fmt.Errorf("decode benefit model list: %w", err)
	}

	models := make([]Model, 0, len(config.Models))
	for _, item := range config.Models {
		id := strings.TrimSpace(item.ModelID)
		if id == "" {
			continue
		}
		contextWindow, maxTokens := item.ContextWindow, item.MaxTokens
		if contextWindow <= 0 || maxTokens <= 0 {
			hintContext, hintOutput := fallbackLimits(id)
			if contextWindow <= 0 {
				contextWindow = hintContext
			}
			if maxTokens <= 0 {
				maxTokens = hintOutput
			}
		}
		models = append(models, Model{
			ID:            id,
			Name:          core.FirstNonEmpty(item.ModelName, id),
			Description:   "CodeArts 每日免费福利额度模型",
			ContextWindow: contextWindow,
			MaxTokens:     capOutputTokens(maxTokens),
			SupportsTools: true,
			Benefit:       true,
		})
	}
	return models, nil
}

// DefaultMaxOutputTokens 是 CodeArts 上游单次输出额度的硬顶。
//
// 实测：max_tokens 的**取值**参与上游预校验——65536 放行，65537 起整条请求
// 回 InferHub.001001005.400（与模型、提示词长度无关，是平台级校验）。目录里
// GLM-5.2 写 131072、deepseek-v4 写 393216，照抄进 /v1/models 客户端就会
// 整条请求被拒，所以对外报出的输出上限先夹一遍。
const DefaultMaxOutputTokens = 65536

// capOutputTokens 把目录自述的输出上限夹到上游硬顶；0 表示目录没给，原样保留。
func capOutputTokens(declared int) int {
	if declared <= 0 || declared <= DefaultMaxOutputTokens {
		return declared
	}
	return DefaultMaxOutputTokens
}

// creditTier 是上游 credit[] 的一个计费档（按输入长度分段）。
type creditTier struct {
	InputFrom    *int   `json:"input_from"`
	Status       string `json:"status"`
	RatioDisplay string `json:"ratio_display"`
}

// baseCreditDisplay 取基础档的官方倍率文案。
//
// 上游按输入长度给多条档位（input_from 0 与 32001 各一条，-1 表示无上限），
// 而控制台的倍率列只有一格，取基础档与官网徽章一致。status 显式非 "0" 的档
// 跳过（缺字段当生效）；没有基础档时退回第一条可读档，而不是回空。文案原样
// 保留：ratio 与 ratio_display 不是一个量纲，不能彼此换算。
func baseCreditDisplay(tiers []creditTier) string {
	fallback := ""
	for _, tier := range tiers {
		if status := strings.TrimSpace(tier.Status); status != "" && status != "0" {
			continue
		}
		display := strings.TrimSpace(tier.RatioDisplay)
		if display == "" {
			continue
		}
		if tier.InputFrom == nil || *tier.InputFrom == 0 {
			return display
		}
		if fallback == "" {
			fallback = display
		}
	}
	return fallback
}

// builtinEntry 是 /v1/model/builtin 的 builtinModels[] 条目。
type builtinEntry struct {
	ModelID        string       `json:"model_id"`
	ModelName      string       `json:"model_name"`
	ModelDesc      string       `json:"model_desc"`
	Enable         *bool        `json:"enable"`
	SupportsImages bool         `json:"supports_images"`
	ContextWindow  int          `json:"context_window"`
	MaxTokens      int          `json:"max_tokens"`
	Credit         []creditTier `json:"credit"`
}

// builtinCatalog 兼容两代响应形态。
//
// 实测（2026-09-27）响应是 {"count":6,"builtinModels":[...]}。早期实现只认
// 顶层 models[]，于是每一轮都解析失败、静默回落内置清单——控制台长期显示
// 一份写死的旧目录。models[] 保留为兼容分支，不再作为主判据。
type builtinCatalog struct {
	BuiltinModels []builtinEntry `json:"builtinModels"`
	Models        []builtinEntry `json:"models"`
}

// bundledModels 返回 CodeArts 的兜底模型清单。
//
// 无凭证或目录接口失败时的退路：照 2026-09-27 的内置目录实测快照写，
// 条目、倍率、图片能力与上游一致；上下文与输出上限来自 agent detail 的
// model_parameters（内置目录本身不给这两个数）。
func bundledModels() []Model {
	return []Model{
		{ID: "GLM-5.2", Name: "GLM-5.2", Description: "Latest Flagship: Long-horizon tasks & complex projects", ContextWindow: 202752, MaxTokens: DefaultMaxOutputTokens, SupportsTools: true, Credits: "0.7x"},
		{ID: "openpangu-2.0-pro", Name: "OpenPangu-2.0-Pro", Description: "Latest Flagship: Long-horizon tasks & complex projects", ContextWindow: 524288, MaxTokens: DefaultMaxOutputTokens, SupportsTools: true, Credits: "0.7x"},
		{ID: "openpangu-2.0-flash", Name: "OpenPangu-2.0-Flash", Description: "Balanced: Reasoning & Speed", ContextWindow: 524288, MaxTokens: DefaultMaxOutputTokens, SupportsTools: true, Credits: "0.32x"},
		{ID: "glm-5.2-sft-harmony", Name: "GLM-5.2-ArkTS-SPARK", Description: "Latest Flagship: Long-horizon tasks & complex projects", ContextWindow: 202752, MaxTokens: DefaultMaxOutputTokens, SupportsTools: true, Credits: "0.7x"},
		{ID: "Qwen3-VL-235B", Name: "Qwen3-VL-235B", Description: "A multimodal vision-language model capable of image comprehension", SupportsTools: true, SupportsImages: true},
		{ID: "kimi-k2.6-vl", Name: "Kimi-K2.6-VL", Description: "Outstanding programming performance", SupportsTools: true, SupportsImages: true},
	}
}

// fallbackLimits 返回兜底清单里同 ID 模型的上下文与输出上限。
//
// 内置目录不给这两个数，真实值在 /v1/agent-center/agents/detail 的
// model_parameters 里；本插件不做 agent 探测，因此只对兜底清单里实测过的
// 条目补位，其余保持 0（未知），不编造数字。
func fallbackLimits(id string) (int, int) {
	for _, model := range bundledModels() {
		if strings.EqualFold(model.ID, id) {
			return model.ContextWindow, model.MaxTokens
		}
	}
	return 0, 0
}

// parseBuiltinModels 解析内置目录响应。
func parseBuiltinModels(body []byte) ([]Model, error) {
	var catalog builtinCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode builtin model catalog: %w", err)
	}
	entries := catalog.BuiltinModels
	if len(entries) == 0 {
		entries = catalog.Models
	}
	models := make([]Model, 0, len(entries))
	for _, entry := range entries {
		// enable 显式 false 才跳过（缺字段当可用）。
		if entry.Enable != nil && !*entry.Enable {
			continue
		}
		id := strings.TrimSpace(core.FirstNonEmpty(entry.ModelID, entry.ModelName))
		if id == "" {
			continue
		}
		contextWindow, maxTokens := entry.ContextWindow, entry.MaxTokens
		if contextWindow <= 0 || maxTokens <= 0 {
			hintContext, hintOutput := fallbackLimits(id)
			if contextWindow <= 0 {
				contextWindow = hintContext
			}
			if maxTokens <= 0 {
				maxTokens = hintOutput
			}
		}
		models = append(models, Model{
			ID:             id,
			Name:           core.FirstNonEmpty(entry.ModelName, id),
			Description:    core.FirstNonEmpty(entry.ModelDesc, "CodeArts 内置模型"),
			ContextWindow:  contextWindow,
			MaxTokens:      capOutputTokens(maxTokens),
			SupportsTools:  true,
			SupportsImages: entry.SupportsImages,
			Credits:        baseCreditDisplay(entry.Credit),
		})
	}
	return models, nil
}

// FetchModels 尝试从上游拉取模型列表。
//
// 内置目录（/v1/model/builtin）是主判据：它给出模型名、展示名、图片能力与
// 官方计费倍率。它不带上限类数字，上下文与输出上限只能从兜底清单补位，
// 未知时不编造（保持 0，由 core 层决定兜底）。
func FetchModels(ctx context.Context, cred *Credential) ([]Model, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return bundledModels(), nil
	}

	// 福利通道清单只在有凭证时拉取：它是「哪些模型能走每日免费额度」的唯一判据。
	benefitModels, errBenefit := FetchBenefitModels(ctx, cred)
	if errBenefit != nil {
		logger.Debug("codearts: 拉取福利模型清单失败: %v", errBenefit)
		benefitModels = nil
	}
	fallback := func() ([]Model, error) {
		return mergeBenefitModels(bundledModels(), benefitModels), nil
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, SnapEngineApiHost+EpModelBuiltin, nil)
	if errReq != nil {
		return fallback()
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 15*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return fallback()
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fallback()
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	models, errParse := parseBuiltinModels(body)
	if errParse != nil {
		logger.Debug("codearts: 解析内置模型目录失败: %v", errParse)
		return fallback()
	}
	if len(models) == 0 {
		return fallback()
	}
	return mergeBenefitModels(models, benefitModels), nil
}

// mergeBenefitModels 把福利通道模型并进清单，已存在的 ID 不重复添加。
func mergeBenefitModels(base, benefit []Model) []Model {
	seen := make(map[string]bool, len(base))
	for _, model := range base {
		seen[model.ID] = true
	}
	for _, model := range benefit {
		if seen[model.ID] {
			continue
		}
		base = append(base, model)
	}
	return base
}
