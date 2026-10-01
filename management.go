package main

// 本文件实现 management 能力：插件自有管理接口与内嵌控制台页。
//
// 安全边界（重要）：
//   - /v0/resource/plugins/freetier2api/console 只返回**静态页面**，不含任何账号/凭证数据；
//   - 所有读取与动作都走 /v0/management/plugins/freetier2api/...，由 CPA 的管理鉴权保护；
//   - 接口返回里**永远不含 token**（只回传账号名、域、状态、额度与任务结果）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/opencodezen"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

const (
	// managementRequestTimeout 是管理接口里上游调用的上限。
	managementRequestTimeout = 3 * time.Minute
	// managementBatchConcurrency 是批量操作的默认并发度上限。
	managementBatchConcurrency = 4
)

// managementRegistration 返回本插件声明的管理路由与资源页。
func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: "GET", Path: managementRoutePrefix + "/status", Description: "账号、额度与任务状态概览。"},
			{Method: "GET", Path: managementRoutePrefix + "/vendors", Description: "列出已启用的供应商（供添加账号下拉与分组展示）。"},
			{Method: "POST", Path: managementRoutePrefix + "/checkin", Description: "对指定或全部账号执行签到。"},
			{Method: "POST", Path: managementRoutePrefix + "/quotas", Description: "批量查询账号额度。"},
			{Method: "POST", Path: managementRoutePrefix + "/accounts/delete", Description: "删除指定账号的凭证文件（宿主监听 auth 目录，会自动注销该账号）。"},
			{Method: "POST", Path: managementRoutePrefix + "/accounts/add", Description: "添加 API Key 类账号。"},
			{Method: "GET", Path: managementRoutePrefix + "/models", Description: "读取当前注册的模型清单。"},
			{Method: "POST", Path: managementRoutePrefix + "/models/toggle", Description: "批量禁用/启用模型。"},
			{Method: "POST", Path: managementRoutePrefix + "/models/alias", Description: "给模型设置/清除对外别名（跨供应商同名可合并路由）。"},
			{Method: "POST", Path: managementRoutePrefix + "/models/refresh", Description: "从上游拉取模型清单并缓存。"},
			{Method: "GET", Path: managementRoutePrefix + "/logs", Description: "读取插件日志（增量）。"},
			{Method: "POST", Path: managementRoutePrefix + "/settings", Description: "更新插件运行期设置（自动签到、任务排程、提示词）。"},
			{Method: "POST", Path: managementRoutePrefix + "/tasks/scan", Description: "扫描全部账号的待办任务。"},
			{Method: "POST", Path: managementRoutePrefix + "/tasks/run", Description: "执行任务队列（账号内串行、账号间并发）。"},
			{Method: "GET", Path: managementRoutePrefix + "/tasks/queue", Description: "查询任务队列进度。"},
			{Method: "POST", Path: managementRoutePrefix + "/tasks/auto", Description: "对单个账号执行单个任务或一键完成。"},
			{Method: "POST", Path: managementRoutePrefix + "/tasks/auto_all", Description: "对全部账号依次执行一键完成任务。"},
			{Method: "GET", Path: managementRoutePrefix + "/school/status", Description: "开学季活动状态。"},
			{Method: "POST", Path: managementRoutePrefix + "/school/run", Description: "执行开学季闭环。"},
			{Method: "POST", Path: managementRoutePrefix + "/travel", Description: "手动执行猫猫旅行。"},
			{Method: "POST", Path: managementRoutePrefix + "/activity", Description: "手动执行活跃上报。"},
			{Method: "POST", Path: managementRoutePrefix + "/keepalive", Description: "手动执行 token 保活。"},
			{Method: "POST", Path: managementRoutePrefix + "/blackcat", Description: "手动执行夜猫子补足。"},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/console",
				Menu:        pluginDisplayName,
				Description: "FreeTier 账号、任务与额度管理页。",
			},
		},
	}
}

