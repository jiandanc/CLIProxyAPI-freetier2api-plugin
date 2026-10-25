// 本文件是模型目录（catalog）：静态清单缓存、别名、禁用集合、同名合并与刷新。
//
// 放在 admin 包内是因为它只服务于管理页与模型注册这条链：目录的写入全部
// 来自管理接口或注册回包，读写都要经过宿主 RPC 与 state.json，没有第二个
// 调用方。为此单开一个包只会让 admin 与它互相 import。
package admin

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/state"
)

// 模型目录（Catalog）管理模型的注册视图：禁用集合、别名、额外模型、
// 静态模型缓存与同名合并。
//
// 这些规则与传输层无关；管理接口只做请求解码与响应封装，规则全在这里。

// ModelSettings 是模型层关心的全局配置快照。
type ModelSettings struct {
	EnabledRealms  []string
	ExtraModels    []string
	DisabledModels []string
	// ModelNameTag 是模型名渠道标识模式（off/prefix/suffix）。
	ModelNameTag string
	// ContextWindowFor 返回额外模型的上下文窗口兜底（缺省 0 表示不填）。
	ContextWindowFor func(modelID string) int64
}

// Catalog 持有模型相关的进程级缓存。
type Catalog struct {
	store    *state.Store
	registry *core.Registry

	mu       sync.RWMutex
	settings ModelSettings
	disabled map[string]bool
	aliases  map[string]string
	// cached 是各供应商最近一次拿到的**官方 ID** 清单（控制台回读用）。
	cached map[string]cachedVendor

	// probing 记录正在进行中的按供应商探测（singleflight）。
	// probeMu 保护它；probeAt 是上次探测时间，用于 TTL 内不重复打上游。
	probeMu sync.Mutex
	probing map[string]chan struct{}
	probeAt map[string]time.Time
}

// cachedVendor 是一个供应商的控制台回读缓存。
//
// 倍率（credits）与模型清单同属一次探测的结果，放在同一个条目里而不是
// 另开一张表：两者必须同进同退，分开存迟早出现「模型换了一批、倍率还是
// 上一批」的错配。
type cachedVendor struct {
	models []pluginapi.ModelInfo
	// credits 是官方模型 ID → 额度/费用倍率文案（如 "x0.05"、"免费"）。
	// 宿主契约 pluginapi.ModelInfo 没有这个字段，只能由供应商的
	// ConsoleModels 能力单独提供。
	credits map[string]string
}

// New 创建 Catalog。
func NewCatalog(store *state.Store, registry *core.Registry) *Catalog {
	return &Catalog{
		store:    store,
		registry: registry,
		disabled: map[string]bool{},
		aliases:  map[string]string{},
		cached:   map[string]cachedVendor{},
		probing:  map[string]chan struct{}{},
		probeAt:  map[string]time.Time{},
	}
}

// Configure 更新全局设置并重算禁用集合（配置声明 ∪ 状态记录）。
func (c *Catalog) Configure(settings ModelSettings) {
	snap := settings
	snap.ExtraModels = append([]string(nil), settings.ExtraModels...)
	snap.DisabledModels = append([]string(nil), settings.DisabledModels...)
	snap.EnabledRealms = append([]string(nil), settings.EnabledRealms...)
	c.mu.Lock()
	c.settings = snap
	c.mu.Unlock()
	c.reloadDisabled()
}

// ReloadFromState 用状态快照重算禁用集合与别名表。
func (c *Catalog) ReloadFromState(snapshot state.State) {
	c.mu.Lock()
	c.reloadAliasesLocked(snapshot.ModelAliases)
	c.mu.Unlock()
	c.reloadDisabledWith(snapshot.DisabledModels)
}

// RefreshFromState 直接读取存储快照并重算。
func (c *Catalog) RefreshFromState() {
	if c.store == nil {
		return
	}
	c.ReloadFromState(c.store.Snapshot())
}

func (c *Catalog) reloadDisabled() {
	if c.store == nil {
		c.reloadDisabledWith(nil)
		return
	}
	c.reloadDisabledWith(c.store.Snapshot().DisabledModels)
}

func (c *Catalog) reloadDisabledWith(stateDisabled []string) {
	c.mu.Lock()
	merged := make([]string, 0, len(c.settings.DisabledModels)+len(stateDisabled))
	merged = append(merged, c.settings.DisabledModels...)
	merged = append(merged, stateDisabled...)
	set := make(map[string]bool, len(merged)*2)
	for _, item := range merged {
		for _, key := range c.ExpandKeys(item) {
			if key != "" {
				set[key] = true
			}
		}
	}
	c.disabled = set
	c.mu.Unlock()
}

