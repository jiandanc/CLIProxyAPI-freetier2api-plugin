package main

// 本文件实现 model provider 能力：静态模型清单与按凭证的模型清单。
//
// **realm 隔离就落在这里**。CodeBuddy 的 cn / global 是两套独立部署，
// 凭证不通用：cn 的 token 打 global 域名会得到 401 TOKEN_EXPIRE。
//
// 隔离机制（已用宿主源码验证）：
//  1. ModelsForAuth 按凭证的 realm 只返回该域的模型 ID（cn 凭证只返回 cn:*）；
//  2. 宿主把这份清单注册成「该凭证支持的模型」（RegisterClient(authID, provider, models)）；
//  3. 宿主选凭证时调用 ClientSupportsModel(authID, requestedModel) 过滤候选
//     （sdk/cliproxy/auth/conductor_selection.go:1202、:993）。
//
// 于是请求 cn:glm-5.2 时，global 凭证会被宿主在选号阶段就排除，
// 插件不需要自己实现任何调度逻辑。

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api-plugin/cpasdk/pluginapi"
	"workbuddy2api-plugin/internal/cb"
	"workbuddy2api-plugin/internal/httpx"
	"workbuddy2api-plugin/internal/logger"
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
// 静态清单是「全量目录」：两个域、所有已知模型。它不参与凭证路由
// （那是 ModelsForAuth 的职责），只用于展示与让客户端能选到模型。
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

// handleModelForAuth 返回某个凭证可用的模型清单。
//
// 这是 realm 隔离的执行点：只返回与凭证同域的模型，宿主据此把
// 「凭证 ↔ 模型」绑定起来，从而在选号阶段淘汰跨域凭证。
func handleModelForAuth(request []byte) ([]byte, error) {
	var rpc authModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := httpx.WithCallbackID(context.Background(), rpc.HostCallbackID)

	credential, errCredential := credentialForAuth(ctx, rpc.HostCallbackID, rpc.StorageJSON, rpc.AuthID, rpc.Attributes)
	if errCredential != nil {
		// 取不到凭证时返回空清单而不是报错：宿主会因为空清单而不再路由到
		// 这个凭证，这正是我们想要的结果（避免用坏凭证打上游）。
		logger.Debug("model.for_auth: no credential for %s: %v", rpc.AuthID, errCredential)
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}

	region := credential.Realm()
	cfg := loadedConfig()
	if !realmEnabled(cfg, string(region)) {
		// 域被配置关闭：同样返回空清单，让宿主自然跳过该账号。
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}

	models, errModels := modelsForRealm(ctx, region)
	if errModels != nil {
		logger.Debug("model.for_auth: probe models for %s failed: %v", region, errModels)
		return okEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: models})
}

// staticModels 返回两个域的模型并集（带 realm 前缀）。
func staticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	cfg := loadedConfig()
	out := make([]pluginapi.ModelInfo, 0, 64)
	var firstErr error
	for _, region := range []cb.Region{cb.RegionCN, cb.RegionGlobal} {
		if !realmEnabled(cfg, string(region)) {
			continue
		}
		models, errModels := modelsForRealm(ctx, region)
		if errModels != nil {
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		out = append(out, models...)
	}
	// 一个域都探测不到时才报错；只要有一个可用就让插件继续工作。
	if len(out) == 0 && firstErr != nil {
		return nil, errorToPluginError(firstErr)
	}
	out = append(out, extraModels(cfg)...)
	// 禁用的模型不进宿主（extra_models 同样遵守）。
	return filterDisabledModels(out), nil
}

// modelsForRealm 返回某个域的模型（带该域前缀）。
func modelsForRealm(ctx context.Context, region cb.Region) ([]pluginapi.ModelInfo, error) {
	client := newUpstreamClient(ctx)
	models, errModels := client.FetchModels(region)
	if errModels != nil {
		return nil, errModels
	}
	// 刷新档位表：对话路径需要它来决定 reasoning_effort 的合法值。
	publishEffortTables(models)

	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		out = append(out, ModelInfoToPluginAPI(region, model))
	}
	// 禁用的模型不进宿主：在注册层剔除，客户端就选不到。
	return filterDisabledModels(out), nil
}