// handleManagement 是管理接口的总分发。
//
// 用守卫式 switch 而非 map：路径匹配需要容忍宿主传全路径或子路径两种形态，
// 写成函数比塞进 map 更清楚。
func handleManagement(request []byte) ([]byte, error) {
	var rpc pluginapi.ManagementRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	method := strings.ToUpper(strings.TrimSpace(rpc.Method))
	if method == "" {
		method = http.MethodGet
	}

	switch {
	case method == http.MethodGet && matchesResourcePath(rpc.Path, "/console"):
		return okEnvelope(htmlResponse(http.StatusOK, []byte(consolePageHTML(managementRoutePrefix))))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, "/status"):
		return okEnvelope(jsonResponse(http.StatusOK, buildStatusPayload(rpc)))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, "/vendors"):
		return okEnvelope(jsonResponse(http.StatusOK, buildVendorsPayload()))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, "/logs"):
		return okEnvelope(jsonResponse(http.StatusOK, buildLogsPayload(rpc)))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/checkin"):
		return okEnvelope(handleCheckinRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/quotas"):
		return okEnvelope(handleQuotasRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/accounts/delete"):
		return okEnvelope(handleAccountDeleteRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/accounts/add"):
		return okEnvelope(handleAccountAddRequest(rpc))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, "/models"):
		return okEnvelope(handleModelsList(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/models/toggle"):
		return okEnvelope(handleModelsToggle(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/models/alias"):
		return okEnvelope(handleModelAlias(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/models/refresh"):
		ctx, cancel := managementContext(rpc)
		defer cancel()
		count, errRefresh := refreshModelsFromUpstream(ctx)
		if errRefresh != nil {
			return okEnvelope(errorResponse(errRefresh))
		}
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"ok": true, "models": count}))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/settings"):
		return okEnvelope(handleSettingsRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/tasks/scan"):
		return okEnvelope(handleTasksScan(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/tasks/run"):
		return okEnvelope(handleTasksRun(rpc))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, "/tasks/queue"):
		return okEnvelope(jsonResponse(http.StatusOK, taskQueueSnapshot()))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/tasks/auto_all"):
		return okEnvelope(handleTasksAutoAll(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/tasks/auto"):
		return okEnvelope(handleTasksAuto(rpc))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, "/school/status"):
		return okEnvelope(handleSchoolStatus(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/school/run"):
		return okEnvelope(handleScheduledRun(rpc, "school"))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/travel"):
		return okEnvelope(handleScheduledRun(rpc, "travel"))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/activity"):
		return okEnvelope(handleScheduledRun(rpc, "activity"))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/keepalive"):
		return okEnvelope(handleKeepaliveRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/blackcat"):
		return okEnvelope(handleScheduledRun(rpc, "blackcat"))
	}

	return okEnvelope(jsonResponse(http.StatusNotFound, map[string]any{
		"error": "not found", "path": rpc.Path, "method": method,
	}))
}

// matchesManagementPath 判断路径是否命中某个管理子路径。
//
// 容忍宿主传全路径（/v0/management/plugins/freetier2api/x）或子路径（/plugins/freetier2api/x）。
func matchesManagementPath(path, suffix string) bool {
	normalized := normalizeRequestPath(path)
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return normalized == managementRoutePrefix+suffix ||
		normalized == "/v0/management"+managementRoutePrefix+suffix
}

// matchesResourcePath 判断路径是否命中资源页。
func matchesResourcePath(path, suffix string) bool {
	normalized := normalizeRequestPath(path)
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return normalized == "/v0/resource/plugins/"+pluginID+suffix
}

// normalizeRequestPath 去掉查询串与尾斜杠。
func normalizeRequestPath(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	if index := strings.IndexByte(trimmed, '?'); index >= 0 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

// managementContext 给管理接口里的上游调用一个可取消的上下文。
//
// host_callback_id 从 HTTP 头取（管理接口是 HTTP 进入的，回调 ID 走 header 而非 body）。
func managementContext(req pluginapi.ManagementRequest) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), managementRequestTimeout)
	return httpx.WithCallbackID(ctx, hostCallbackID(req)), cancel
}

// hostCallbackID 从管理请求头里取宿主回调 ID。
func hostCallbackID(req pluginapi.ManagementRequest) string {
	if req.Headers == nil {
		return ""
	}
	return strings.TrimSpace(req.Headers.Get("X-Host-Callback-Id"))
}

// 三个响应构造器。

// 管理响应用宿主的 pluginapi.ManagementResponse。
//
// **必须用它、且保持 Go 字段名**：该结构体没有 json tag，宿主按
// StatusCode / Headers / Body 三个键解码；Body 是 []byte，Go 会编码成
// base64，宿主解码后把原始字节写给浏览器。这是宿主约定的线格式。
//
// （曾试图改成 snake_case + json.RawMessage 的自有格式，结果宿主反序列化
// 失败，页面报 "plugin resource handler failed" 且什么都不显示。）
func htmlResponse(status int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{htmlContentType}},
		Body:       body,
	}
}

func jsonResponse(status int, payload any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		raw = []byte(`{"error":"marshal failed"}`)
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{jsonContentType}},
		Body:       raw,
	}
}

// errorResponse 把内部错误转成带状态码的 JSON 响应。
func errorResponse(err error) pluginapi.ManagementResponse {
	status := http.StatusInternalServerError
	message := "unknown error"
	if err != nil {
		message = err.Error()
	}
	if pluginErr, okPlugin := err.(*pluginError); okPlugin {
		message = pluginErr.Message
		if pluginErr.HTTPStatus > 0 {
			status = pluginErr.HTTPStatus
		}
	}
	return jsonResponse(status, map[string]any{"error": message, "ok": false})
}

// consoleModel 是控制台页展示用的模型条目。
//
// 刻意**自定义**而不直接回传 pluginapi.ModelInfo：宿主对插件的管理响应是原样
// 透传的，而 pluginapi.ModelInfo **没有 json tag**，字段名会变成 Go 字段名
// （ID / ContextLength / DisplayName）。页面按小写键读就会全部拿到 undefined，
// 表现成一屏的 "--"。这里显式声明契约，两端一致。
type consoleModel struct {
	ID string `json:"id"`
	// ScopeID 是带供应商限定的禁用键（<vendor>:<id>），供页面做勾选与禁用操作。
	ScopeID string `json:"scope_id"`
	// VendorID 是模型所属的供应商实例（workbuddycn 等），页面用它做分组与筛选。
	VendorID string `json:"vendor_id"`
	// VendorName 是供应商展示名（如「WorkBuddy 国内版」）。
	VendorName string `json:"vendor_name"`
	Realm      string `json:"realm"`
	Name       string `json:"name,omitempty"`
	// Alias 是该模型设置的对外别名（未设置时为空；对外 ID 届时即 Alias）。
	Alias           string   `json:"alias,omitempty"`
	Description     string   `json:"description,omitempty"`
	ContextLength   int64    `json:"context_length,omitempty"`
	MaxOutputTokens int64    `json:"max_output_tokens,omitempty"`
	Efforts         []string `json:"efforts,omitempty"`
	DefaultEffort   string   `json:"default_effort,omitempty"`
	SupportsImages  bool     `json:"supports_images,omitempty"`
	SupportsTools   bool     `json:"supports_tools,omitempty"`
	Credits         string   `json:"credits,omitempty"`
	Disabled        bool     `json:"disabled,omitempty"`
}

// handleModelsList 返回本插件当前注册的模型清单。
//
// 用途：控制台页展示模型与它们的元数据（上下文窗口、推理档位、能力）。
// 数据来自模型目录缓存（不触发上游请求），因此响应很快。
// nolint: revive // req 保留是为了与其它管理处理器签名一致。
func handleModelsList(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)
	cfg := loadedConfig()
	models := make([]consoleModel, 0, 128)

	active := activeVendorSet(ctx, callbackID)

	// 逐个供应商取模型。如果供应商没有添加 auth/key，不要展示该供应商的模型。
	for _, vendor := range core.Vendors() {
		if !realmEnabled(cfg, vendor.Region()) {
			continue
		}
		if !active[vendor.ID()] {
			continue
		}
		models = append(models, consoleModelsForVendor(vendor)...)
	}

	// 额外注册的模型（配置里的 extra_models）也一并展示（前提是该供应商有凭证）。
	for _, extra := range extraModels(cfg) {
		// 供应商取自 OwnedBy：extraModelInfo 已把 ID 归一成裸名，再 SplitModelID 会丢掉前缀。
		vendorID := extra.OwnedBy
		if vendorID != "" {
			if _, okVendor := core.VendorByID(vendorID); !okVendor {
				continue
			}
			if !active[vendorID] {
				continue
			}
		}
		models = append(models, consoleModel{
			ID:            extra.ID,
			ScopeID:       core.ModelKeyFor(vendorID, extra.ID),
			VendorID:      vendorID,
			VendorName:    vendorNameFor(vendorID),
			Realm:         vendorRegionFor(vendorID),
			Name:          firstNonEmptyString(extra.DisplayName, extra.Name),
			ContextLength: extra.ContextLength,
		})
	}

	// 打上禁用标记供页面显示状态与勾选。
	//
	// 这里**不做过滤**：禁用的模型仍要列出来，否则用户看不到自己禁用了什么、
	// 也无法再启用。真正生效的过滤在注册路径（models.go 的 staticModels）。
	models = withDisabledFlag(models)
	sort.Slice(models, func(i, j int) bool {
		// 先按供应商名分组，组内按模型 ID —— 与账号表的排序口径一致。
		if models[i].VendorName != models[j].VendorName {
			return models[i].VendorName < models[j].VendorName
		}
		return models[i].ID < models[j].ID
	})

	payload := map[string]any{"ok": true, "models": models, "count": len(models)}
	if len(models) == 0 {
		// 缓存为空通常意味着还没探测过：给页面一句可执行的提示。
		payload["error"] = "本地暂无模型缓存，正在从上游获取；也可点「从上游刷新」立即拉取。"
	}
	return jsonResponse(http.StatusOK, payload)
}

// consoleModelsForVendor 取某个供应商的模型清单（供控制台页展示）。
//
// 走协议层各自的缓存：WorkBuddy 的缓存按区域存（含上下文窗口与推理档位），
// Qoder 的清单需要按凭证向上游拉（有内置兜底）。取不到时返回空——
// 页面少显示几个模型比让整个模型页报错好。
func consoleModelsForVendor(vendor core.Vendor) []consoleModel {
	switch vendor.ID() {
	case workbuddy.VendorIDCN, workbuddy.VendorIDGlobal:
		region := workbuddy.NormalizeRegion(vendor.Region())
		cached := workbuddy.CachedModels()[region]
		out := make([]consoleModel, 0, len(cached))
		for _, model := range cached {
			out = append(out, consoleModel{
				ID:              model.ID,
				ScopeID:         core.ModelKeyFor(vendor.ID(), model.ID),
				VendorID:        vendor.ID(),
				VendorName:      vendor.Name(),
				Realm:           vendor.Region(),
				Name:            model.Name,
				Description:     model.Description,
				ContextLength:   model.ContextWindow,
				MaxOutputTokens: model.MaxTokens,
				Efforts:         model.Efforts,
				DefaultEffort:   model.DefaultEffort,
				SupportsImages:  model.SupportsImages,
				SupportsTools:   model.SupportsToolCall,
				Credits:         model.Credits,
			})
		}
		return out
	default:
		// 其它供应商（Qoder）：从刚注册的模型清单回读。
		// 这里不主动打上游——管理页的「读取模型清单」应当是轻量操作，
		// 真正的上游探测由「从上游刷新」按钮触发。
		return consoleModelsFromRegistry(vendor)
	}
}

// consoleModelsFromRegistry 从插件注册表里已有的模型清单回读某个供应商的模型。
//
// 数据源与 model.static 相同（宿主已持有），因此不会额外打上游。
func consoleModelsFromRegistry(vendor core.Vendor) []consoleModel {
	infos := registeredModelsForVendor(vendor.ID())
	out := make([]consoleModel, 0, len(infos))
	for _, info := range infos {
		entry := consoleModel{
			ID:              info.ID,
			ScopeID:         core.ModelKeyFor(vendor.ID(), info.ID),
			VendorID:        vendor.ID(),
			VendorName:      vendor.Name(),
			Realm:           vendor.Region(),
			Name:            firstNonEmptyString(info.DisplayName, info.Name),
			Description:     info.Description,
			ContextLength:   info.ContextLength,
			MaxOutputTokens: info.MaxCompletionTokens,
			SupportsImages:  modelSupportsImages(info),
			SupportsTools:   modelSupportsTools(info),
		}
		if info.Thinking != nil {
			entry.Efforts = info.Thinking.Levels
		}
		out = append(out, entry)
	}
	return out
}

// vendorNameFor 返回供应商展示名（未知 id 返回空串）。
func vendorNameFor(vendorID string) string {
	if vendor, okVendor := core.VendorByID(vendorID); okVendor {
		return vendor.Name()
	}
	return ""
}

// vendorRegionFor 返回供应商的区域（未知 id 返回空串）。
func vendorRegionFor(vendorID string) string {
	if vendor, okVendor := core.VendorByID(vendorID); okVendor {
		return vendor.Region()
	}
	return ""
}

// buildStatusPayload 构造账号状态概览。
//
// **不含任何 token**：只回传标识、域、状态、额度与任务记录。
func buildStatusPayload(req pluginapi.ManagementRequest) map[string]any {
	cfg := loadedConfig()
	state := snapshotState()
	ctx, cancel := managementContext(req)
	defer cancel()

	accounts := listAccountSummaries(ctx, hostCallbackID(req))

	// 同时给出「注册的模型数」与「缓存的上游清单条数」两个口径，
	// 便于排查「模型列表和上游不一致」这类问题。
	modelCounts := map[string]int{}
	for _, model := range state.Models {
		modelCounts[model.Realm]++
	}

	return map[string]any{
		"ok":                true,
		"plugin":            pluginID,
		"version":           effectivePluginVersion(),
		"enabled_realms":    cfg.EnabledRealms,
		"prompt_mode":       promptModeFor(),
		"log_level":         logger.CurrentLevel(),
		"state_dir":         cfg.StateDir,
		"accounts":          accounts,
		"account_count":     len(accounts),
		"model_counts":      modelCounts,
		"models_fetched_at": state.ModelsFetchedAt,
		"restart_pending":   state.RestartPending,
		"settings":          effectiveSettings(),
		"scheduler":         schedulerStatus(),
	}
}

// accountSummary 是账号概览条目（不含凭证）。
type accountSummary struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Label     string `json:"label"`
	// VendorID 是供应商实例（workbuddycn 等），页面据此分组与展示。
	VendorID string `json:"vendor_id"`
	// VendorName 是供应商展示名（如「Qoder 国内版」）。
	VendorName      string         `json:"vendor_name"`
	Realm           string         `json:"realm"`
	Status          string         `json:"status"`
	Disabled        bool           `json:"disabled"`
	RefreshedAt     string         `json:"refreshed_at,omitempty"`
	Checkin         *checkinRecord `json:"checkin,omitempty"`
	Quota           *quotaResult   `json:"quota,omitempty"`
	SupportsCheckin bool           `json:"supports_checkin"`
}

var (
	quotaCacheMu sync.RWMutex
	quotaCache   = map[string]quotaResult{}
)

func getCachedQuota(keys ...string) *quotaResult {
	quotaCacheMu.RLock()
	defer quotaCacheMu.RUnlock()
	for _, key := range keys {
		if k := strings.TrimSpace(key); k != "" {
			if q, ok := quotaCache[k]; ok {
				copyQ := q
				return &copyQ
			}
		}
	}
	return nil
}

func cacheQuotaResult(q quotaResult, keys ...string) {
	quotaCacheMu.Lock()
	defer quotaCacheMu.Unlock()
	for _, key := range keys {
		if k := strings.TrimSpace(key); k != "" {
			quotaCache[k] = q
		}
	}
}

// listAccountSummaries 列出本插件名下的账号概览。
func listAccountSummaries(ctx context.Context, callbackID string) []accountSummary {
	entries, errList := listHostAuths(ctx, callbackID)
	if errList != nil {
		logger.Debug("list host auths failed: %v", errList)
		return nil
	}
	state := snapshotState()

	out := make([]accountSummary, 0, len(entries))
	seenIndex := make(map[string]int)

	for _, entry := range entries {
		summary := accountSummary{
			AuthID:    firstNonEmptyString(entry.ID, entry.Name),
			AuthIndex: entry.AuthIndex,
			Label:     firstNonEmptyString(entry.Label, entry.Name, entry.ID),
			Status:    entry.Status,
			Disabled:  entry.Disabled,
		}

		var modTime time.Time
		if !entry.ModTime.IsZero() {
			modTime = entry.ModTime
		} else if entry.Path != "" {
			if fi, err := os.Stat(entry.Path); err == nil {
				modTime = fi.ModTime()
			}
		}

		// 归属与区域都从凭证本身解析（交给命中的供应商），
		// 不能拿固定的解析器去解所有凭证——Qoder 的凭证用 WorkBuddy 的
		// 解析器会失败，表现为页面上「凭证解析失败」。
		var accountUID string
		if raw, filePath, okRaw := getAuthJSONAndPathByIndex(ctx, callbackID, entry.AuthIndex); okRaw {
			if modTime.IsZero() && filePath != "" {
				if fi, err := os.Stat(filePath); err == nil {
					modTime = fi.ModTime()
				}
			}
			// 按凭证归属解析；解不出时保留宿主给的 provider 作为兜底展示。
			if credential, okParse := parseVendorCredential(raw, accountIdentity(entry), nil); okParse {
				summary.VendorID = credential.VendorIDValue()
				summary.Realm = credential.RegionValue()
				if vendor, okVendor := core.VendorByID(summary.VendorID); okVendor {
					summary.VendorName = vendor.Name()
					summary.SupportsCheckin = vendor.SupportsCheckin()
				}
				if label := credential.LabelValue(); label != "" {
					summary.Label = label
				}
				if uid := credential.FileIDValue(); uid != "" {
					accountUID = uid
					if record, okRecord := state.Checkin[uid]; okRecord {
						summary.Checkin = &record
					} else if record, okLegacy := lookupLegacyCheckin(credential); okLegacy {
						// 旧记录回退（只读）。见 lookupLegacyCheckin 的说明。
						summary.Checkin = &record
					}
				}
			}
		}
		if modTime.IsZero() && entry.Name != "" {
			for _, dir := range []string{"/root/.cli-proxy-api", "."} {
				candidate := filepath.Join(dir, entry.Name)
				if fi, err := os.Stat(candidate); err == nil {
					modTime = fi.ModTime()
					break
				}
			}
		}
		if !modTime.IsZero() {
			summary.RefreshedAt = modTime.Format(time.RFC3339)
		}
		// 归属解析不出来的账号保留空值：页面会退回显示宿主给的 provider 名，
		// 硬塞一个默认区域会让用户以为它属于某个供应商（实际并没有）。

		if q := getCachedQuota(accountUID, summary.AuthID, summary.AuthIndex, entry.Name, summary.Label); q != nil {
			summary.Quota = q
		}

		// 账号唯一键：优先使用账号 UID，兜底使用规范化后的 AuthID
		dedupKey := accountUID
		if dedupKey == "" {
			dedupKey = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(summary.AuthID), ".json"))
		}

		if idx, exists := seenIndex[dedupKey]; exists && dedupKey != "" {
			// 同账号合并更新，避免在页面重复展示多行
			if out[idx].RefreshedAt == "" && summary.RefreshedAt != "" {
				out[idx].RefreshedAt = summary.RefreshedAt
			}
			if out[idx].Label == "" || out[idx].Label == out[idx].AuthID {
				if summary.Label != "" {
					out[idx].Label = summary.Label
				}
			}
			if out[idx].Checkin == nil && summary.Checkin != nil {
				out[idx].Checkin = summary.Checkin
			}
			continue
		}

		if dedupKey != "" {
			seenIndex[dedupKey] = len(out)
		}
		out = append(out, summary)
	}
	return out
}