// RealmEnabled 报告区域是否启用（空区域视为启用）。
func (c *Catalog) RealmEnabled(region string) bool {
	c.mu.RLock()
	enabled := append([]string(nil), c.settings.EnabledRealms...)
	c.mu.RUnlock()
	return core.RealmEnabled(enabled, region)
}

// ExpandKeys 把模型标识展开成查询键。
//
//   - 裸名：原样返回（对所有供应商生效）；
//   - 供应商 ID 前缀：规范 vendorKey；
//   - 区域别名：展开成该区域每家供应商的 vendorKey。
func (c *Catalog) ExpandKeys(modelID string) []string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return nil
	}
	prefix, bare := c.registry.SplitModelID(trimmed)
	if prefix == "" {
		return []string{bare}
	}
	if _, okVendor := c.registry.ByID(prefix); okVendor {
		return []string{c.registry.ModelKeyFor(prefix, bare)}
	}
	keys := make([]string, 0, 4)
	for _, vendor := range c.registry.Vendors() {
		if vendor.Region() == prefix {
			keys = append(keys, c.registry.ModelKeyFor(vendor.ID(), bare))
		}
	}
	if len(keys) > 0 {
		return keys
	}
	return []string{bare}
}

// ExpandDisabledList 展开配置里的禁用项并稳定排序。
func (c *Catalog) ExpandDisabledList(items []string) []string {
	set := make(map[string]bool, len(items)*2)
	for _, item := range items {
		for _, key := range c.ExpandKeys(item) {
			if key != "" {
				set[key] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// IsDisabled 判断某供应商下的模型是否被禁用。
//
// **只对当前清单里存在的模型生效**：禁用键由某个官方 ID 派生（`vendor:id`），
// 供应商换掉对外 ID 后旧键就指向一个不存在的模型。若不加这一步，旧键会把
// 同名的新模型误判成已禁用——真实事故：Qoder 从上游 key 改为规范化 slug 后，
// 遗留的 `qodercn:dfmodel` 让新的 `deepseek-flash` 报「model is disabled」。
//
// 刻意**不删除**状态里的旧键：清单可能因为探测失败而暂时拿不到，删除会让
// 用户配置永久丢失。保留它们只让判定失效，清单恢复后自动重新生效。
func (c *Catalog) IsDisabled(vendorID, modelID string) bool {
	bare := strings.TrimSpace(modelID)
	if bare == "" {
		return false
	}
	if prefix, rest := c.registry.SplitModelID(bare); prefix != "" {
		if vendor, okVendor := c.vendorForPrefix(prefix); okVendor && vendor.ID() != vendorID {
			return false
		}
		bare = rest
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.disabled[bare] && !c.disabled[c.registry.ModelKeyFor(vendorID, bare)] {
		return false
	}
	return c.registeredLocked(vendorID, bare)
}

// registeredLocked 报告某供应商的当前清单里是否还有这个官方 ID。
//
// 清单为空表示还没拉到（冷启动或探测失败），此时无从判定「模型已消失」，
// 按「存在」处理——一次网络抖动不能改变鉴权语义。调用方须持有读锁。
func (c *Catalog) registeredLocked(vendorID, modelID string) bool {
	models := c.cached[vendorID].models
	if len(models) == 0 {
		return true
	}
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model.ID), modelID) {
			return true
		}
	}
	return false
}

// vendorForPrefix 把限定前缀（供应商 ID 或区域别名）解析成供应商实例。
func (c *Catalog) vendorForPrefix(prefix string) (core.Vendor, bool) {
	if vendor, okVendor := c.registry.ByID(prefix); okVendor {
		return vendor, true
	}
	for _, vendor := range c.registry.Vendors() {
		if vendor.Region() == prefix {
			return vendor, true
		}
	}
	return nil, false
}

// FilterForVendor 剔除某供应商下被禁用的模型。
func (c *Catalog) FilterForVendor(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if c.IsDisabled(vendorID, model.ID) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// SetDisabled 批量设置禁用状态，落盘并返回归一后的清单。
func (c *Catalog) SetDisabled(targets []string, disabled bool) []string {
	normalized := make([]string, 0, len(targets))
	for _, target := range targets {
		normalized = append(normalized, c.ExpandKeys(target)...)
	}
	if c.store == nil {
		return normalized
	}
	var next []string
	c.store.Mutate(func(s *state.State) {
		current := make(map[string]bool, len(s.DisabledModels))
		for _, id := range s.DisabledModels {
			current[strings.TrimSpace(id)] = true
		}
		for _, key := range normalized {
			if key == "" {
				continue
			}
			if disabled {
				current[key] = true
			} else {
				delete(current, key)
			}
		}
		next = make([]string, 0, len(current))
		for id := range current {
			next = append(next, id)
		}
		sort.Strings(next)
		s.DisabledModels = next
		s.RestartPending = true
	})
	c.reloadDisabledWith(next)
	return next
}

// RestartPending 报告是否存在待宿主重载的模型变更。
func (c *Catalog) RestartPending() bool {
	if c.store == nil {
		return false
	}
	return c.store.Snapshot().RestartPending
}

// ClearRestartPending 在宿主重建模型注册表后清除待生效标记。
func (c *Catalog) ClearRestartPending() {
	if c.store == nil {
		return
	}
	if !c.store.Snapshot().RestartPending {
		return
	}
	c.store.Mutate(func(s *state.State) { s.RestartPending = false })
}

// CacheStatic 记录某供应商的官方模型清单（控制台回读用）。
//
// 允许写空清单：调用方用空值表达「显式清空」，`registeredLocked` 也依赖
// 「空清单 = 未知」的保守语义。**探测回包不要用它**，改用 CacheProbedModels。
//
// 倍率是探测产物，随清单一起清空：新的清单还没配到倍率时，留着上一批的
// 倍率只会张冠李戴。
func (c *Catalog) CacheStatic(vendorID string, models []pluginapi.ModelInfo) {
	c.mu.Lock()
	c.cached[vendorID] = cachedVendor{models: append([]pluginapi.ModelInfo(nil), models...)}
	c.mu.Unlock()
}

// CacheProbedModels 记录一次探测得到的官方清单（含倍率）并落盘。
//
// 探测失败（上游抖动、凭证暂不可用、免费层改名导致过滤后为空）与「模型真的
// 全部下线」在回包上无法区分，而前者远比后者常见：一次瞬时 401 让整家供应商
// 从控制台消失，是实际发生过的事故。因此空结果只记日志、不动缓存。
//
// 落盘后插件重载（宿主重启/重装）可由 LoadCachedState 回灌，控制台不必再等
// 一轮上游探测。批量探测（StaticModels / RefreshFromUpstream）改用 cacheProbed，
// 在整轮结束时统一落盘一次。
//
// 返回是否写入了缓存。
func (c *Catalog) CacheProbedModels(vendorID string, models []pluginapi.ModelInfo) bool {
	if !c.cacheProbed(vendorID, models) {
		return false
	}
	c.persistProbed()
	return true
}

// cacheProbed 只更新内存缓存，由调用方决定何时落盘。
//
// 倍率与清单在这里一起写：两者都是同一次探测的产物，共用一个条目就不会
// 出现「清单是新的、倍率还是上一批」的错配。
func (c *Catalog) cacheProbed(vendorID string, models []pluginapi.ModelInfo) bool {
	if len(models) == 0 {
		logger.Info("models: %s probe returned 0 model(s); keeping %d cached",
			vendorID, len(c.Registered(vendorID)))
		return false
	}
	// 先取倍率再进临界区：ModelCredits 由供应商实现，持 Catalog 锁调用它
	// 等于把两把锁嵌套在一起，属于可避免的死锁风险。
	credits := c.creditsFromProvider(vendorID)
	c.mu.Lock()
	c.cached[vendorID] = cachedVendor{
		models:  append([]pluginapi.ModelInfo(nil), models...),
		credits: credits,
	}
	c.mu.Unlock()
	return true
}

// creditsFromProvider 读取供应商自报的倍率表（官方模型 ID → 文案）。
//
// 只有实现了 CreditsProvider 的供应商才有倍率；宿主契约里没有这个字段，
// 因此不能从 models 里推。空值不写入，避免把「没提供」当成「免费」。
// 倍率的归一化（FormatRate）由各供应商在自己那层完成，这里只做去空格，
// 不再二次格式化——否则「0」这类合法值会被反复改写。
// 调用方必须在 Catalog 写锁之外调用。
func (c *Catalog) creditsFromProvider(vendorID string) map[string]string {
	vendor, okVendor := c.registry.ByID(vendorID)
	if !okVendor {
		return nil
	}
	provider, okProvider := core.As[core.CreditsProvider](vendor)
	if !okProvider {
		return nil
	}
	credits := make(map[string]string)
	for id, value := range provider.ModelCredits() {
		id = strings.TrimSpace(id)
		value = strings.TrimSpace(value)
		if id != "" && value != "" {
			credits[id] = value
		}
	}
	if len(credits) == 0 {
		return nil
	}
	return credits
}

// persistProbed 把当前清单写入 state.json（内容未变则不写）。
//
// 宿主加载 N 个凭证会调 N 次 model.for_auth，无脑落盘会把一次启动放大成十几次
// state.json 写入。比较在锁外读完快照后完成，只在确实变化时才进 Mutate。
func (c *Catalog) persistProbed() {
	if c.store == nil {
		return
	}
	flat := c.CachedState()
	if len(flat) == 0 || equalCachedModels(c.store.Snapshot().Models, flat) {
		return
	}
	now := time.Now().Format(time.RFC3339)
	c.store.Mutate(func(s *state.State) {
		s.Models = flat
		s.ModelsFetchedAt = now
	})
}

// equalCachedModels 比较两份清单是否等价（忽略顺序）。
func equalCachedModels(a, b []core.CachedModel) bool {
	if len(a) != len(b) {
		return false
	}
	type key struct{ vendor, id string }
	index := make(map[key]core.CachedModel, len(a))
	for _, item := range a {
		index[key{item.VendorID, item.ID}] = item
	}
	for _, item := range b {
		prev, ok := index[key{item.VendorID, item.ID}]
		if !ok || prev.Name != item.Name || prev.ContextLength != item.ContextLength ||
			prev.MaxOutputTokens != item.MaxOutputTokens || prev.Credits != item.Credits {
			return false
		}
	}
	return true
}

// Registered 返回某供应商最近一次的官方模型清单。
func (c *Catalog) Registered(vendorID string) []pluginapi.ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]pluginapi.ModelInfo(nil), c.cached[vendorID].models...)
}

