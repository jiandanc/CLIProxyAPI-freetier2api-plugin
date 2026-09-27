package main

// 本文件实现 model provider 能力：静态模型清单与按凭证的模型清单。
//
// **供应商隔离就落在这里**。每个供应商实例按自己的区域探测模型，宿主把这份
// 清单注册成「该凭证支持的模型」，选号时用它过滤候选——于是用错域的凭证
// 在选号阶段就被淘汰，插件不需要自己实现任何调度逻辑。
//
// 模型 ID 用**裸名**（不带供应商前缀）：跨供应商的同名模型（如 qoder 的
// GLM-5.3 与 workbuddy 的 glm-5.3）由宿主按 strings.EqualFold 合并为一个条目。

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// staticModelRPCRequest 与宿主的 model.static 请求对齐。
type staticModelRPCRequest struct {
	pluginapi.StaticModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// authModelRPCRequest 与宿主的 model.for_auth 请求对齐。
type authModelRPCRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleModelStatic 返回静态模型清单（供 /v1/models 展示与客户端下拉）。
//
// 静态清单是「全量目录」：所有启用供应商的所有已知模型，同名合并去重。
// 它不参与凭证路由（那是 handleModelForAuth 的职责），只用于展示。
func handleModelStatic(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := httpx.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := staticModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: models})
}

// handleModelRegister 处理宿主的 model.register 契约（ModelRegistrar）。
func handleModelRegister(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := httpx.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := staticModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return okEnvelope(pluginapi.ModelRegistrationResponse{Provider: providerKey, Models: models})
}

// handleModelForAuth 返回某个凭证可用的模型清单。
//
// 这是供应商隔离的执行点：只返回与凭证同供应商的模型，宿主据此把
// 「凭证 ↔ 模型」绑定起来，从而在选号阶段淘汰不匹配的凭证。
func handleModelForAuth(request []byte) ([]byte, error) {
	var rpc authModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := httpx.WithCallbackID(context.Background(), rpc.HostCallbackID)

	credential, vendor, errResolve := resolveVendorCredential(ctx, rpc.HostCallbackID,
		rpc.StorageJSON, rpc.AuthID, rpc.Attributes)
	if errResolve != nil {
		// 取不到凭证时返回空清单而不是报错：宿主会因为空清单而不再路由到
		// 这个凭证，这正是我们想要的结果（避免用坏凭证打上游）。
		logger.Debug("model.for_auth: no credential for %s: %v", rpc.AuthID, errResolve)
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	if !realmEnabled(loadedConfig(), vendor.Region()) {
		// 供应商所在的区域被配置关闭：同样返回空清单，让宿主自然跳过该账号。
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}

	models, errModels := vendor.ModelsForAuth(ctx, credential)
	if errModels != nil {
		logger.Debug("model.for_auth: probe models for %s failed: %v", vendor.ID(), errModels)
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: providerKey,
		Models:   filterDisabledModelsForVendor(vendor.ID(), models),
	})
}

// staticModels 返回所有启用供应商的模型并集（裸模型名，同名合并去重）。
func staticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	var all []pluginapi.ModelInfo
	var firstErr error
	for _, vendor := range core.Vendors() {
		if !realmEnabled(loadedConfig(), vendor.Region()) {
			continue
		}
		models, errModels := vendor.StaticModels(ctx)
		if errModels != nil {
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		all = append(all, filterDisabledModelsForVendor(vendor.ID(), models)...)
	}
	if len(all) == 0 && firstErr != nil {
		return nil, errorToPluginError(firstErr)
	}
	all = append(all, extraModels(loadedConfig())...)
	return mergeSameNameModels(all), nil
}