// buildLogsPayload 返回插件日志（支持 since 增量拉取）。
func buildLogsPayload(req pluginapi.ManagementRequest) map[string]any {
	since := queryInt64(req.Query, "since", 0)
	limit := int(queryInt64(req.Query, "limit", 200))
	entries, newest := logger.Snapshot(uint64(since), limit)
	return map[string]any{
		"ok":      true,
		"entries": entries,
		"next":    newest,
	}
}

// queryInt64 从查询参数取整数。
func queryInt64(query map[string][]string, key string, fallback int64) int64 {
	if query == nil {
		return fallback
	}
	values, okValues := query[key]
	if !okValues || len(values) == 0 {
		return fallback
	}
	parsed, errParse := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
	if errParse != nil {
		return fallback
	}
	return parsed
}

// handleCheckinRequest 对指定或全部账号执行签到。
type accountBatchRequest struct {
	AccountIDs []string `json:"account_ids"`
}

// accountDeleteRequest 是删除账号的请求体。
type accountDeleteRequest struct {
	AccountIDs []string `json:"account_ids"`
}

// handleAccountDeleteRequest 删除指定账号的凭证文件。
//
// **为什么是删文件而不是调宿主接口**：宿主在 ABI 里只暴露了 auth.list /
// auth.get / auth.save，没有删除方法；插件在进程内也拿不到管理密钥，
// 无法走宿主的 DELETE /v0/management/auth-files。
//
// 但宿主用 fsnotify 监听 auth 目录并处理 Remove/Rename
// （internal/watcher/events.go 的 handleEvent → removeClientLocked），
// 因此**删掉文件后宿主会自己注销该账号**——不需要额外通知。
//
// 安全约束：只删「宿主确认属于本插件的凭证文件」，且路径必须落在 auth 目录内。
// 宿主给的 Path 理论上是可信的，但落盘路径绝不能依赖上游数据的"善意"，
// 一次越界删除就可能是用户的任意文件。
func handleAccountDeleteRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountDeleteRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	if len(body.AccountIDs) == 0 {
		return jsonResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "account_ids 不能为空"})
	}

	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	entries, errList := listHostAuths(ctx, callbackID)
	if errList != nil {
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": false, "error": "读取账号列表失败：" + errList.Error(),
		})
	}
	wanted := make(map[string]bool, len(body.AccountIDs))
	for _, target := range body.AccountIDs {
		wanted[strings.TrimSpace(target)] = true
	}

	results := make([]accountActionResult, 0, len(body.AccountIDs))
	deleted := 0
	for _, entry := range entries {
		if !wanted[entry.ID] && !wanted[entry.Name] && !wanted[entry.AuthIndex] {
			continue
		}
		result := accountActionResult{
			AuthID: firstNonEmptyString(entry.ID, entry.Name),
			Label:  firstNonEmptyString(entry.Label, entry.Name, entry.ID),
		}
		if errDelete := deleteAuthFile(ctx, callbackID, entry); errDelete != nil {
			result.OK = false
			result.Message = errDelete.Error()
			results = append(results, result)
			continue
		}
		result.OK = true
		result.Message = "已删除"
		deleted++
		results = append(results, result)
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "deleted": deleted, "results": results,
	})
}

