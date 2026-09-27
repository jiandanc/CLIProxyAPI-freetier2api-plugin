package main

// 本文件实现模型的按域禁用/启用。
//
// 语义与「禁用」的落点：被禁用的模型不再注册给宿主对应凭证，因此：
//   - 国内/海外账号可对同名模型独立禁用；
//   - 宿主不会把请求路由到被禁用的域；若两域均被禁用，宿主直接返回 model_not_found；
//   - 请求执行层通过内存读写锁缓存进行轻量零锁校验。

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
)

var (
	disabledCacheMu  sync.RWMutex
	disabledCacheSet = map[string]bool{}
)

// reloadDisabledModelCache 刷新内存中的禁用模型查询集合。
func reloadDisabledModelCache(disabled []string) {
	disabledCacheMu.Lock()
	defer disabledCacheMu.Unlock()
	m := make(map[string]bool, len(disabled)*2)
	for _, id := range disabled {
		trimmed := strings.TrimSpace(id)
		if trimmed != "" {
			m[trimmed] = true
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
	return disabledCacheSet[core.ModelKeyFor(vendorID, bare)]
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
				}
			}
		}
		next := make([]string, 0, len(current))
		for id := range current {
			next = append(next, id)
		}
		sort.Strings(next)
		state.DisabledModels = next
		reloadDisabledModelCache(next)
	})

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
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return ""
	}
	prefix, bare := core.SplitModelID(trimmed)
	if prefix == "" {
		return bare
	}
	if _, okVendor := core.VendorByID(prefix); okVendor {
		return core.ModelKeyFor(prefix, bare)
	}
	// 区域别名：展开成该区域下每个供应商的键。多个键用换行分隔，
	// 由保存逻辑拆开（这里返回单个字符串以保持签名简单）。
	keys := make([]string, 0, 4)
	for _, vendor := range core.Vendors() {
		if vendor.Region() == prefix {
			keys = append(keys, core.ModelKeyFor(vendor.ID(), bare))
		}
	}
	if len(keys) == 0 {
		return ""
	}
	return strings.Join(keys, "\n")
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
