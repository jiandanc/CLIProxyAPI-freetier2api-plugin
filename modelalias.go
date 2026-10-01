package main

// 本文件实现模型的**别名**：给某供应商的某个模型起一个对外名称。
//
// 别名就是该模型注册给宿主的**对外模型 ID**——不是备注、也不改动官方 ID 本身。
// 它解决两件事：
//
//  1. 把官方 ID 换成顺手的名字（如 Qoder 的 qmodel_38max → qwen-max）；
//  2. **跨供应商同名路由**：把不同供应商的不同模型设成同一个别名，宿主按
//     strings.EqualFold 合并为一个条目，选号时在这几家之间自动调度——
//     任意一家可用即可应答，天然故障转移。
//
// 作用范围是**供应商内的单个模型 ID**：同名的官方 ID 出现在别的供应商时不受
// 影响，各家可各自起别名。
//
// 出站时再把别名还原成官方 ID（见 resolveOutboundModel）：客户端只认别名，
// 上游只认官方 ID，两者在请求边界处转换。

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

var (
	modelAliasMu    sync.RWMutex
	modelAliasCache = map[string]string{}
)

// aliasScopedKey 是别名表的键：<vendorID>:<官方模型 ID>。
func aliasScopedKey(vendorID, modelID string) string {
	return strings.ToLower(strings.TrimSpace(vendorID)) + ":" + strings.TrimSpace(modelID)
}

// refreshModelAliasCache 从状态文件重算别名表。调用方**不得**持有 stateMu。
func refreshModelAliasCache() {
	reloadModelAliasCache(snapshotState().ModelAliases)
}

// reloadModelAliasCache 用给定映射整体替换内存中的别名表。
func reloadModelAliasCache(aliases map[string]string) {
	next := make(map[string]string, len(aliases))
	for key, alias := range aliases {
		trimmedKey := strings.TrimSpace(key)
		trimmedAlias := strings.TrimSpace(alias)
		if trimmedKey == "" || trimmedAlias == "" {
			continue
		}
		next[trimmedKey] = trimmedAlias
	}
	modelAliasMu.Lock()
	modelAliasCache = next
	modelAliasMu.Unlock()
}

// modelAliasFor 返回某供应商某模型设置的别名；未设置时返回空串。
func modelAliasFor(vendorID, modelID string) string {
	modelAliasMu.RLock()
	defer modelAliasMu.RUnlock()
	return modelAliasCache[aliasScopedKey(vendorID, modelID)]
}

// resolveOutboundModel 把客户端请求的模型名还原成上游的官方 ID。
//
// 客户端拿到的是别名，上游只认官方 ID。别名表按 **<vendorID>:别名** 反查官方
// ID（官方 ID 是表里的键尾），命中即替换。未设别名时原样返回——Qoder 等
// 直接以上游 key 注册的供应商本就无需翻译。
//
// 反查命中即返回：aliasScopedKey 把供应商与模型 ID 都归一成小写，管理页写入时
// 已保证「同一供应商内别名唯一」，因此用别名反查只会命中一条，遍历顺序无影响。
// 别名比对用 EqualFold：宿主按 EqualFold 匹配模型名，客户端可能用不同大小写
// 请求同一别名，这里必须同样宽松，否则会把它当成未知模型透传给上游。
func resolveOutboundModel(vendorID, model string) string {
	requested := strings.TrimSpace(model)
	if requested == "" {
		return model
	}
	prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
	modelAliasMu.RLock()
	defer modelAliasMu.RUnlock()
	for key, alias := range modelAliasCache {
		if strings.EqualFold(alias, requested) && strings.HasPrefix(key, prefix) {
			return strings.TrimPrefix(key, prefix)
		}
	}
	return model
}

// applyModelAliases 给某供应商的模型清单换上别名（对外 ID）。
//
// 只改 ID 与名称：别名是「这个模型对外叫什么」，因此名称也随之一致，
// 客户端与 /v1/models 看到的都是同一个名字。元数据（上下文、能力、档位）不动。
func applyModelAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if len(models) == 0 {
		return models
	}
	modelAliasMu.RLock()
	defer modelAliasMu.RUnlock()
	if len(modelAliasCache) == 0 {
		return models
	}
	prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if alias := modelAliasCache[prefix+strings.TrimSpace(model.ID)]; alias != "" {
			model.ID = alias
			model.Name = alias
			model.DisplayName = alias
		}
		out = append(out, model)
	}
	return out
}

// modelAliasRequest 是设置/清除别名的请求。
type modelAliasRequest struct {
	// Model 是基础模型 ID（<vendorID>:<官方模型 ID>），即模型表里的 scope_id。
	Model string `json:"model"`
	// Alias 是目标别名；留空表示清除别名（恢复用官方 ID）。
	Alias string `json:"alias"`
}

// handleModelAlias 设置或清除某个模型的别名。
func handleModelAlias(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body modelAliasRequest
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{
				"ok": false, "error": "请求体解析失败：" + errUnmarshal.Error(),
			})
		}
	}

	vendorID, bare := core.SplitModelID(strings.TrimSpace(body.Model))
	if vendorID == "" || strings.TrimSpace(bare) == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "model 必须是 <vendor>:<模型 ID> 形式",
		})
	}
	alias := strings.TrimSpace(body.Alias)
	if strings.ContainsAny(alias, " \t\r\n") {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "别名不能包含空白字符",
		})
	}

	scoped := aliasScopedKey(vendorID, bare)
	mutateState(func(state *pluginState) {
		if state.ModelAliases == nil {
			state.ModelAliases = map[string]string{}
		}
		// 别名变了：对外 ID 随之改变，标记「待生效」。
		state.RestartPending = true
		if alias == "" {
			delete(state.ModelAliases, scoped)
			reloadModelAliasCache(state.ModelAliases)
			return
		}
		// 同一供应商内别名唯一：清掉该供应商下占用同名别名的旧模型，
		// 否则反查（resolveOutboundModel）会命中两条、结果随机。
		prefix := strings.ToLower(strings.TrimSpace(vendorID)) + ":"
		for existing, existingAlias := range state.ModelAliases {
			if existing != scoped && strings.EqualFold(existingAlias, alias) && strings.HasPrefix(existing, prefix) {
				delete(state.ModelAliases, existing)
			}
		}
		state.ModelAliases[scoped] = alias
		reloadModelAliasCache(state.ModelAliases)
	})

	return jsonResponse(http.StatusOK, map[string]any{"ok": true})
}