// deleteAuthFile 删除一个账号的凭证文件。
func deleteAuthFile(ctx context.Context, callbackID string, entry hostAuthEntry) error {
	// 先解析凭证，确认它确实归本插件——避免误删别家插件的凭证文件。
	raw, filePath, okRaw := getAuthJSONAndPathByIndex(ctx, callbackID, entry.AuthIndex)
	if !okRaw {
		return fmt.Errorf("读取凭证失败，无法确认归属")
	}
	if _, okParse := parseVendorCredential(raw, accountIdentity(entry), nil); !okParse {
		return fmt.Errorf("该凭证不属于本插件，拒绝删除")
	}

	targetPath := strings.TrimSpace(filePath)
	if targetPath == "" {
		targetPath = strings.TrimSpace(entry.Path)
	}
	if targetPath == "" {
		// 运行时凭证（无物理文件）删不掉：如实告知，而不是假装成功。
		return fmt.Errorf("该账号没有对应的凭证文件（运行时凭证），无法删除")
	}
	if errSafe := ensureAuthFilePathSafe(targetPath); errSafe != nil {
		return errSafe
	}
	if errRemove := os.Remove(targetPath); errRemove != nil {
		if os.IsNotExist(errRemove) {
			return nil
		}
		return fmt.Errorf("删除文件失败：%w", errRemove)
	}
	logger.Info("deleted auth file %s (vendor=%s)", filepath.Base(targetPath), entry.Name)
	return nil
}

// ensureAuthFilePathSafe 校验待删路径是一个 .json 凭证文件。
//
// 三重约束，任何一条不满足都拒绝：
//  1. 必须是绝对路径（相对路径会相对于进程工作目录，落在哪儿不可控）；
//  2. 扩展名必须是 .json（凭证文件都是 JSON，别的文件一律不碰）；
//  3. 路径必须已存在且是普通文件（目录、符号链接一律拒绝——符号链接会让
//     os.Remove 删掉链接本身，但更糟的是它可能指向 auth 目录之外）。
func ensureAuthFilePathSafe(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("拒绝删除非绝对路径：%s", path)
	}
	if !strings.HasSuffix(strings.ToLower(path), ".json") {
		return fmt.Errorf("拒绝删除非凭证文件：%s", filepath.Base(path))
	}
	info, errStat := os.Lstat(path)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			return nil
		}
		return fmt.Errorf("检查文件失败：%w", errStat)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("拒绝删除符号链接：%s", filepath.Base(path))
	}
	if info.IsDir() {
		return fmt.Errorf("拒绝删除目录：%s", filepath.Base(path))
	}
	return nil
}