const (
	// modelProbeTTL 是补探测失败后的冷却时间：这段时间内不再重复打上游，
	// 避免页面反复刷新把一次失败放大成上游压力。成功不设冷却。
	modelProbeTTL = 30 * time.Second
	// modelProbeWait 是列表请求等待补探测的上限：超时先渲染已有缓存，
	// 探测在后台继续跑完，下一次刷新即可看到。
	modelProbeWait = 3 * time.Second
	// modelProbeTimeout 是补探测自身的上游调用上限。
	modelProbeTimeout = 30 * time.Second
)

// EnsureModels 在缓存为空时为某供应商触发一次补探测（不等待完成）。
//
// 为什么需要这一步：控制台清单只读这里的内存缓存，而它只在探测类路径写入。
// 宿主加载凭证走的是 model.for_auth，插件重启后若 static 探测还没跑完，页面
// 就会整家供应商缺模型——表现为「有时有、有时没有」。这里在缓存为空时主动补
// 一次，并用 singleflight + 冷却保证一次页面加载最多发起一次上游请求。
//
// 只启动不等待：调用方在收集完所有供应商后再统一 WaitProbes，避免逐个
// 串行等待把页面拖慢成 N 倍的上游延迟。
func (c *Catalog) EnsureModels(vendor core.Vendor) {
	vendorID := vendor.ID()
	if len(c.Registered(vendorID)) > 0 {
		return
	}

	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	if c.probing[vendorID] != nil {
		return
	}
	if last, ok := c.probeAt[vendorID]; ok && time.Since(last) < modelProbeTTL {
		return
	}
	pending := make(chan struct{})
	c.probing[vendorID] = pending
	go c.probeVendor(vendor, pending)
}

