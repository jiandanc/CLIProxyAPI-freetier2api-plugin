package cb

// 本文件实现模型清单的上游探测。
//
// CN 与 global 的探测路径不同，且**都是并发多路 + 并集合并**：
//   - CN：/v3/config（IDE UA）+ 企业模型目录；
//   - global：/v3/config（IDE UA）+ /v3/config（CLI UA）+ 企业模型目录家族。
//
// 两路/三路缺一不可：实测 IDE UA 与 CLI UA 返回的清单**各有独有模型**
// （IDE 有 o4-mini/enhance-1.0，CLI 有 deepseek 系），任何单路都会漏模型。
//
// 合并策略：v3 条目优先（字段更全），企业端点只补 v3 缺失的 id。
// 单路失败降级为另一路；全部失败才返回错误。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// v3ConfigResponse 是 /v3/config 的响应体。
//
// 注意：doJSON 已经解开了一层信封（code / msg / data），
// 因此这里对应的是 data 的内容，不带 code 字段。
type v3ConfigResponse struct {
	Models []dynModelEntry `json:"models"`
	// ProductFeaturesConfig 里放「试用模型横幅」：上游把限时试用的模型
	// 放在这里而**不在 models 里**，但那些模型实际可调用。
	ProductFeaturesConfig struct {
		ModelTrialBanner struct {
			Banners []struct {
				ModelID       string `json:"modelId"`
				TargetModelID string `json:"targetModelId"`
			} `json:"banners"`
		} `json:"ModelTrialBanner"`
	} `json:"productFeaturesConfig"`
}

// enterpriseModelsResponse 是企业模型目录的响应体（同样已被解开一层信封）。
type enterpriseModelsResponse struct {
	// Agents 给出模型的白名单与顺序（cli agent 的 models 列表）。
	Agents []struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	} `json:"agents"`
	// Models 是字段来源，按 id 取。
	Models []dynModelEntry `json:"models"`
}

// modelProbe 是模型探测需要的凭证来源。
//
// 探测必须用一个真实凭证（上游要鉴权），但探测逻辑不该关心凭证从哪来。
// 由 package main 在启动/刷新时注入：通常是队列里的第一个已启用凭证。
var (
	probeCredentialMu sync.RWMutex
	probeCredentialFn func(region Region) *Credential
)

// SetProbeCredentialFunc 注入「按域取一个可用凭证」的回调。
func SetProbeCredentialFunc(fn func(region Region) *Credential) {
	probeCredentialMu.Lock()
	probeCredentialFn = fn
	probeCredentialMu.Unlock()
}

// probeCredential 取一个用于探测的凭证。
//
// 未注入或取不到时返回一个空凭证：请求会带上 X-No-Authorization 标记，
// 上游返回的 401 会被正常分类，而不是让探测静默失败。
func (c *Client) probeCredential() *Credential {
	probeCredentialMu.RLock()
	fn := probeCredentialFn
	probeCredentialMu.RUnlock()
	if fn == nil {
		return &Credential{}
	}
	if cred := fn(RegionCN); cred != nil {
		return cred
	}
	return &Credential{}
}

// fetchModelsFromUpstream 并发探测某个域的模型清单。
func (c *Client) fetchModelsFromUpstream(region Region) ([]ModelInfo, error) {
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}

	type probeResult struct {
		// primary 为 true 表示该路是 v3 目录（字段更全，合并时优先）。
		primary bool
		models  []ModelInfo
		err     error
	}
	var (
		waitGroup sync.WaitGroup
		mu        sync.Mutex
		results   []probeResult
	)

	record := func(primary bool, models []ModelInfo, err error) {
		mu.Lock()
		results = append(results, probeResult{primary: primary, models: models, err: err})
		mu.Unlock()
	}

	// /v3/config（IDE UA）：两域共用，字段最全。
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		models, errProbe := c.probeV3Config(region, codeBuddyIDEUA)
		record(true, models, errProbe)
	}()

	if region.IsGlobal() {
		// /v3/config（CLI UA）：仅 global。独有的 deepseek 系模型只在这条路上。
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			models, errProbe := c.probeV3Config(region, codeBuddyCLIUA)
			record(true, models, errProbe)
		}()
	}

	// 企业模型目录：v3 缺失的 id 由它补齐。
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		models, errProbe := c.probeEnterpriseModels(region)
		record(false, models, errProbe)
	}()

	waitGroup.Wait()

	// 先合并两路 v3（IDE 结果在前，字段权威），再用企业端点补缺失。
	var primaryModels, fallbackModels []ModelInfo
	var firstErr error
	for _, result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		if result.primary {
			primaryModels = append(primaryModels, result.models...)
		} else {
			fallbackModels = append(fallbackModels, result.models...)
		}
	}

	merged := dedupeModels(primaryModels)
	// 企业端点只补 v3 没有的 id（不覆盖已有字段）。
	for _, model := range fallbackModels {
		if !containsModelID(merged, model.ID) {
			merged = append(merged, model)
		}
	}
	if len(merged) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("probe models for %s: %w", region, firstErr)
		}
		return nil, fmt.Errorf("probe models for %s: no models returned", region)
	}

	merged = filterChatModels(merged)
	if len(merged) == 0 {
		return nil, fmt.Errorf("probe models for %s: all entries filtered as non-chat", region)
	}
	merged = applyCatalogFallbacks(merged, region)
	sortModels(merged)
	return merged, nil
}