// accountAddRequest 是添加 API Key 类账号的请求体。
type accountAddRequest struct {
	Vendor string `json:"vendor"`
	APIKey string `json:"api_key"`
	Label  string `json:"label"`
}

// handleAccountAddRequest 添加一个 API Key 凭证并落盘。
func handleAccountAddRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountAddRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	vendorID := strings.TrimSpace(body.Vendor)
	apiKey := strings.TrimSpace(body.APIKey)
	label := strings.TrimSpace(body.Label)

	if vendorID == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "vendor 不能为空"})
	}
	if apiKey == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "api_key 不能为空"})
	}
	vendor, okVendor := core.VendorByID(vendorID)
	if !okVendor {
		return jsonResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("未知供应商 %q", vendorID)})
	}

	payload := map[string]any{
		"type":         providerKey,
		core.VendorKey: vendor.ID(),
		"api_key":      apiKey,
	}
	if label != "" {
		payload["label"] = label
	}

	rawJSON, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": errMarshal.Error()})
	}

	seed := label
	if seed == "" {
		seed = opencodezen.MaskedKey(apiKey)
	}
	if seed == "" {
		seed = newHexID(8)
	}
	fileName := core.FileNameFor(vendor.ID(), seed)

	_, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	saved := false
	if _, errSave := callHostScoped(callbackID, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: fileName,
		JSON: rawJSON,
	}); errSave == nil {
		saved = true
	} else {
		logger.Debug("host.auth.save failed for %s: %v", fileName, errSave)
	}

	// 磁盘直写兜底
	for _, dir := range []string{"/root/.cli-proxy-api", "."} {
		dst := filepath.Join(dir, fileName)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			if errWrite := os.WriteFile(dst, rawJSON, 0o600); errWrite == nil {
				saved = true
				break
			}
		}
	}

	if !saved {
		return jsonResponse(http.StatusInternalServerError, map[string]any{
			"ok": false, "error": "保存凭证文件失败",
		})
	}

	logger.Info("added auth file %s for vendor %s", fileName, vendor.ID())
	return jsonResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"auth_id": fileName,
		"message": "账号保存成功",
	})
}

func handleCheckinRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountBatchRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	ctx, cancel := managementContext(req)
	defer cancel()

	results := runForAccounts(ctx, hostCallbackID(req), body.AccountIDs,
		func(vendor core.Vendor, credential *core.Credential) bool {
			return vendor.SupportsCheckin()
		},
		func(ctx context.Context, vendor core.Vendor, credential *core.Credential) (string, error) {
			result, errCheckin := vendor.Checkin(ctx, credential)
			if errCheckin != nil {
				return "", errCheckin
			}
			if result == nil {
				return "", fmt.Errorf("签到未返回结果")
			}
			recordCheckinResult(credential, result)
			return result.Message, nil
		})
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
}

// recordCheckinResult 把签到结果写入状态。
// lookupLegacyCheckin 读取「账号身份迁移前的 UID 键」下的签到记录。
//
// 账号身份从 UID 改为文件名（core.Credential.FileID）之前，签到记录是按上游
// UID 落的键——WorkBuddy 的键是纯 UUID（79fdc1fc-…），与文件名不同。迁移后
// 若只按新键查，存量账号会突然显示「未签到」，直到下一次签到才补上新键。
//
// 这是**只读回退**：不写旧键、不迁移数据，仅让存量的「今天已签到」继续可见。
// 代价是旧键会一直留在 state.json 里（每条几十字节），换取的是不需要一次性
// 数据迁移脚本、也不会因迁移失败丢记录。
func lookupLegacyCheckin(credential *core.Credential) (checkinRecord, bool) {
	legacyUID := credential.UIDValue()
	if legacyUID == "" || legacyUID == credential.FileIDValue() {
		return checkinRecord{}, false
	}
	state := snapshotState()
	record, okRecord := state.Checkin[legacyUID]
	return record, okRecord
}

// recordCheckinResult 把一次签到结果写进状态（供页面展示）。
//
// 接收中立的 core.CheckinResult：各家的签到结果字段不同（WorkBuddy 有积分+
// 能量，Qoder 只有 credits），状态里只记两家都有的部分。
func recordCheckinResult(credential *core.Credential, result *core.CheckinResult) {
	if credential == nil || result == nil {
		return
	}
	uid := credential.FileIDValue()
	if uid == "" {
		return
	}
	today := todayString()
	mutateState(func(state *pluginState) {
		record := state.Checkin[uid]
		record.LastDate = today
		if result.Credit > 0 {
			record.Credit = result.Credit
		}
		if result.Energy > 0 {
			record.Energy = result.Energy
		}
		state.Checkin[uid] = record
	})
}

// quotaResult 是单个账号的额度结果。
//
// 刻意返回**结构化数据**而不是一句文字：控制台页要把额度并入账号表，
// 需要 remain/total/packages 这些字段；只回 message 的话页面无从渲染。
type quotaResult struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index,omitempty"`
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
	Label     string `json:"label"`
	// VendorID / VendorName 让页面把额度归到正确的供应商分组下。
	VendorID   string `json:"vendor_id,omitempty"`
	VendorName string `json:"vendor_name,omitempty"`
	Realm      string `json:"realm"`
	OK         bool   `json:"ok"`
	Message    string `json:"message,omitempty"`
	// Summary 是额度摘要（各家的额度模型不同，用统一的可读文字表达）。
	Summary string `json:"summary,omitempty"`
	// Remain / Total 是归一化的「剩余 / 总额」两个数字，供页面的
	// 「剩余/总额 + 使用率进度条」渲染。各家的额度口径不同，因此在适配层
	// 统一折算到这里；取不到时两者都留 0，页面显示 "—"。
	Remain int64 `json:"remain"`
	Total  int64 `json:"total"`
	// Metrics 是原始关键指标（宿主的 QuotaMetric 形态），
	// 供需要看细分的场景使用。
	Metrics []pluginapi.QuotaMetric `json:"metrics,omitempty"`
}