// WaitProbes 等待所有进行中的补探测，最多等 timeout。
//
// 超时后探测仍在后台跑完并写缓存，下一次刷新即可看到。
func (c *Catalog) WaitProbes(timeout time.Duration) {
	c.probeMu.Lock()
	pending := make([]chan struct{}, 0, len(c.probing))
	for _, done := range c.probing {
		pending = append(pending, done)
	}
	c.probeMu.Unlock()
	if len(pending) == 0 {
		return
	}
	deadline := time.After(timeout)
	for _, done := range pending {
		select {
		case <-done:
		case <-deadline:
			return
		}
	}
}

// ProbesRunning 返回当前进行中的补探测数量。
func (c *Catalog) ProbesRunning() int {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	return len(c.probing)
}

// probeVendor 在后台拉一次供应商清单并写入控制台缓存。
//
// 用后台 context 而不是管理请求的 context：页面刷新会取消管理 RPC，
// 探测不该因此半途而废（同一类问题见额度查询）。
//
// 冷却只对失败打点：成功后再来一次补探测应当照常进行，否则一次成功会
// 把接下来 30 秒的补探测全部挡掉。
func (c *Catalog) probeVendor(vendor core.Vendor, done chan struct{}) {
	vendorID := vendor.ID()
	ctx, cancel := context.WithTimeout(context.Background(), modelProbeTimeout)
	models, errModels := vendor.StaticModels(ctx)
	cancel()

	wrote := false
	if errModels != nil {
		logger.Info("models: %s console probe failed: %v", vendorID, errModels)
	} else {
		wrote = c.CacheProbedModels(vendorID, models)
		if wrote {
			logger.Info("models: %s console probe cached %d model(s)", vendorID, len(models))
		}
	}

	c.probeMu.Lock()
	if c.probing[vendorID] == done {
		delete(c.probing, vendorID)
	}
	if wrote {
		delete(c.probeAt, vendorID)
	} else {
		c.probeAt[vendorID] = time.Now()
	}
	c.probeMu.Unlock()
	close(done)
}