// mergeSameNameModels 按裸模型名合并同名条目，参数取并集最优。
//
// 为什么要合并：宿主按 EqualFold 匹配模型 ID，同名条目在它眼里就是同一个模型。
// 不合并的话 /v1/models 会列出重复项，而客户端只会看到其中一个。
//
// 「取并集最优」的含义：上下文窗口与输出上限取大（不同供应商对同一模型的
// 参数认知可能不同，取大更宽松），能力标志取或，推理档位取并集。
func mergeSameNameModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
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
		if modelSupportsImages(model) {
			target.SupportedInputModalities = appendUnique(target.SupportedInputModalities, "image")
		}
		if modelSupportsTools(model) {
			target.SupportedGenerationMethods = appendUnique(target.SupportedGenerationMethods, "tool_calls")
		}
		target.Thinking = mergeThinking(target.Thinking, model.Thinking)
	}
	return merged
}

// pluginapi.ModelInfo 的能力以字符串列表表达（没有布尔字段），
// 因此这两个判断是「解释」而不是直接读字段。

// modelSupportsTools 报告模型是否声明了工具调用能力。
func modelSupportsTools(model pluginapi.ModelInfo) bool {
	for _, method := range model.SupportedGenerationMethods {
		if strings.EqualFold(strings.TrimSpace(method), "tool_calls") {
			return true
		}
	}
	return false
}

// modelSupportsImages 报告模型是否声明了图片输入能力。
func modelSupportsImages(model pluginapi.ModelInfo) bool {
	for _, modality := range model.SupportedInputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
}

// mergeThinking 合并两份推理档位声明（档位取并集，能力取或）。
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

// appendUnique 追加一个尚不存在的元素。
func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// extraModels 把配置里的额外模型名转成模型条目。
//
// 用途：上游目录探测失败、或想手打注册某个未出现在目录里的模型。
// 名字可带供应商前缀（workbuddycn:glm-5.2）精确指定，也可不带（对所有启用
// 供应商注册）。
func extraModels(cfg pluginConfig) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(cfg.ExtraModels))
	for _, name := range cfg.ExtraModels {
		bare := strings.TrimSpace(name)
		if bare == "" {
			continue
		}
		if vendorID, bareModel := core.SplitModelID(bare); vendorID != "" {
			if vendor, okVendor := core.VendorByID(vendorID); okVendor &&
				realmEnabled(cfg, vendor.Region()) &&
				!isModelDisabled(vendor.ID(), bareModel) {
				out = append(out, extraModelInfo(vendor.ID(), bareModel))
			}
			continue
		}
		for _, vendor := range core.Vendors() {
			if !realmEnabled(cfg, vendor.Region()) || isModelDisabled(vendor.ID(), bare) {
				continue
			}
			out = append(out, extraModelInfo(vendor.ID(), bare))
		}
	}
	return out
}

// extraModelInfo 构造一条额外模型条目。
func extraModelInfo(vendorID, bareModel string) pluginapi.ModelInfo {
	return core.ModelInfoToPluginAPI(vendorID, core.ModelDescriptor{
		ID:            bareModel,
		Name:          bareModel,
		ContextLength: defaultContextWindowFor(bareModel),
	})
}

// defaultContextWindowFor 返回模型的默认上下文窗口（探测失败时兜底）。
func defaultContextWindowFor(modelID string) int64 {
	return workbuddy.DefaultContextWindowFor(modelID)
}

// applyModelCatalog 在注册/重配置时预热模型缓存。
//
// 灌入上次的状态缓存能让 /v1/models 立刻有内容，不必等首次上游探测完成。
func applyModelCatalog(cfg pluginConfig) {
	// 注入模型探测用的凭证来源：从宿主列出的凭证里挑第一个可用的。
	workbuddy.SetProbeCredentialFunc(func(region workbuddy.Region) *workbuddy.Credential {
		return probeCredentialForRealm(region)
	})
	cached := loadCachedModelsFromState()
	if len(cached) == 0 {
		return
	}
	for region, models := range cached {
		workbuddy.LoadCachedModels(models, region)
	}
}