// handleQuotasRequest 批量查询额度。
func handleQuotasRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body accountBatchRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	entries, errList := listHostAuths(ctx, callbackID)
	if errList != nil {
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": false, "results": []any{},
			"error": "读取账号列表失败：" + errList.Error(),
		})
	}
	wanted := make(map[string]bool, len(body.AccountIDs))
	for _, target := range body.AccountIDs {
		wanted[strings.TrimSpace(target)] = true
	}

	type job struct {
		entry      hostAuthEntry
		vendor     core.Vendor
		credential *core.Credential
	}
	var preResults []quotaResult
	jobs := make([]job, 0, len(entries))
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		if len(wanted) > 0 && !wanted[entry.ID] && !wanted[entry.Name] && !wanted[entry.AuthIndex] {
			continue
		}
		var raw []byte
		if entry.AuthIndex != "" {
			if r, ok := getAuthJSONByIndex(ctx, callbackID, entry.AuthIndex); ok {
				raw = r
			}
		}
		if len(raw) == 0 && entry.Path != "" {
			if r, err := os.ReadFile(entry.Path); err == nil && len(r) > 0 {
				raw = r
			}
		}
		if len(raw) == 0 && entry.Name != "" {
			for _, dir := range []string{"/root/.cli-proxy-api", "./auths", "."} {
				candidate := filepath.Join(dir, entry.Name)
				if r, err := os.ReadFile(candidate); err == nil && len(r) > 0 {
					raw = r
					break
				}
			}
		}
		if len(raw) == 0 {
			targetID := firstNonEmptyString(entry.AuthIndex, entry.ID, entry.Name)
			if r, err := fetchAuthJSON(ctx, callbackID, targetID); err == nil && len(r) > 0 {
				raw = r
			}
		}
		if len(raw) == 0 {
			preResults = append(preResults, quotaResult{
				AuthID:  firstNonEmptyString(entry.ID, entry.Name),
				Label:   firstNonEmptyString(entry.Label, entry.Name, entry.ID),
				OK:      false,
				Message: "凭证读取失败",
			})
			continue
		}
		// 按凭证归属分发解析：拿固定解析器解所有凭证会让另一家的凭证
		// 报「凭证解析失败」（例如 Qoder 的凭证用 WorkBuddy 的结构去解）。
		credential, vendor, errResolve := resolveVendorCredential(ctx, callbackID, raw, accountIdentity(entry), nil)
		if errResolve != nil {
			preResults = append(preResults, quotaResult{
				AuthID:  firstNonEmptyString(entry.ID, entry.Name),
				Label:   firstNonEmptyString(entry.Label, entry.Name, entry.ID),
				OK:      false,
				Message: "凭证解析失败: " + errResolve.Error(),
			})
			continue
		}
		jobs = append(jobs, job{entry: entry, vendor: vendor, credential: credential})
	}

	jobResults := make([]quotaResult, len(jobs))
	semaphore := make(chan struct{}, managementBatchConcurrency)
	done := make(chan int, len(jobs))
	for index := range jobs {
		go func(slot int) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			current := jobs[slot]
			result := quotaResult{
				AuthID:    firstNonEmptyString(current.entry.ID, current.entry.Name),
				AuthIndex: current.entry.AuthIndex,
				Name:      current.entry.Name,
				UID:       current.credential.FileIDValue(),
				Label:     firstNonEmptyString(current.credential.LabelValue(), current.entry.Label, current.entry.Name),
				VendorID:  current.vendor.ID(),
				Realm:     current.credential.RegionValue(),
			}
			quota, errQuota := current.vendor.Quota(ctx, current.credential)
			if errQuota != nil {
				result.Message = errQuota.Error()
			} else {
				result.OK = true
				result.Summary = summarizeQuotaMetrics(quota)
				result.Remain, result.Total = normalizeQuotaNumbers(quota)
				result.Metrics = quota.Summary
				cacheQuotaResult(result, result.UID, result.AuthID, result.AuthIndex, result.Name, result.Label)
			}
			jobResults[slot] = result
			done <- slot
		}(index)
	}
	for range jobs {
		<-done
	}
	finalResults := append(preResults, jobResults...)
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "results": finalResults})
}

// summarizeQuotaMetrics 把归一的额度响应压成一行文字（供账号表展示）。
//
// 优先用 Summary 里的指标（那是各家自己定义的关键数字），没有时退回分组明细。
func summarizeQuotaMetrics(quota *pluginapi.QuotaFetchResponse) string {
	if quota == nil {
		return ""
	}
	parts := make([]string, 0, len(quota.Summary)+2)
	for _, metric := range quota.Summary {
		label := strings.TrimSpace(metric.Label)
		if label == "" {
			label = metric.Key
		}
		parts = append(parts, fmt.Sprintf("%s %s", label, formatMetricValue(metric.Value)))
	}
	for _, group := range quota.Groups {
		for _, bucket := range group.Buckets {
			label := strings.TrimSpace(group.DisplayName)
			if label == "" {
				label = bucket.Window
			}
			parts = append(parts, fmt.Sprintf("%s %s", label, bucket.Description))
		}
	}
	return strings.Join(parts, " · ")
}