// LoadCachedState 从 state.json 恢复全部供应商的模型缓存，并通知供应商预热。
func (c *Catalog) LoadCachedState(cached []core.CachedModel) {
	entries := map[string]cachedVendor{}
	for _, item := range cached {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		vendorID := cachedVendorID(item)
		entry := entries[vendorID]
		entry.models = append(entry.models, pluginapi.ModelInfo{
			ID: item.ID, Name: item.Name, DisplayName: item.Name,
			Description: item.Description, ContextLength: item.ContextLength,
			InputTokenLimit: item.ContextLength, MaxCompletionTokens: item.MaxOutputTokens,
			OutputTokenLimit: item.MaxOutputTokens, Thinking: thinkingFromCached(item),
		})
		if rate := core.FormatRate(item.Credits); rate != "" {
			if entry.credits == nil {
				entry.credits = map[string]string{}
			}
			entry.credits[item.ID] = rate
		}
		entries[vendorID] = entry
	}
	c.mu.Lock()
	for vendorID, entry := range entries {
		c.cached[vendorID] = entry
	}
	c.mu.Unlock()

	for _, vendor := range c.registry.Vendors() {
		loader, okLoader := core.As[core.ModelCacheLoader](vendor)
		if !okLoader {
			continue
		}
		loader.LoadModelCache(cachedForVendor(cached, vendor.ID()))
	}
}

// CachedState 把当前各供应商的模型缓存拍平成 state.json 结构。
func (c *Catalog) CachedState() []core.CachedModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]core.CachedModel, 0, 64)
	for vendorID, entry := range c.cached {
		for _, model := range entry.models {
			cachedModel := core.CachedModel{
				VendorID: vendorID, ID: model.ID, Name: model.Name,
				Description: model.Description, ContextLength: model.ContextLength,
				MaxOutputTokens: model.MaxCompletionTokens,
				Credits:         entry.credits[model.ID],
			}
			if vendor, okVendor := c.registry.ByID(vendorID); okVendor {
				cachedModel.Realm = vendor.Region()
			}
			if model.Thinking != nil {
				cachedModel.Efforts = append([]string(nil), model.Thinking.Levels...)
				cachedModel.DefaultEffort = model.Thinking.Levels[0]
			}
			cachedModel.SupportsImages = core.ModelSupportsImages(model)
			cachedModel.SupportsTool = core.ModelSupportsTools(model)
			out = append(out, cachedModel)
		}
	}
	return out
}

// CreditsFor 返回某供应商某官方模型的倍率文案；未知时返回空串。
func (c *Catalog) CreditsFor(vendorID, modelID string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cached[vendorID].credits[strings.TrimSpace(modelID)]
}

// cachedVendorID 解析缓存条目的归属供应商。
//
// 旧状态没有 vendor_id：当时只有 WorkBuddy 写模型缓存，按 realm 回填。
func cachedVendorID(item core.CachedModel) string {
	if vendorID := strings.TrimSpace(item.VendorID); vendorID != "" {
		return vendorID
	}
	if item.Realm == "global" {
		return "workbuddyglobal"
	}
	return "workbuddycn"
}

func cachedForVendor(cached []core.CachedModel, vendorID string) []core.CachedModel {
	out := make([]core.CachedModel, 0, len(cached))
	for _, item := range cached {
		id := item.VendorID
		if id == "" {
			switch item.Realm {
			case "global":
				id = "workbuddyglobal"
			default:
				id = "workbuddycn"
			}
		}
		if id == vendorID {
			out = append(out, item)
		}
	}
	return out
}

