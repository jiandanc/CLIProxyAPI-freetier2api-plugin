package main

// 本文件实现模型的按域禁用/启用。
//
// 语义与「禁用」的落点：被禁用的模型不再注册给宿主对应凭证，因此：
//   - 国内/海外账号可对同名模型独立禁用；
//   - 宿主不会把请求路由到被禁用的域；若两域均被禁用，宿主直接返回 model_not_found；
//   - 请求执行层通过内存读写锁缓存进行轻量零锁校验。
//
// 禁用清单有两个来源，取**并集**：
//   - config.yaml 的 plugins.configs.freetier2api.disabled_models（声明式，批量管理）；
//   - 控制台页面的「禁用选中/启用选中」按钮，写入状态文件 disabled_models（临时调整）。
//
// 页面无法放开配置里声明的禁用项——配置是声明式的最终意图；要放开就改配置。
// 两个来源都变化时都调用 refreshDisabledModelCache 重算并集，避免互相覆盖。

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/qoder"
	"freetier2api-plugin/internal/vendors/qoder/bridge"
)

var (
	disabledCacheMu  sync.RWMutex
	disabledCacheSet = map[string]bool{}
)

// mergeDisabledLists 合并两个来源的禁用清单：配置声明 + 状态文件记录。
//
// 只做纯计算、不取任何锁：调用方可能已经持有 stateMu（loadState、mutateState
// 的回调内），在那里读 snapshotState() 会因 stateMu 不可重入而自锁。
func mergeDisabledLists(stateDisabled []string) []string {
	merged := make([]string, 0, len(stateDisabled)+8)
	merged = append(merged, loadedConfig().DisabledModels...)
	merged = append(merged, stateDisabled...)
	return merged
}

// refreshDisabledModelCache 重算禁用集合：配置声明 ∪ 状态文件记录。
//
// 配置与状态各自独立变化（reconfigure / 页面操作），任何一处变化都必须
// 走这个入口重算，否则后写的一方会把另一方冲掉。
// 调用方**不得**持有 stateMu（内部会读 snapshotState）。
func refreshDisabledModelCache() {
	reloadDisabledModelCache(mergeDisabledLists(snapshotState().DisabledModels))
}

// reloadDisabledModelCache 用给定清单整体替换内存中的禁用模型查询集合。
//
// 每一项都经 expandModelKey 归一：调用方可能给出区域别名（cn:glm-5.2）或
// 未展开的历史数据，而查询侧只按规范键（vendorID:裸名）与裸名匹配。
func reloadDisabledModelCache(disabled []string) {
	disabledCacheMu.Lock()
	defer disabledCacheMu.Unlock()
	m := make(map[string]bool, len(disabled)*2)
	for _, id := range disabled {
		for _, key := range expandModelKey(id) {
			if key != "" {
				m[key] = true
			}
		}
	}
	disabledCacheSet = m
}

// isModelDisabled 检查某个供应商下的模型是否被禁用。
//
// 键是 <vendorID>:<裸名>（见 core.ModelKeyFor）。用供应商限定而不是裸名：
// 同一个模型名可能来自多个供应商（如 glm-5.3 同时存在于 workbuddy 与 qoder），
// 禁用其中一家不该影响另一家。
//
// 同时兼容裸键：配置里手写的旧数据可能只有裸名，那表示「所有供应商都禁用」。
func isModelDisabled(vendorID, modelID string) bool {
	bare := strings.TrimSpace(modelID)
	if bare == "" {
		return false
	}
	// 带限定前缀时以限定为准：支持供应商 ID（workbuddycn:glm-5.2）与
	// 区域别名（cn:glm-5.2）两种写法，后者是历史写法。
	if prefix, b := core.SplitModelID(bare); prefix != "" {
		if vendor, okVendor := vendorForPrefix(prefix); okVendor && vendor.ID() != vendorID {
			return false
		}
		bare = b
	}
	disabledCacheMu.RLock()
	defer disabledCacheMu.RUnlock()
	if disabledCacheSet[bare] {
		return true
	}
	if disabledCacheSet[core.ModelKeyFor(vendorID, bare)] {
		return true
	}
	// 兼容 Qoder 历史配置中以内部 SKU (如 gfmodel) 记录的禁用状态
	if vendorID == qoder.VendorIDCN || vendorID == qoder.VendorIDGlobal {
		if sku := bridge.ResolveQoderModelKey(bare); sku != "" && sku != bare {
			if disabledCacheSet[core.ModelKeyFor(vendorID, sku)] || disabledCacheSet[sku] {
				return true
			}
		}
	}
	return false
}

// vendorForPrefix 把限定前缀（供应商 ID 或区域别名）解析成供应商实例。
//
// 区域别名（cn / global）可能对应多个供应商，取第一个匹配的即可——
// 判定「这个限定是否排除了当前供应商」只需要知道它指的是哪个区域。
func vendorForPrefix(prefix string) (core.Vendor, bool) {
	if vendor, okVendor := core.VendorByID(prefix); okVendor {
		return vendor, true
	}
	for _, vendor := range core.Vendors() {
		if vendor.Region() == prefix {
			return vendor, true
		}
	}
	return nil, false
}