// probeV3Config 拉取 /v3/config 并解析模型清单。
func (c *Client) probeV3Config(region Region, userAgent string) ([]ModelInfo, error) {
	// 探测需要一个凭证：没有凭证就无法请求上游。
	cred := c.probeCredential()
	if cred == nil {
		return nil, fmt.Errorf("no credential available for model probe")
	}
	endpoints := GetEndpoints(region)
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodGet, endpoints.ChatBase+v3ConfigPath, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build v3 config request: %w", errReq)
	}
	c.applyV3ConfigHeaders(req, cred, userAgent)

	data, errDo := c.doJSONWithLimit(req, configBodyLimit)
	if errDo != nil {
		return nil, errDo
	}

	var payload v3ConfigResponse
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode v3 config: %w", errUnmarshal)
	}
	models := make([]ModelInfo, 0, len(payload.Models))
	for _, entry := range payload.Models {
		if entry.Disabled || nonChatModel(entry) {
			continue
		}
		models = append(models, entry.toModelInfo())
	}
	// 试用模型横幅：实际可调用但不在 data.models 里，元数据从 target 继承，
	// 但计费与营销字段必须清空（它们描述的是「转正后」的状态）。
	for _, banner := range payload.ProductFeaturesConfig.ModelTrialBanner.Banners {
		trialID := strings.TrimSpace(banner.ModelID)
		if trialID == "" || containsModelID(models, trialID) {
			continue
		}
		targetID := strings.TrimSpace(banner.TargetModelID)
		trial := ModelInfo{ID: trialID, Name: trialID}
		if base, okBase := findModel(models, targetID); okBase {
			trial = base
			trial.ID = trialID
			trial.Name = trialID
			trial.Credits = ""
		}
		models = append(models, trial)
	}
	return models, nil
}

// probeEnterpriseModels 拉取企业模型目录。
func (c *Client) probeEnterpriseModels(region Region) ([]ModelInfo, error) {
	cred := c.probeCredential()
	if cred == nil {
		return nil, fmt.Errorf("no credential available for model probe")
	}
	endpoints := GetEndpoints(region)
	paths := []string{enterpriseModelsPathCN}
	if region.IsGlobal() {
		paths = []string{enterpriseModelsPathGlobal, enterpriseModelsPathCN}
	}

	var lastErr error
	for _, path := range paths {
		req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodGet, endpoints.ChatBase+path, nil)
		if errReq != nil {
			lastErr = errReq
			continue
		}
		c.applyV3ConfigHeaders(req, cred, codeBuddyIDEUA)

		data, errDo := c.doJSONWithLimit(req, configBodyLimit)
		if errDo != nil {
			lastErr = errDo
			// 仅在 404 时换下一条路径（其他错误换路径也救不回来）。
			if upstreamErr, okErr := errDo.(*Error); okErr && upstreamErr.Kind == KindNotFound {
				continue
			}
			return nil, errDo
		}

		var payload enterpriseModelsResponse
		if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
			lastErr = fmt.Errorf("decode enterprise models: %w", errUnmarshal)
			continue
		}
		return parseEnterpriseModels(payload), nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("enterprise models endpoint is unavailable")
}

// parseEnterpriseModels 按「agents 白名单顺序 + models 字段来源」解析企业目录。
//
// 白名单顺序是刻意的：上游用它表达模型的推荐排序，丢了顺序等于丢了产品意图。
func parseEnterpriseModels(payload enterpriseModelsResponse) []ModelInfo {
	byID := make(map[string]dynModelEntry, len(payload.Models))
	for _, entry := range payload.Models {
		if id := entry.resolvedID(); id != "" {
			byID[id] = entry
		}
	}

	order := make([]string, 0, len(byID))
	seen := make(map[string]bool, len(byID))
	for _, agent := range payload.Agents {
		if !strings.EqualFold(strings.TrimSpace(agent.Name), "cli") {
			continue
		}
		for _, id := range agent.Models {
			trimmed := strings.TrimSpace(id)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			order = append(order, trimmed)
		}
	}
	// 没有 agent 白名单时退回 models 的原序。
	if len(order) == 0 {
		for _, entry := range payload.Models {
			if id := entry.resolvedID(); id != "" && !seen[id] {
				seen[id] = true
				order = append(order, id)
			}
		}
	}

	out := make([]ModelInfo, 0, len(order))
	for _, id := range order {
		entry, okEntry := byID[id]
		if !okEntry {
			// 白名单里有、字段表里没有：只有 id 可用，窗口交给查找链兜底。
			out = append(out, ModelInfo{ID: id, Name: id})
			continue
		}
		if entry.Disabled || nonChatModel(entry) {
			continue
		}
		out = append(out, entry.toModelInfo())
	}
	return out
}

// dedupeModels 按 ID 去重，保留首次出现的条目。
func dedupeModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, model)
	}
	return out
}

// filterChatModels 剔除对话不可用的模型。
func filterChatModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		// tiny 输出的条目在上游条目层已被过滤，这里再兜一层：
		// 动态兜底解析出的条目没有原始 tags，只能靠输出上限判断。
		if model.MaxTokens > 0 && model.MaxTokens <= 256 {
			continue
		}
		out = append(out, model)
	}
	return out
}

func containsModelID(models []ModelInfo, id string) bool {
	_, okFound := findModel(models, id)
	return okFound
}

func findModel(models []ModelInfo, id string) (ModelInfo, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	return ModelInfo{}, false
}
