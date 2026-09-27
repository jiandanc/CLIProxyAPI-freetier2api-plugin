package cb

// 本文件实现模型目录：从上游探测模型清单、缓存，并提供窗口与档位的查找链。

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// modelCacheEntry 是某个域的模型目录缓存。
type modelCacheEntry struct {
	models    []ModelInfo
	fetchedAt time.Time
	lastFail  time.Time
}

// 缓存窗口。与上游的探测代价匹配：清单变化很慢，但失败后要尽快允许重试。
const (
	modelCacheTTL        = time.Hour
	modelCacheFailWindow = 5 * time.Minute
)

//go:embed catalog_seed.json
var catalogSeedRaw []byte

// ModelInfo 是归一化后的模型元数据。
type ModelInfo struct {
	// ID 是上游模型名（不含插件侧的 realm 前缀）。
	ID string
	// Name 是展示名（上游 display_name，缺失时回落 ID）。
	Name string
	// Description 是模型说明（可能带积分倍率前缀）。
	Description string
	// ContextWindow 是最大输入 token 数。
	ContextWindow int64
	// MaxTokens 是最大输出 token 数（思考与回答共享该预算）。
	MaxTokens int64
	// Efforts 是可选的 reasoning 档位。
	Efforts []string
	// DefaultEffort 是默认档位。
	DefaultEffort string
	// Credits 是积分倍率（形如 "x0.05"）。
	Credits string
	// SupportsImages 表示是否支持图片输入。
	SupportsImages bool
	// SupportsToolCall 表示是否支持工具调用。
	SupportsToolCall bool
	// SupportsReasoning 表示是否支持思维链。
	SupportsReasoning bool
	// CanDisableThinking 表示是否允许关闭思考。
	CanDisableThinking bool
	// OnlyReasoning 表示仅推理（不产出正文）。
	OnlyReasoning bool
	// IsDefault 表示是否为账号默认模型。
	IsDefault bool
}

// catalogSeed 是内嵌的兜底数据。
type catalogSeed struct {
	Context map[string]struct {
		Context   int64 `json:"context"`
		MaxOutput int64 `json:"max_output"`
	} `json:"context"`
	EffortsCN     map[string]effortSeed `json:"efforts_cn"`
	EffortsGlobal map[string]effortSeed `json:"efforts_global"`
}

type effortSeed struct {
	Efforts []string `json:"efforts"`
	Default string   `json:"default"`
}

var (
	seedOnce sync.Once
	seedData catalogSeed
)

func loadSeed() catalogSeed {
	seedOnce.Do(func() {
		if errUnmarshal := json.Unmarshal(catalogSeedRaw, &seedData); errUnmarshal != nil {
			// 内嵌资源损坏是构建期问题，运行期只能降级：没有兜底表仍可工作
			// （窗口与档位字段会缺失，但不影响对话）。
			seedData = catalogSeed{}
		}
	})
	return seedData
}