// formatMetricValue 把指标数值格式化成可读文本。
//
// 额度值都是整数或一位小数，用 %g 会输出 1.5e+06 这种科学计数法，
// 在表格里很难看。
func formatMetricValue(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// accountActionResult 是单账号操作的执行结果。
type accountActionResult struct {
	AuthID string `json:"auth_id"`
	Label  string `json:"label"`
	// VendorID 让页面把结果归到正确的供应商分组下。
	VendorID string `json:"vendor_id,omitempty"`
	Realm    string `json:"realm"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

// runForAccounts 对指定账号（或全部账号）并发执行一个动作。
//
// 并发度有上限：任务类操作会在上游留下行为记录，全账号瞬间并发容易被风控注意到。
func runForAccounts(
	ctx context.Context,
	callbackID string,
	targets []string,
	filter func(core.Vendor, *core.Credential) bool,
	action func(context.Context, core.Vendor, *core.Credential) (string, error),
) []accountActionResult {
	entries, errList := listHostAuths(ctx, callbackID)
	if errList != nil {
		return []accountActionResult{{OK: false, Message: "读取账号列表失败：" + errList.Error()}}
	}
	wanted := make(map[string]bool, len(targets))
	for _, target := range targets {
		wanted[strings.TrimSpace(target)] = true
	}

	cfg := loadedConfig()
	type job struct {
		entry      hostAuthEntry
		vendor     core.Vendor
		credential *core.Credential
	}
	jobs := make([]job, 0, len(entries))
	seenUID := make(map[string]bool)
	for _, entry := range entries {
		if len(wanted) > 0 {
			if !wanted[entry.ID] && !wanted[entry.Name] && !wanted[entry.AuthIndex] {
				continue
			}
		}
		if entry.Disabled || entry.Unavailable {
			continue
		}
		raw, okRaw := getAuthJSONByIndex(ctx, callbackID, entry.AuthIndex)
		if !okRaw {
			continue
		}
		// 按归属分发：拿固定解析器解所有凭证会让另一家的凭证被静默跳过。
		credential, vendor, errResolve := resolveVendorCredential(ctx, callbackID, raw, accountIdentity(entry), nil)
		if errResolve != nil {
			continue
		}
		if !realmEnabled(cfg, vendor.Region()) {
			continue
		}
		if filter != nil && !filter(vendor, credential) {
			continue
		}
		uid := credential.FileIDValue()
		if uid == "" {
			uid = firstNonEmptyString(entry.ID, entry.Name)
		}
		if uid != "" {
			if seenUID[uid] {
				continue
			}
			seenUID[uid] = true
		}
		jobs = append(jobs, job{entry: entry, vendor: vendor, credential: credential})
	}

	results := make([]accountActionResult, len(jobs))
	concurrency := managementBatchConcurrency
	if concurrency > len(jobs) {
		concurrency = len(jobs)
	}
	if concurrency < 1 {
		concurrency = 1
	}
	semaphore := make(chan struct{}, concurrency)
	done := make(chan int, len(jobs))

	for index := range jobs {
		go func(slot int) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			current := jobs[slot]
			result := accountActionResult{
				AuthID:   firstNonEmptyString(current.entry.ID, current.entry.Name),
				Label:    firstNonEmptyString(current.credential.LabelValue(), current.entry.Label),
				VendorID: current.vendor.ID(),
				Realm:    current.credential.RegionValue(),
			}
			message, errAction := action(ctx, current.vendor, current.credential)
			if errAction != nil {
				result.OK = false
				result.Message = errAction.Error()
			} else {
				result.OK = true
				result.Message = message
			}
			results[slot] = result
			done <- slot
		}(index)
	}
	for range jobs {
		<-done
	}
	return results
}

// handleSettingsRequest 更新插件运行期设置。
type settingsUpdateRequest struct {
	AutoCheckin    *bool  `json:"auto_checkin"`
	AutoCheckinAt  string `json:"auto_checkin_at"`
	AutoTasks      *bool  `json:"auto_tasks"`
	PromptMode     string `json:"prompt_mode"`
	CheckinHours   []int  `json:"checkin_hours"`
	TravelHours    []int  `json:"travel_hours"`
	ActivityHours  []int  `json:"activity_hours"`
	KeepaliveHours []int  `json:"keepalive_hours"`
	BlackcatHours  []int  `json:"blackcat_hours"`
}

func handleSettingsRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body settingsUpdateRequest
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	changed := map[string]any{}

	mutateState(func(state *pluginState) {
		if body.AutoCheckin != nil {
			state.AutoCheckin = body.AutoCheckin
			changed["auto_checkin"] = *body.AutoCheckin
		}
		if at := strings.TrimSpace(body.AutoCheckinAt); at != "" {
			if _, _, errClock := parseClock(at); errClock == nil {
				state.AutoCheckinAt = at
				changed["auto_checkin_at"] = at
			}
		}
		if body.AutoTasks != nil {
			state.AutoTasks = body.AutoTasks
			changed["auto_tasks"] = *body.AutoTasks
		}
		if mode := strings.ToLower(strings.TrimSpace(body.PromptMode)); mode != "" {
			switch mode {
			case "passthrough", "custom", "append":
				state.PromptMode = &mode
				changed["prompt_mode"] = mode
			}
		}
		for key, pair := range map[string]struct {
			hours []int
			dest  *[]int
		}{
			"checkin_hours":   {body.CheckinHours, &state.CheckinHours},
			"travel_hours":    {body.TravelHours, &state.TravelHours},
			"activity_hours":  {body.ActivityHours, &state.ActivityHours},
			"keepalive_hours": {body.KeepaliveHours, &state.KeepaliveHours},
			"blackcat_hours":  {body.BlackcatHours, &state.BlackcatHours},
		} {
			if len(pair.hours) > 0 {
				*pair.dest = pair.hours
				changed[key] = pair.hours
			}
		}
	})

	if len(changed) == 0 {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "没有可更新的字段",
		})
	}
	// 排程变化要唤醒调度循环重新计算下次执行时刻。
	pokeScheduler()
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": changed, "settings": effectiveSettings(),
	})
}

// handleScheduledRun 手动触发一个定时任务。
func handleScheduledRun(req pluginapi.ManagementRequest, kind string) any {
	go runScheduledTask(kind)
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "started": true, "task": kind,
		"message": "任务已在后台开始执行，进度见插件日志",
	})
}

// keepaliveResult 是单账号 Token 续期的结果。
type keepaliveResult struct {
	AuthID string `json:"auth_id"`
	Label  string `json:"label"`
	// VendorID 让页面把结果归到正确的供应商分组下。
	VendorID string `json:"vendor_id,omitempty"`
	Realm    string `json:"realm"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

// handleKeepaliveRequest 手动执行全账号 Token 续期保活。
func handleKeepaliveRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	callbackID := hostCallbackID(req)

	entries, errList := listHostAuths(ctx, callbackID)
	if errList != nil {
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": false, "error": "读取账号列表失败：" + errList.Error(),
		})
	}

	type job struct {
		entry      hostAuthEntry
		vendor     core.Vendor
		credential *core.Credential
		rawJSON    []byte
		filePath   string
	}
	jobs := make([]job, 0, len(entries))
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		raw, filePath, okRaw := getAuthJSONAndPathByIndex(ctx, callbackID, entry.AuthIndex)
		if !okRaw {
			continue
		}
		// 按归属分发：续期链路各家不同（WorkBuddy 换 token，Qoder jobToken 交换）。
		credential, vendor, errResolve := resolveVendorCredential(ctx, callbackID, raw, accountIdentity(entry), nil)
		if errResolve != nil {
			continue
		}
		if filePath != "" {
			credential.SetFilePath(filePath)
		}
		jobs = append(jobs, job{entry: entry, vendor: vendor, credential: credential, rawJSON: raw, filePath: filePath})
	}

	if len(jobs) == 0 {
		return jsonResponse(http.StatusOK, map[string]any{
			"ok": true, "message": "没有需要续期的可用账号", "succeeded": 0, "failed": 0, "results": []any{},
		})
	}

	results := make([]keepaliveResult, len(jobs))
	var (
		succeededCount int
		failedCount    int
		firstErrMsg    string
	)
	for i, j := range jobs {
		label := firstNonEmptyString(j.credential.LabelValue(), j.entry.Label, j.entry.Name)
		res := keepaliveResult{
			AuthID:   firstNonEmptyString(j.entry.ID, j.entry.Name),
			Label:    label,
			VendorID: j.vendor.ID(),
			Realm:    j.credential.RegionValue(),
		}

		updated, refreshed, errRefresh := j.vendor.Refresh(ctx, j.credential)
		if errRefresh != nil {
			failedCount++
			res.OK = false
			res.Message = errRefresh.Error()
			if firstErrMsg == "" {
				firstErrMsg = fmt.Sprintf("%s: %s", label, errRefresh.Error())
			}
			logger.Error("keepalive %s: %v", label, errRefresh)
		} else {
			succeededCount++
			res.OK = true
			if refreshed {
				res.Message = "Token 续期成功"
				saveRefreshedCredential(ctx, callbackID, j.vendor, j.entry, j.rawJSON, updated, j.filePath)
			} else {
				// 上游没下发新令牌：不写盘（避免 mtime 无意义变动）。
				res.Message = "凭证有效（未变更）"
			}
			logger.Info("keepalive %s: refreshed=%t", label, refreshed)
		}
		results[i] = res
	}

	var message string
	if failedCount > 0 && succeededCount == 0 {
		message = fmt.Sprintf("全部账号续期失败（%s）", firstErrMsg)
	} else if failedCount > 0 {
		message = fmt.Sprintf("续期完成：%d 个成功，%d 个失败（%s）", succeededCount, failedCount, firstErrMsg)
	} else {
		message = fmt.Sprintf("全账号 Token 续期成功（共 %d 个账号已更新）", succeededCount)
	}

	return jsonResponse(http.StatusOK, map[string]any{
		"ok":        failedCount == 0,
		"total":     len(jobs),
		"succeeded": succeededCount,
		"failed":    failedCount,
		"message":   message,
		"results":   results,
	})
}

// handleSchoolStatus 返回开学季活动状态。
func handleSchoolStatus(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	statuses := schoolStatusForAccounts(ctx, hostCallbackID(req))
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "accounts": statuses})
}