// extraModels 把配置里的额外模型名转成模型条目。
//
// 用途：上游目录探测失败、或想手打注册某个未出现在目录里的模型。
// 这些条目的窗口与档位走静态兜底表（cb 包内的查找链）。
func extraModels(cfg pluginConfig) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(cfg.ExtraModels))
	for _, name := range cfg.ExtraModels {
		bare := strings.TrimSpace(name)
		if bare == "" {
			continue
		}
		// 配置里可以带域前缀，也可以不带（不带则两个域都注册）。
		if realm, bareModel := SplitModelID(bare); realm != "" {
			region := cb.NormalizeRegion(realm)
			if realmEnabled(cfg, string(region)) {
				out = append(out, extraModelInfo(region, bareModel))
			}
			continue
		}
		for _, region := range []cb.Region{cb.RegionCN, cb.RegionGlobal} {
			if realmEnabled(cfg, string(region)) {
				out = append(out, extraModelInfo(region, bare))
			}
		}
	}
	return out
}

// extraModelInfo 构造一条额外模型条目。
func extraModelInfo(region cb.Region, bareModel string) pluginapi.ModelInfo {
	return ModelInfoToPluginAPI(region, cb.ModelInfo{
		ID:            bareModel,
		Name:          bareModel,
		ContextWindow: cb.DefaultContextWindowFor(bareModel),
	})
}

var (
	effortPublishMu   sync.Mutex
	lastEffortPublish time.Time
)

// publishEffortTables 把模型清单里的档位信息发布给对话路径。
//
// 节流：模型清单会被 /v1/models 与 model.for_auth 反复取用，
// 而档位表内容变化极慢，没必要每次都重建。
func publishEffortTables(models []cb.ModelInfo) {
	effortPublishMu.Lock()
	if time.Since(lastEffortPublish) < time.Minute {
		effortPublishMu.Unlock()
		return
	}
	lastEffortPublish = time.Now()
	effortPublishMu.Unlock()

	defaults := make(map[string]string, len(models))
	supported := make(map[string][]string, len(models))
	for _, model := range models {
		if model.DefaultEffort != "" {
			defaults[model.ID] = model.DefaultEffort
		}
		if len(model.Efforts) > 0 {
			supported[model.ID] = model.Efforts
		}
	}
	cb.SetEffortTables(defaults, supported)
}

// applyModelCatalog 在注册/重配置时预热模型缓存。
//
// 灌入上次的状态缓存能让 /v1/models 立刻有内容，不必等首次上游探测完成。
func applyModelCatalog(cfg pluginConfig) {
	// 注入模型探测用的凭证来源：从宿主列出的凭证里挑第一个可用的。
	cb.SetProbeCredentialFunc(func(region cb.Region) *cb.Credential {
		return probeCredentialForRegion(region)
	})

	cached := loadCachedModelsFromState()
	if len(cached) == 0 {
		return
	}
	for region, models := range cached {
		cb.LoadCachedModels(models, region)
	}
}

// probeCredentialForRegion 为模型探测取一个该域的可用凭证。
func probeCredentialForRegion(region cb.Region) *cb.Credential {
	ctx := context.Background()
	entries, errList := listHostAuths(ctx, "")
	if errList != nil {
		logger.Debug("list host auths for model probe failed: %v", errList)
		return nil
	}
	cfg := loadedConfig()
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		stored, okStored := getAuthJSONByIndex(ctx, "", entry.AuthIndex)
		if !okStored {
			continue
		}
		credential, errParse := cb.ParseCredential(stored, defaultRealmForParse(cfg))
		if errParse != nil {
			continue
		}
		if credential.Realm() == region {
			return credential
		}
	}
	return nil
}

// loadCachedModelsFromState 从状态文件读取上次缓存的模型清单。
func loadCachedModelsFromState() map[cb.Region][]cb.ModelInfo {
	state := snapshotState()
	if state.ModelsFetchedAt == "" || len(state.Models) == 0 {
		return nil
	}
	out := map[cb.Region][]cb.ModelInfo{}
	for _, cached := range state.Models {
		region := cb.NormalizeRegion(cached.Realm)
		out[region] = append(out[region], cb.ModelInfo{
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
func persistModels(client *cb.Client) {
	cached := cb.CachedModels()
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

// refreshModelsFromUpstream 主动刷新两个域的模型清单（管理页调用）。
func refreshModelsFromUpstream(ctx context.Context) (int, error) {
	cfg := loadedConfig()
	client := newUpstreamClient(ctx)
	total := 0
	var firstErr error
	for _, region := range []cb.Region{cb.RegionCN, cb.RegionGlobal} {
		if !realmEnabled(cfg, string(region)) {
			continue
		}
		models, errModels := client.FetchModels(region)
		if errModels != nil {
			if firstErr == nil {
				firstErr = errModels
			}
			continue
		}
		publishEffortTables(models)
		total += len(models)
	}
	if total == 0 && firstErr != nil {
		return 0, firstErr
	}
	persistModels(client)
	return total, nil
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
	return okEnvelope(pluginapi.ModelRegistrationResponse{
		Provider: providerKey,
		Models:   models,
	})
}