func thinkingFromCached(item core.CachedModel) *pluginapi.ThinkingSupport {
	if len(item.Efforts) == 0 && item.DefaultEffort == "" {
		return nil
	}
	return &pluginapi.ThinkingSupport{Levels: append([]string(nil), item.Efforts...), DynamicAllowed: item.DefaultEffort != ""}
}

// aliasScopedKey 是别名表的键：<vendorID>:<官方模型 ID>。
func aliasScopedKey(vendorID, modelID string) string {
	return strings.ToLower(strings.TrimSpace(vendorID)) + ":" + strings.TrimSpace(modelID)
}

func (c *Catalog) reloadAliasesLocked(aliases map[string]string) {
	next := make(map[string]string, len(aliases))
	for key, alias := range aliases {
		trimmedKey := strings.TrimSpace(key)
		trimmedAlias := strings.TrimSpace(alias)
		if trimmedKey == "" || trimmedAlias == "" {
			continue
		}
		next[trimmedKey] = trimmedAlias
	}
	c.aliases = next
}

// AliasFor 返回某供应商某模型设置的别名；未设置时返回空串。
func (c *Catalog) AliasFor(vendorID, modelID string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.aliases[aliasScopedKey(vendorID, modelID)]
}

// ApplyAliases 给某供应商的模型清单换上别名（对外 ID）。
func (c *Catalog) ApplyAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(models) == 0 || (c.settings.ModelNameTag == ModelNameTagOff && len(c.aliases) == 0) {
		return models
	}
	prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
	// 撞车判定用**本次入参清单**构造的 ID 集合：registeredLocked 对缓存为空
	// 的供应商会把任何名字判成「已存在」，导致标识被整体放弃——同一份清单
	// 里有的带标识、有的不带，恰好复活本特性要消除的歧义。
	present := make(map[string]bool, len(models))
	for _, model := range models {
		present[strings.ToLower(strings.TrimSpace(model.ID))] = true
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		bare := strings.TrimSpace(model.ID)
		manual := c.aliases[prefix+bare]
		tagged := c.taggedModelID(vendorID, bare)
		switch {
		case manual != "":
			model.ID = manual
			model.Name = manual
			model.DisplayName = manual
		case tagged != "" && !present[strings.ToLower(tagged)]:
			// 自动渠道标识不覆盖：手动别名优先；tag 后的名字撞上**同供应商
			// 清单里的其他模型**时放弃标识（防路由歧义；跨供应商本就不合并）。
			model.ID = tagged
			model.Name = tagged
			model.DisplayName = tagged
		}
		out = append(out, model)
	}
	return out
}

// taggedModelID 按当前模式给模型 ID 加渠道标识；off 或结果为空返回空串。
//
// 标识统一按**供应商**生成（不同账号之间不区分）：zcode 的 GLM-5.3 无论几个
// 账号对外都是 zcode-GLM-5.3。
func (c *Catalog) taggedModelID(vendorID, modelID string) string {
	switch c.settings.ModelNameTag {
	case ModelNameTagPrefix:
		return vendorID + "-" + modelID
	case ModelNameTagSuffix:
		return modelID + "-" + vendorID
	}
	return ""
}

// ResolveOutboundModel 把客户端请求的模型名还原成上游的官方 ID。
//
// 只有**指向当前注册模型**的别名才生效。供应商换掉对外 ID 后（Qoder 从上游
// key 改为规范化 slug），旧别名会指向一个不存在的 ID；照样还原会让后续的
// 禁用判定与出站都落在那个陈旧 ID 上，表现为「列表里启用、调用却报已禁用」。
//
// 与 IsDisabled 同理，这里不删除旧别名：清单可能因为探测失败而暂时拿不到，
// 删掉会让用户配置永久丢失。忽略它即可，清单恢复后自动生效。
func (c *Catalog) ResolveOutboundModel(vendorID, model string) string {
	requested := strings.TrimSpace(model)
	if requested == "" {
		return model
	}
	prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
	c.mu.RLock()
	defer c.mu.RUnlock()
	for key, alias := range c.aliases {
		if !strings.EqualFold(alias, requested) || !strings.HasPrefix(key, prefix) {
			continue
		}
		bare := strings.TrimPrefix(key, prefix)
		if c.registeredLocked(vendorID, bare) {
			return bare
		}
		logger.Debug("models: ignoring alias %q -> %q (%s no longer registers it)", alias, bare, vendorID)
	}
	// 自动渠道标识的还原：prefix / suffix **两种形态都试**（不依赖当前模式）。
	// 模式切换后（如 prefix→off/suffix）宿主注册表仍是旧名字，客户端手里的
	// 旧标识请求必须仍能还原，否则带标识的名字会直通上游报 invalid model。
	for _, bare := range c.untagModelID(vendorID, requested) {
		if c.registeredLocked(vendorID, bare) {
			return bare
		}
	}
	return model
}