// vendorDescriptor 是供应商的展示描述（供页面下拉与分组用）。
type vendorDescriptor struct {
	// ID 是供应商实例标识（workbuddycn 等），页面用它发起登录与筛选。
	ID string `json:"id"`
	// Name 是展示名（如「WorkBuddy 国内版」）。
	Name string `json:"name"`
	// Region 是区域标识（cn / global；无区域供应商为空串）。
	Region string `json:"region"`
	// SupportsCheckin 报告该供应商是否提供签到（页面据此隐藏入口）。
	SupportsCheckin bool `json:"supports_checkin"`
	// AuthMode 是凭证形态（apikey / oauth）：页面据此决定账号列是否脱敏。
	AuthMode string `json:"auth_mode"`
	// SupportsLogin 报告该供应商是否提供登录流程。
	//
	// 页面据此决定「添加账号」下拉里该项是发起登录还是提示手填凭证——
	// OpenCode ZEN 是纯 API key，点它发起登录只会拿到一个「不支持」错误。
	SupportsLogin bool `json:"supports_login"`
}

// buildVendorsPayload 返回已启用的供应商清单。
//
// 页面据此渲染「添加账号」下拉与账号表分组，因此新增供应商不需要改 HTML。
func buildVendorsPayload() map[string]any {
	cfg := loadedConfig()
	vendors := make([]vendorDescriptor, 0, len(core.Vendors()))
	for _, vendor := range core.Vendors() {
		if !realmEnabled(cfg, vendor.Region()) {
			continue
		}
		vendors = append(vendors, vendorDescriptor{
			ID:              vendor.ID(),
			Name:            vendor.Name(),
			Region:          vendor.Region(),
			SupportsCheckin: vendor.SupportsCheckin(),
			AuthMode:        vendorAuthMode(vendor),
			SupportsLogin:   vendorSupportsLogin(vendor),
		})
	}
	return map[string]any{"ok": true, "vendors": vendors}
}

// vendorAuthMode 返回供应商的凭证形态（未声明时按 oauth 处理）。
func vendorAuthMode(vendor core.Vendor) string {
	if reporter, okReporter := vendor.(core.AuthModeReporter); okReporter {
		if mode := strings.TrimSpace(reporter.AuthMode()); mode != "" {
			return mode
		}
	}
	return "oauth"
}

// vendorSupportsLogin 报告供应商是否提供登录流程。
//
// 声明式：见 core.LoginSupport 的说明。未声明的按「支持」处理——
// 绝大多数供应商都有登录，只有 API key 型（OpenCode ZEN）没有。
func vendorSupportsLogin(vendor core.Vendor) bool {
	if support, okSupport := vendor.(core.LoginSupport); okSupport {
		return support.SupportsLogin()
	}
	return true
}

// registeredModelsForVendor 返回某个供应商当前注册给宿主的模型清单。
//
// 数据来自插件自己的模型目录（与 model.static 同源），因此不会额外打上游——
// 管理页的「读取模型清单」应当是轻量操作。
func registeredModelsForVendor(vendorID string) []pluginapi.ModelInfo {
	return cachedStaticModels()[vendorID]
}

// staticModelsCache 缓存最近一次 staticModels 的结果，供管理页回读。
//
// 为什么不直接调 vendor.StaticModels：那会打上游（Qoder 需要建 cosy 会话），
// 而管理页刷新是高频操作。模型注册路径已经拉过一次，这里复用它的结果。
var (
	staticModelsMu    sync.RWMutex
	staticModelsCache = map[string][]pluginapi.ModelInfo{}
)

// cacheStaticModels 记录某个供应商的模型清单（由注册路径调用）。
func cacheStaticModels(vendorID string, models []pluginapi.ModelInfo) {
	staticModelsMu.Lock()
	defer staticModelsMu.Unlock()
	staticModelsCache[vendorID] = models
}

// cachedStaticModels 返回缓存的模型清单副本。
func cachedStaticModels() map[string][]pluginapi.ModelInfo {
	staticModelsMu.RLock()
	defer staticModelsMu.RUnlock()
	out := make(map[string][]pluginapi.ModelInfo, len(staticModelsCache))
	for vendorID, models := range staticModelsCache {
		copied := make([]pluginapi.ModelInfo, len(models))
		copy(copied, models)
		out[vendorID] = copied
	}
	return out
}

// saveRefreshedCredential 把续期后的凭证写回宿主与磁盘。
//
// 写两处是刻意的双保险：
//   - host.auth.save 让宿主热更新内存注册表（否则内存态与磁盘态不一致，
//     表现为「明明续期了却还报凭证失效」）；
//   - 直接写文件兜住 host.auth.save 不可用的场景（如宿主未提供该能力）。
func saveRefreshedCredential(ctx context.Context, callbackID string, vendor core.Vendor,
	entry hostAuthEntry, original []byte, updated *core.Credential, filePath string) {
	payload, okPayload := refreshedStorageJSON(vendor, original, updated)
	if okPayload {
		if _, errSave := callHostScoped(callbackID, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
			Name: entry.Name,
			JSON: payload,
		}); errSave != nil {
			logger.Debug("keepalive %s: host.auth.save failed: %v", entry.Name, errSave)
		}
	}

	targetPath := filePath
	if targetPath == "" {
		targetPath = entry.Path
	}
	if targetPath == "" {
		return
	}
	if errWrite := os.WriteFile(targetPath, payload, 0o600); errWrite != nil {
		logger.Debug("keepalive %s: write credential file failed: %v", entry.Name, errWrite)
	}
}

// refreshedStorageJSON 用供应商自己的合并逻辑更新凭证 JSON。
//
// 各家结构不同（WorkBuddy 是嵌套的 auth/account，Qoder 与 Cline 是扁平字段），
// 通用合并会把顶层字段写乱，因此合并交给供应商自己实现（core.StorageMerger）。
//
// 未实现该接口的供应商返回 ok=false，调用方据此跳过写盘——不写比写坏好。
func refreshedStorageJSON(vendor core.Vendor, original []byte, updated *core.Credential) ([]byte, bool) {
	merger, okMerger := vendor.(core.StorageMerger)
	if !okMerger {
		return original, false
	}
	merged, errMerge := merger.MergeStorageJSON(original, updated)
	if errMerge != nil {
		return original, false
	}
	return merged, true
}

// normalizeQuotaNumbers 从归一的额度响应里折算「剩余 / 总额」两个数字。
//
// 各家的额度口径不同（WorkBuddy 是积分余额，Qoder 是套餐用量桶且分
// userQuota / addOnQuota 两个桶），因此统一在适配层折算，页面不必理解
// 任何一家指标键的语义。
//
// 折算规则：优先取「剩余」语义的指标与「总额」语义的指标；都取不到时返回 0,0
// （页面显示 "—"，这是诚实的结果，好过显示一个编出来的数）。
func normalizeQuotaNumbers(quota *pluginapi.QuotaFetchResponse) (remain, total int64) {
	if quota == nil {
		return 0, 0
	}
	// 按已知的指标键收集。新增供应商时在这里补它的键即可——
	// 比让页面按语义猜稳定得多。
	for _, metric := range quota.Summary {
		switch metric.Key {
		case "credit_remain", "user_quota_remaining", "addon_quota_remaining", "remaining":
			if remain == 0 {
				remain = int64(metric.Value)
			}
		case "credit_total", "user_quota_total", "addon_quota_total", "total":
			if total == 0 {
				total = int64(metric.Value)
			}
		}
	}
	if total == 0 {
		// 没有「总额」指标时，用剩余 + 已用凑一个总额，让进度条有意义。
		var used int64
		for _, metric := range quota.Summary {
			switch metric.Key {
			case "credit_used", "user_quota_used", "addon_quota_used", "used":
				used += int64(metric.Value)
			}
		}
		if used > 0 {
			total = remain + used
		}
	}
	return remain, total
}
