package main

// 本文件实现模型的禁用/启用。
//
// 语义与「禁用」的落点：被禁用的模型**不再注册给宿主**，因此：
//   - 不出现在 /v1/models；
//   - 宿主不会把它路由到本插件，客户端请求它会直接得到 model_not_found。
//
// 落点是**注册层**而不是执行层：宿主在模型解析阶段就会拒绝未知模型，
// 插件没有机会介入，所以必须在注册清单里就把它们去掉。
//
// 禁用清单存在插件状态目录（state.json），与账号状态同生命周期。

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"workbuddy2api-plugin/cpasdk/pluginapi"
	"workbuddy2api-plugin/internal/cb"
	"workbuddy2api-plugin/internal/logger"
)

// modelToggleRequest 是模型禁用/启用请求。
type modelToggleRequest struct {
	// Models 是要操作的模型 ID（带 cn:/global: 前缀的完整 ID）。
	Models []string `json:"models"`
	// Disabled 为目标状态：true 禁用、false 启用。
	Disabled bool `json:"disabled"`
}

// disabledModelSet 返回当前被禁用的模型集合（副本）。
func disabledModelSet() map[string]bool {
	state := snapshotState()
	out := make(map[string]bool, len(state.DisabledModels))
	for _, id := range state.DisabledModels {
		out[strings.TrimSpace(id)] = true
	}
	return out
}

// modelDisabled 报告某个模型是否被禁用。
func modelDisabled(modelID string) bool {
	return disabledModelSet()[strings.TrimSpace(modelID)]
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
	// 兼容 {"models": "a,b"} 的字符串写法。
	if len(body.Models) == 0 {
		fields := managementBody(req)
		body.Models = stringSliceField(fields, "models")
		if raw, okRaw := fields["disabled"].(bool); okRaw {
			body.Disabled = raw
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
	})

	logger.Info("models %s: %d 个（%s）", toggleVerb(body.Disabled), len(targets),
		strings.Join(targets, ", "))

	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": len(targets), "disabled": body.Disabled,
		"disabled_models": snapshotState().DisabledModels,
		"message": "已" + toggleVerb(body.Disabled) + " " + itoa(len(targets)) + " 个模型；" +
			"重启宿主后 /v1/models 生效",
	})
}

func toggleVerb(disabled bool) string {
	if disabled {
		return "禁用"
	}
	return "启用"
}

// filterDisabledModels 从模型清单里剔除被禁用的模型。
//
// 用在**注册路径**（model.static / model.for_auth）：被禁用的模型不进宿主，
// 客户端就选不到、也请求不到。
func filterDisabledModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	disabled := disabledModelSet()
	if len(disabled) == 0 {
		return models
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if disabled[strings.TrimSpace(model.ID)] {
			continue
		}
		out = append(out, model)
	}
	return out
}

// withDisabledFlag 给控制台页的模型清单打上禁用标记。
func withDisabledFlag(models []consoleModel) []consoleModel {
	disabled := disabledModelSet()
	for index := range models {
		models[index].Disabled = disabled[strings.TrimSpace(models[index].ID)]
	}
	return models
}

// itoa 小工具（避免为一次转换引入 strconv）。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// 编译期断言：cb 包仍被本文件使用（模型过滤依赖它的类型）。
var _ = cb.ModelInfo{}