// modelToggleRequest 是模型禁用/启用请求。
type modelToggleRequest struct {
	// Models 是要操作的模型 ID（可为 scoped 如 cn:glm-5.2，或裸名）。
	Models []string `json:"models"`
	// Disabled 为目标状态：true 禁用、false 启用。
	Disabled bool `json:"disabled"`
}

// handleModelsToggle 批量禁用/启用模型。
func handleModelsToggle(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body modelToggleRequest
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{
				"ok": false, "error": "请求体解析失败：" + errUnmarshal.Error(),
			})
		}
	}

	targets := make([]string, 0, len(body.Models))
	for _, item := range body.Models {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			targets = append(targets, trimmed)
		}
	}
	if len(targets) == 0 {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "请先选择要操作的模型",
		})
	}

	mutateState(func(state *pluginState) {
		current := make(map[string]bool, len(state.DisabledModels))
		for _, id := range state.DisabledModels {
			current[strings.TrimSpace(id)] = true
		}
		for _, id := range targets {
			// 归一化到 <vendorID>:<裸名> 后再存：页面可能提交区域别名
			// （cn:glm-5.2）或裸名，而查询侧只按规范键匹配。
			for _, key := range strings.Split(normalizeModelKeyForStorage(id), "\n") {
				if key == "" {
					continue
				}
				if body.Disabled {
					current[key] = true
				} else {
					delete(current, key)
					prefix, bare := core.SplitModelID(key)
					if prefix == qoder.VendorIDCN || prefix == qoder.VendorIDGlobal {
						if sku := bridge.ResolveQoderModelKey(bare); sku != "" && sku != bare {
							delete(current, core.ModelKeyFor(prefix, sku))
							delete(current, sku)
						}
					}
				}
			}
		}
		next := make([]string, 0, len(current))
		for id := range current {
			next = append(next, id)
		}
		sort.Strings(next)
		state.DisabledModels = next
		// 锁内只重算内存集合（不含配置并集，避免重入 stateMu）；
		// mutateState 返回后再刷新完整并集。
		reloadDisabledModelCache(next)
	})
	// 配置里声明的禁用项不会被这次页面操作放开：刷新并集。
	refreshDisabledModelCache()

	logger.Info("models %s: %d 个（%s）", toggleVerb(body.Disabled), len(targets),
		strings.Join(targets, ", "))

	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": len(targets), "disabled": body.Disabled,
		"disabled_models": snapshotState().DisabledModels,
		"message":         "已" + toggleVerb(body.Disabled) + " " + strconv.Itoa(len(targets)) + " 个模型（调用即时生效）",
	})
}

// normalizeModelKeyForStorage 把待禁用的模型标识归一成存储键。
//
// 三种输入都要接受：
//   - 裸名（glm-5.2）—— 对所有供应商生效，原样存；
//   - 供应商 ID 前缀（workbuddycn:glm-5.2）—— 已是规范形式，原样存；
//   - 区域别名前缀（cn:glm-5.2）—— 展开成该区域下所有供应商的键。
//
// 返回空字符串表示输入无效（调用方应忽略）。
func normalizeModelKeyForStorage(modelID string) string {
	return strings.Join(expandModelKey(modelID), "\n")
}

// expandModelKey 把待禁用的模型标识展开成查询键：
//   - 裸名（glm-5.2）—— 返回裸名（对所有供应商生效）；
//   - 供应商 ID 前缀（workbuddycn:glm-5.2）—— 返回规范 vendorKey；
//   - 区域别名前缀（cn:glm-5.2）—— 展开成该区域下所有供应商的 vendorKey。
//
// 区域别名展开不出任何供应商时退回裸名：这样配置里写了别名但供应商尚未注册
// （例如该区域被 enabled_realms 关掉）时，禁用意图依然保留。
func expandModelKey(modelID string) []string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return nil
	}
	prefix, bare := core.SplitModelID(trimmed)
	if prefix == "" {
		return []string{bare}
	}
	if _, okVendor := core.VendorByID(prefix); okVendor {
		return []string{core.ModelKeyFor(prefix, bare)}
	}
	keys := make([]string, 0, 4)
	for _, vendor := range core.Vendors() {
		if vendor.Region() == prefix {
			keys = append(keys, core.ModelKeyFor(vendor.ID(), bare))
		}
	}
	if len(keys) > 0 {
		return keys
	}
	return []string{bare}
}

func toggleVerb(disabled bool) string {
	if disabled {
		return "禁用"
	}
	return "启用"
}

// filterDisabledModelsForVendor 从某供应商的模型清单里剔除已被禁用的模型。
func filterDisabledModelsForVendor(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if isModelDisabled(vendorID, model.ID) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// withDisabledFlag 给控制台页的模型清单打上禁用标记。
func withDisabledFlag(models []consoleModel) []consoleModel {
	for index := range models {
		models[index].Disabled = isModelDisabled(models[index].VendorID, models[index].ID)
	}
	return models
}