// probeCredentialForRealm 为模型探测取一个该区域的可用凭证。
//
// 模型目录的探测接口需要凭证鉴权，因此必须借一个同区域的账号去打。
// 取不到就放弃（探测失败会退回静态兜底表，不影响可用性）。
func probeCredentialForRealm(region workbuddy.Region) *workbuddy.Credential {
	ctx := context.Background()
	entries, errList := listHostAuths(ctx, "")
	if errList != nil {
		logger.Debug("list host auths for model probe failed: %v", errList)
		return nil
	}
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		stored, okStored := getAuthJSONByIndex(ctx, "", entry.AuthIndex)
		if !okStored {
			continue
		}
		// 按凭证自身的归属解析，避免用错区域的账号去打探测接口。
		if vendor, okVendor := parseVendorCredential(stored, entry.Name, nil); okVendor {
			if vendor.RegionValue() != string(region) {
				continue
			}
			if native, okNative := vendor.Native.(*workbuddy.Credential); okNative {
				return native
			}
		}
	}
	return nil
}

// loadCachedModelsFromState 从状态文件读取上次缓存的模型清单。
func loadCachedModelsFromState() map[workbuddy.Region][]workbuddy.ModelInfo {
	state := snapshotState()
	if state.ModelsFetchedAt == "" || len(state.Models) == 0 {
		return nil
	}
	out := map[workbuddy.Region][]workbuddy.ModelInfo{}
	for _, cached := range state.Models {
		region := workbuddy.NormalizeRegion(cached.Realm)
		out[region] = append(out[region], workbuddy.ModelInfo{
			ID:                 cached.ID,
			Name:               cached.Name,
			ContextWindow:      cached.ContextLength,
			MaxTokens:          cached.MaxOutputTokens,
			Efforts:            cached.Efforts,
			DefaultEffort:      cached.DefaultEffort,
			SupportsImages:     cached.SupportsImages,
			SupportsToolCall:   cached.SupportsTool,
			CanDisableThinking: cached.CanDisableThink,
			OnlyReasoning:      cached.OnlyReasoning,
			Description:        cached.Description,
		})
	}
	return out
}

// persistModels 把探测结果写回状态文件，供下次启动预热。
func persistModels() {
	cached := workbuddy.CachedModels()
	if len(cached) == 0 {
		return
	}
	flat := make([]cachedModel, 0, len(cached))
	for region, models := range cached {
		for _, model := range models {
			flat = append(flat, cachedModel{
				ID:              model.ID,
				Name:            model.Name,
				Realm:           string(region),
				ContextLength:   model.ContextWindow,
				MaxOutputTokens: model.MaxTokens,
				Efforts:         model.Efforts,
				DefaultEffort:   model.DefaultEffort,
				SupportsImages:  model.SupportsImages,
				SupportsTool:    model.SupportsToolCall,
				CanDisableThink: model.CanDisableThinking,
				OnlyReasoning:   model.OnlyReasoning,
				Description:     model.Description,
			})
		}
	}
	now := nowFunc().Format(time.RFC3339)
	mutateState(func(state *pluginState) {
		state.Models = flat
		state.ModelsFetchedAt = now
	})
}

// modelCatalogMu 串行化模型目录刷新，避免并发探测把上游打满。
var modelCatalogMu sync.Mutex

// refreshModelsFromUpstream 主动刷新所有启用供应商的模型清单（管理页调用）。
func refreshModelsFromUpstream(ctx context.Context) (int, error) {
	modelCatalogMu.Lock()
	defer modelCatalogMu.Unlock()

	total := 0
	var firstErr error
	for _, vendor := range core.Vendors() {
		if !realmEnabled(loadedConfig(), vendor.Region()) {
			continue
		}
		models, errModels := vendor.StaticModels(ctx)
		if errModels != nil {
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		total += len(models)
	}
	if total == 0 && firstErr != nil {
		return 0, errorToPluginError(firstErr)
	}
	persistModels()
	return total, nil
}