// dynModelEntry 是上游 /v3/config 与模型目录接口的条目形态。
//
// 字段覆盖 CN 与 global 两种形态：global 会用 modelId/model 作为
// id 的宽松回退键，CN 目录不下发这两个字段。
type dynModelEntry struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	ModelID         string   `json:"modelId"`
	Model           string   `json:"model"`
	Description     string   `json:"descriptionZh"`
	Credits         string   `json:"credits"`
	Tags            []string `json:"tags"`
	Vendor          string   `json:"vendor"`
	IsDefault       bool     `json:"isDefault"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	MaxAllowedSize  int64    `json:"maxAllowedSize"`
	Disabled        bool     `json:"disabled"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsReason  bool     `json:"supportsReasoning"`
	SupportsTool    bool     `json:"supportsToolCall"`
	OnlyReasoning   bool     `json:"onlyReasoning"`
	Reasoning       struct {
		Effort             string   `json:"effort"`
		Summary            string   `json:"summary"`
		DefaultEffort      string   `json:"defaultEffort"`
		CanDisableThinking bool     `json:"canDisableThinking"`
		SupportedEfforts   []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// resolvedID 返回条目的模型 ID（带回退链）。
func (e dynModelEntry) resolvedID() string {
	for _, candidate := range []string{e.ID, e.ModelID, e.Model} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return strings.TrimSpace(e.Name)
}

// toModelInfo 把上游条目映射为归一化元数据。
//
// 这是 dynEntry → ModelInfo 的**单一事实来源**：两个域的映射规则必须一致，
// 否则同一模型在 cn 与 global 下会呈现不同的窗口值。
//
// 窗口与档位这里原样透传上游值（可能为 0 / 空），由 applyCatalogFallbacks
// 按 realm 补齐——补齐需要 realm，而映射不需要，两者分开职责更清楚。
func (e dynModelEntry) toModelInfo() ModelInfo {
	id := e.resolvedID()
	name := strings.TrimSpace(e.Name)
	if name == "" {
		name = id
	}
	return ModelInfo{
		ID:                 id,
		Name:               name,
		Description:        strings.TrimSpace(e.Description),
		ContextWindow:      e.MaxInputTokens,
		MaxTokens:          e.MaxOutputTokens,
		Efforts:            normalizeEfforts(e.Reasoning.SupportedEfforts),
		DefaultEffort:      firstNonEmpty(e.Reasoning.DefaultEffort, e.Reasoning.Effort),
		Credits:            strings.TrimSpace(e.Credits),
		SupportsImages:     e.SupportsImages,
		SupportsToolCall:   e.SupportsTool,
		SupportsReasoning:  e.SupportsReason,
		CanDisableThinking: e.Reasoning.CanDisableThinking,
		OnlyReasoning:      e.OnlyReasoning,
		IsDefault:          e.IsDefault,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// normalizeEfforts 去重并保持顺序。
func normalizeEfforts(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		effort := strings.ToLower(strings.TrimSpace(item))
		if effort == "" || seen[effort] {
			continue
		}
		seen[effort] = true
		out = append(out, effort)
	}
	return out
}

// nonChatModel 判断条目是否为非对话模型（选中会得到 11102）。
//
// 三类：嵌入/补全/代码专用（id 前缀）、tiny 输出（不是对话模型）、
// 图片生成（tags 标记）。
func nonChatModel(entry dynModelEntry) bool {
	id := strings.ToLower(entry.resolvedID())
	for _, prefix := range []string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	if entry.MaxOutputTokens > 0 && entry.MaxOutputTokens <= 256 {
		return true
	}
	for _, tag := range entry.Tags {
		if strings.EqualFold(strings.TrimSpace(tag), "text-to-image") {
			return true
		}
	}
	return false
}

// FetchModels 返回某个域的模型清单。
//
// 失败时返回缓存（哪怕已过期）而不是错误：上游偶发抖动不该让整个模型列表消失。
// 从未成功过则返回错误，由调用方决定是否暴露给用户。
func (c *Client) FetchModels(region Region) ([]ModelInfo, error) {
	modelCacheMu.Lock()
	entry, hasCache := modelCache[region]
	modelCacheMu.Unlock()

	if hasCache && len(entry.models) > 0 {
		if time.Since(entry.fetchedAt) < modelCacheTTL {
			return entry.models, nil
		}
		// 失败冷却期内不重复打上游。
		if time.Since(entry.lastFail) < modelCacheFailWindow {
			return entry.models, nil
		}
	}

	models, errFetch := c.fetchModelsFromUpstream(region)
	if errFetch != nil {
		modelCacheMu.Lock()
		cached := modelCache[region]
		cached.lastFail = time.Now()
		if len(cached.models) == 0 {
			cached.models = entry.models
		}
		modelCache[region] = cached
		modelCacheMu.Unlock()
		if len(cached.models) > 0 {
			return cached.models, nil
		}
		return nil, errFetch
	}

	modelCacheMu.Lock()
	modelCache[region] = modelCacheEntry{models: models, fetchedAt: time.Now()}
	modelCacheMu.Unlock()
	return models, nil
}

// 模型目录缓存是**包级**的，刻意不挂在 Client 上。
//
// 原因：调用方（对话、任务、管理页）都是「一次操作建一个 Client」，
// 把缓存挂在实例上等于每次都从零开始——管理页会永远读到空清单。
// 上游目录本身是全局数据（按域一份），放包级既正确又省重复探测。
var (
	modelCacheMu sync.Mutex
	modelCache   = map[Region]modelCacheEntry{}
)

// CachedModels 只返回已缓存的清单，不触发上游请求（供管理页展示）。
func CachedModels() map[Region][]ModelInfo {
	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()
	out := make(map[Region][]ModelInfo, len(modelCache))
	for region, entry := range modelCache {
		if len(entry.models) > 0 {
			out[region] = entry.models
		}
	}
	return out
}

// LoadCachedModels 从状态文件加载上次缓存的模型清单。
//
// 启动时先灌缓存，可以让 /v1/models 在首次上游探测完成前就有内容，
// 避免客户端启动瞬间看到空列表。
func LoadCachedModels(cached []ModelInfo, realm Region) {
	if len(cached) == 0 {
		return
	}
	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()
	if len(modelCache[realm].models) > 0 {
		return
	}
	modelCache[realm] = modelCacheEntry{models: cached}
}

// applyCatalogFallbacks 用查找链补齐窗口与档位。
//
// 四级查找链（窗口）：上游动态值 → 静态兜底表 (catalog_seed) → 默认值。
// 档位是三级链：上游动态值 → realm 分表静态兜底 → 省略字段。
//
// 两套链刻意不同：窗口有「宁可高估」的安全侧（低估会让客户端过早截断上下文），
// 而档位没有安全侧（发错档位会被上游 400 拒绝），所以档位查不到就省略。
func applyCatalogFallbacks(models []ModelInfo, region Region) []ModelInfo {
	seed := loadSeed()
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		if model.ContextWindow <= 0 {
			if cap, okCap := seed.Context[model.ID]; okCap {
				model.ContextWindow = cap.Context
				if model.MaxTokens <= 0 {
					model.MaxTokens = cap.MaxOutput
				}
			}
		}
		if model.ContextWindow <= 0 {
			model.ContextWindow = defaultContextWindow
		}
		// 档位：上游没给才回落静态分表。上游给了就以它为准，绝不叠加。
		if len(model.Efforts) == 0 {
			if cap, okCap := staticEffortCap(region, model.ID); okCap {
				model.Efforts = cap.Efforts
				if model.DefaultEffort == "" {
					model.DefaultEffort = cap.DefaultEffort
				}
			}
		}
		// defaultEffort 必须属于 efforts，否则客户端会发出非法档位。
		if model.DefaultEffort != "" && !containsString(model.Efforts, model.DefaultEffort) {
			model.DefaultEffort = ""
		}
		out = append(out, model)
	}
	return out
}

// defaultContextWindow 是窗口查找链的最终兜底。
//
// 上游模型普遍是 1M 上下文，低估会让客户端过早截断对话。
const defaultContextWindow int64 = 1000000

// DefaultContextWindowFor 返回某个模型的兜底窗口（静态表优先，否则用全局默认）。
//
// 供插件在注册「额外模型」（不在上游目录里、由运维手写）时使用。
func DefaultContextWindowFor(modelID string) int64 {
	seed := loadSeed()
	if cap, okCap := seed.Context[strings.TrimSpace(modelID)]; okCap && cap.Context > 0 {
		return cap.Context
	}
	return defaultContextWindow
}

// staticEffortCap 按 realm 查静态档位兜底表。
//
// **必须按 realm 分表**：同一个模型名在两侧的合法档位不同
// （deepseek-v4.1-flash 在 CN 是 low/high/max，在 global 只有 high），
// 混用会让 global 请求带上 low/max 而被上游 400 拒绝。
func staticEffortCap(region Region, modelID string) (ModelInfo, bool) {
	seed := loadSeed()
	table := seed.EffortsCN
	if region.IsGlobal() {
		table = seed.EffortsGlobal
	}
	entry, okEntry := table[strings.ToLower(strings.TrimSpace(modelID))]
	if !okEntry {
		return ModelInfo{}, false
	}
	return ModelInfo{Efforts: normalizeEfforts(entry.Efforts), DefaultEffort: entry.Default}, true
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// modelCachePath 返回模型缓存文件的路径。
func modelCachePath(stateDir string) string {
	return filepath.Join(strings.TrimSpace(stateDir), "models.json")
}

// SaveModelCache 把探测结果写入缓存文件（供下次启动预热）。
func SaveModelCache(stateDir string, byRealm map[Region][]ModelInfo) error {
	dir := strings.TrimSpace(stateDir)
	if dir == "" {
		return nil
	}
	serializable := map[string][]ModelInfo{}
	for region, models := range byRealm {
		if len(models) == 0 {
			continue
		}
		serializable[string(region)] = models
	}
	if len(serializable) == 0 {
		return nil
	}
	raw, errMarshal := json.MarshalIndent(serializable, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal model cache: %w", errMarshal)
	}
	path := modelCachePath(dir)
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
		return fmt.Errorf("write model cache: %w", errWrite)
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace model cache: %w", errRename)
	}
	return nil
}

// LoadModelCache 读取模型缓存文件。
func LoadModelCache(stateDir string) map[Region][]ModelInfo {
	path := modelCachePath(stateDir)
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil
	}
	var loaded map[string][]ModelInfo
	if errUnmarshal := json.Unmarshal(raw, &loaded); errUnmarshal != nil {
		return nil
	}
	out := make(map[Region][]ModelInfo, len(loaded))
	for realm, models := range loaded {
		out[NormalizeRegion(realm)] = models
	}
	return out
}

// sortModels 按 ID 排序，保证 /v1/models 输出稳定。
func sortModels(models []ModelInfo) {
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
}

// ResetModelCacheForTest 清空包级模型目录缓存（仅供测试使用）。
func ResetModelCacheForTest() {
	modelCacheMu.Lock()
	modelCache = map[Region]modelCacheEntry{}
	modelCacheMu.Unlock()
}
