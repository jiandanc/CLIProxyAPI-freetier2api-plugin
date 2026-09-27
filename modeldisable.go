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
	"freetier2api-plugin/internal/vendors/workbuddy"
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

// isModelDisabled 检查指定域下的模型是否被禁用（支持 scoped 如 cn:model 与裸名）。
func isModelDisabled(region workbuddy.Region, modelID string) bool {
	bare := strings.TrimSpace(modelID)
	if bare == "" {
		return false
	}
	if _, b := SplitModelID(bare); b != "" {
		bare = b
	}
	disabledCacheMu.RLock()
	defer disabledCacheMu.RUnlock()
	if disabledCacheSet[bare] {
		return true
	}
	scoped := string(region) + ":" + bare
	return disabledCacheSet[scoped]
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
			if body.Disabled {
				current[id] = true
			} else {
				delete(current, id)
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

func toggleVerb(disabled bool) string {
	if disabled {
		return "禁用"
	}
	return "启用"
}

// filterDisabledModelsForRealm 从某域的模型清单里剔除已被禁用的模型。
func filterDisabledModelsForRealm(region workbuddy.Region, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if isModelDisabled(region, model.ID) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// withDisabledFlag 给控制台页的模型清单打上禁用标记。
func withDisabledFlag(models []consoleModel) []consoleModel {
	for index := range models {
		r := workbuddy.NormalizeRegion(models[index].Realm)
		models[index].Disabled = isModelDisabled(r, models[index].ID)
	}
	return models
}