// untagModelID 尝试把带渠道标识的模型 ID 还原成官方 ID，返回全部候选
// （prefix 剥离与 suffix 剥离各一个；都不匹配返回空）。
func (c *Catalog) untagModelID(vendorID, requested string) []string {
	vendorLower := strings.ToLower(strings.TrimSpace(vendorID))
	requestedLower := strings.ToLower(requested)
	var out []string
	if lead := vendorLower + "-"; strings.HasPrefix(requestedLower, lead) && len(requested) > len(lead) {
		out = append(out, requested[len(lead):])
	}
	if tail := "-" + vendorLower; strings.HasSuffix(requestedLower, tail) && len(requested) > len(tail) {
		out = append(out, requested[:len(requested)-len(tail)])
	}
	return out
}

// SetAlias 设置或清除某模型的别名。
//
// 同一供应商内别名唯一：写入时清掉占用同名别名的旧模型，否则反查会命中两条。
func (c *Catalog) SetAlias(vendorID, bare, alias string) {
	if c.store == nil {
		return
	}
	scoped := aliasScopedKey(vendorID, bare)
	alias = strings.TrimSpace(alias)
	var next map[string]string
	c.store.Mutate(func(s *state.State) {
		if s.ModelAliases == nil {
			s.ModelAliases = map[string]string{}
		}
		s.RestartPending = true
		if alias == "" {
			delete(s.ModelAliases, scoped)
		} else {
			prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
			for existing, existingAlias := range s.ModelAliases {
				if existing != scoped && strings.EqualFold(existingAlias, alias) && strings.HasPrefix(existing, prefix) {
					delete(s.ModelAliases, existing)
				}
			}
			s.ModelAliases[scoped] = alias
		}
		next = make(map[string]string, len(s.ModelAliases))
		for key, value := range s.ModelAliases {
			next[key] = value
		}
	})
	c.mu.Lock()
	c.reloadAliasesLocked(next)
	c.mu.Unlock()
}

// VendorForAliasScope 解析 <vendor>:<model> 形式，供管理接口校验。
func (c *Catalog) VendorForAliasScope(scope string) (string, string) {
	return c.registry.SplitModelID(strings.TrimSpace(scope))
}

// StaticModels 返回所有启用且有凭证的供应商的模型并集（同名合并）。
//
// 诊断约定：某供应商没出现在结果里时，这里必须留下可读的原因（域未启用 /
// 没有可用凭证 / 探测失败）。否则用户只看到「模型列表是空的」，无从下手。
func (c *Catalog) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	var all []pluginapi.ModelInfo
	var firstErr error
	active := hostbridge.ActiveVendorSet(ctx, "", c.registry)
	for _, vendor := range c.registry.Vendors() {
		if !c.RealmEnabled(vendor.Region()) {
			logger.Debug("models: skip %s (realm %q not enabled)", vendor.ID(), vendor.Region())
			continue
		}
		if !active[vendor.ID()] {
			logger.Debug("models: skip %s (no usable credential)", vendor.ID())
			continue
		}
		models, errModels := vendor.StaticModels(ctx)
		if errModels != nil {
			logger.Info("models: %s probe failed: %v", vendor.ID(), errModels)
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		logger.Info("models: %s registered %d model(s)", vendor.ID(), len(models))
		// 缓存官方 ID 清单供管理页回读（注册路径随后会换成别名）。
		c.cacheProbed(vendor.ID(), models)
		all = append(all, c.ApplyAliases(vendor.ID(), c.FilterForVendor(vendor.ID(), models))...)
	}
	// 整轮探测结束统一落盘：逐个供应商写会把一次注册放大成 N 次磁盘写入。
	c.persistProbed()
	if len(all) == 0 && firstErr != nil {
		return nil, firstErr
	}
	for _, extra := range c.ExtraModels() {
		all = append(all, c.ApplyAliases(extra.OwnedBy, []pluginapi.ModelInfo{extra})...)
	}
	return MergeSameNameModels(all), nil
}

// RefreshFromUpstream 主动刷新所有启用且有凭证的供应商模型清单，返回条目数。
func (c *Catalog) RefreshFromUpstream(ctx context.Context) (int, error) {
	total := 0
	var firstErr error
	active := hostbridge.ActiveVendorSet(ctx, "", c.registry)
	for _, vendor := range c.registry.Vendors() {
		if !c.RealmEnabled(vendor.Region()) || !active[vendor.ID()] {
			logger.Debug("models: refresh skip %s (realm enabled=%t, credential=%t)",
				vendor.ID(), c.RealmEnabled(vendor.Region()), active[vendor.ID()])
			continue
		}
		models, errModels := vendor.StaticModels(ctx)
		if errModels != nil {
			logger.Info("models: refresh %s failed: %v", vendor.ID(), errModels)
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		logger.Info("models: refresh %s got %d model(s)", vendor.ID(), len(models))
		c.cacheProbed(vendor.ID(), models)
		total += len(models)
	}
	c.persistProbed()
	if total == 0 && firstErr != nil {
		return 0, firstErr
	}
	return total, nil
}

// ExtraModels 把配置里的额外模型名转成模型条目。
func (c *Catalog) ExtraModels() []pluginapi.ModelInfo {
	c.mu.RLock()
	settings := c.settings
	c.mu.RUnlock()
	out := make([]pluginapi.ModelInfo, 0, len(settings.ExtraModels))
	for _, name := range settings.ExtraModels {
		bare := strings.TrimSpace(name)
		if bare == "" {
			continue
		}
		if vendorID, bareModel := c.registry.SplitModelID(bare); vendorID != "" {
			if vendor, okVendor := c.registry.ByID(vendorID); okVendor &&
				c.RealmEnabled(vendor.Region()) && !c.IsDisabled(vendor.ID(), bareModel) {
				out = append(out, c.extraModelInfo(vendor.ID(), bareModel, settings))
			}
			continue
		}
		for _, vendor := range c.registry.Vendors() {
			if !c.RealmEnabled(vendor.Region()) || c.IsDisabled(vendor.ID(), bare) {
				continue
			}
			out = append(out, c.extraModelInfo(vendor.ID(), bare, settings))
		}
	}
	return out
}

func (c *Catalog) extraModelInfo(vendorID, bareModel string, settings ModelSettings) pluginapi.ModelInfo {
	var contextLength int64
	if settings.ContextWindowFor != nil {
		contextLength = settings.ContextWindowFor(bareModel)
	}
	return core.ModelInfoToPluginAPI(vendorID, core.ModelDescriptor{
		ID:            bareModel,
		Name:          bareModel,
		ContextLength: contextLength,
	})
}

// MergeSameNameModels 按裸模型名合并同名条目，参数取并集最优。
//
// 宿主按 EqualsFold 匹配模型 ID，同名条目在它眼里就是同一个模型；
// 不合并会让 /v1/models 列出重复项。窗口/输出取大，能力取或，档位取并集。
func MergeSameNameModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	merged := make([]pluginapi.ModelInfo, 0, len(models))
	seen := make(map[string]int, len(models))
	for _, model := range models {
		bare := strings.TrimSpace(model.ID)
		if bare == "" {
			continue
		}
		key := strings.ToLower(bare)
		index, okSeen := seen[key]
		if !okSeen {
			seen[key] = len(merged)
			merged = append(merged, model)
			continue
		}
		target := &merged[index]
		if model.ContextLength > target.ContextLength {
			target.ContextLength = model.ContextLength
			target.InputTokenLimit = model.InputTokenLimit
		}
		if model.MaxCompletionTokens > target.MaxCompletionTokens {
			target.MaxCompletionTokens = model.MaxCompletionTokens
			target.OutputTokenLimit = model.OutputTokenLimit
		}
		if target.Description == "" {
			target.Description = model.Description
		}
		if core.ModelSupportsImages(model) {
			target.SupportedInputModalities = appendUnique(target.SupportedInputModalities, "image")
		}
		if core.ModelSupportsTools(model) {
			target.SupportedGenerationMethods = appendUnique(target.SupportedGenerationMethods, "tool_calls")
		}
		target.Thinking = mergeThinking(target.Thinking, model.Thinking)
	}
	return merged
}

func mergeThinking(a, b *pluginapi.ThinkingSupport) *pluginapi.ThinkingSupport {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	levels := append([]string(nil), a.Levels...)
	for _, level := range b.Levels {
		levels = appendUnique(levels, level)
	}
	return &pluginapi.ThinkingSupport{
		Levels:         levels,
		DynamicAllowed: a.DynamicAllowed || b.DynamicAllowed,
		ZeroAllowed:    a.ZeroAllowed || b.ZeroAllowed,
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
