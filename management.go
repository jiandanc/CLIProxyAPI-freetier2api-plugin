package main

// 本文件实现 management 能力：插件自有管理接口与内嵌控制台页。
//
// 安全边界（重要）：
//   - /v0/resource/plugins/workbuddy2api/console 只返回**静态页面**，不含任何账号/凭证数据；
//   - 所有读取与动作都走 /v0/management/plugins/workbuddy2api/...，由 CPA 的管理鉴权保护；
//   - 接口返回里**永远不含 token**（只回传账号名、域、状态、额度与任务结果）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api-plugin/cpasdk/pluginapi"
	"workbuddy2api-plugin/internal/cb"
	"workbuddy2api-plugin/internal/httpx"
	"workbuddy2api-plugin/internal/logger"
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
			{Method: "POST", Path: managementRoutePrefix + "/checkin", Description: "对指定或全部账号执行签到。"},
			{Method: "POST", Path: managementRoutePrefix + "/quotas", Description: "批量查询账号额度。"},
			{Method: "GET", Path: managementRoutePrefix + "/models", Description: "读取当前注册的模型清单。"},
			{Method: "POST", Path: managementRoutePrefix + "/models/toggle", Description: "批量禁用/启用模型。"},
			{Method: "POST", Path: managementRoutePrefix + "/models/refresh", Description: "从上游拉取模型清单并缓存。"},
			{Method: "GET", Path: managementRoutePrefix + "/logs", Description: "读取插件日志（增量）。"},
			{Method: "POST", Path: managementRoutePrefix + "/settings", Description: "更新插件运行期设置（自动签到、任务排程、提示词）。"},
			{Method: "POST", Path: managementRoutePrefix + "/tasks/scan", Description: "扫描全部账号的待办任务。"},
			{Method: "POST", Path: managementRoutePrefix + "/tasks/run", Description: "执行任务队列（账号内串行、账号间并发）。"},
			{Method: "GET", Path: managementRoutePrefix + "/tasks/queue", Description: "查询任务队列进度。"},
			{Method: "POST", Path: managementRoutePrefix + "/tasks/auto", Description: "对单个账号执行单个任务或一键完成。"},
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
				Description: "WorkBuddy 账号、任务与额度管理页。",
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

	case method == http.MethodGet && matchesManagementPath(rpc.Path, "/logs"):
		return okEnvelope(jsonResponse(http.StatusOK, buildLogsPayload(rpc)))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/checkin"):
		return okEnvelope(handleCheckinRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/quotas"):
		return okEnvelope(handleQuotasRequest(rpc))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, "/models"):
		return okEnvelope(handleModelsList(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/models/toggle"):
		return okEnvelope(handleModelsToggle(rpc))

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
		return okEnvelope(handleScheduledRun(rpc, "keepalive"))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, "/blackcat"):
		return okEnvelope(handleScheduledRun(rpc, "blackcat"))
	}

	return okEnvelope(jsonResponse(http.StatusNotFound, map[string]any{
		"error": "not found", "path": rpc.Path, "method": method,
	}))
}

// matchesManagementPath 判断路径是否命中某个管理子路径。
//
// 容忍宿主传全路径（/v0/management/plugins/workbuddy2api/x）或子路径（/plugins/workbuddy2api/x）。
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

// managementBody 解析管理请求的 JSON body（空 body 返回空 map）。
func managementBody(req pluginapi.ManagementRequest) map[string]any {
	if len(req.Body) == 0 {
		return map[string]any{}
	}
	payload := decodeJSONMap(req.Body)
	if payload == nil {
		return map[string]any{}
	}
	return payload
}

// consoleModel 是控制台页展示用的模型条目。
//
// 刻意**自定义**而不直接回传 pluginapi.ModelInfo：宿主对插件的管理响应是原样
// 透传的，而 pluginapi.ModelInfo **没有 json tag**，字段名会变成 Go 字段名
// （ID / ContextLength / DisplayName）。页面按小写键读就会全部拿到 undefined，
// 表现成一屏的 "--"。这里显式声明契约，两端一致。
type consoleModel struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
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
	cfg := loadedConfig()
	cached := cb.CachedModels()

	models := make([]consoleModel, 0, 64)
	for _, region := range []cb.Region{cb.RegionCN, cb.RegionGlobal} {
		if !realmEnabled(cfg, string(region)) {
			continue
		}
		for _, model := range cached[region] {
			models = append(models, consoleModel{
				ID:              PrefixModelID(region, model.ID),
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
	}
	// 额外注册的模型（配置里的 extra_models）也一并展示。
	for _, extra := range extraModels(cfg) {
		models = append(models, consoleModel{
			ID:            extra.ID,
			Name:          firstNonEmptyString(extra.DisplayName, extra.Name),
			ContextLength: extra.ContextLength,
		})
	}

	// 打上禁用标记供页面显示状态与勾选。
	//
	// 这里**不做过滤**：禁用的模型仍要列出来，否则用户看不到自己禁用了什么、
	// 也无法再启用。真正生效的过滤在注册路径（models.go 的 filterDisabledModels）。
	models = withDisabledFlag(models)
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })

	payload := map[string]any{"ok": true, "models": models, "count": len(models)}
	if len(models) == 0 {
		// 缓存为空通常意味着还没探测过：给页面一句可执行的提示。
		payload["error"] = "本地暂无模型缓存，正在从上游获取；也可点「从上游刷新」立即拉取。"
	}
	return jsonResponse(http.StatusOK, payload)
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
		"prompt_mode":       cfg.PromptMode,
		"log_level":         logger.CurrentLevel(),
		"state_dir":         cfg.StateDir,
		"accounts":          accounts,
		"account_count":     len(accounts),
		"model_counts":      modelCounts,
		"models_fetched_at": state.ModelsFetchedAt,
		"settings":          effectiveSettings(),
		"scheduler":         schedulerStatus(),
	}
}

// accountSummary 是账号概览条目（不含凭证）。
type accountSummary struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Label     string `json:"label"`
	Realm     string `json:"realm"`
	Status    string `json:"status"`
	Disabled  bool   `json:"disabled"`
	// Checkin 是签到记录（可能为空）。
	Checkin *checkinRecord `json:"checkin,omitempty"`
}

// listAccountSummaries 列出本插件名下的账号概览。
func listAccountSummaries(ctx context.Context, callbackID string) []accountSummary {
	entries, errList := listHostAuths(ctx, callbackID)
	if errList != nil {
		logger.Debug("list host auths failed: %v", errList)
		return nil
	}
	state := snapshotState()
	cfg := loadedConfig()

	out := make([]accountSummary, 0, len(entries))
	for _, entry := range entries {
		summary := accountSummary{
			AuthID:    firstNonEmptyString(entry.ID, entry.Name),
			AuthIndex: entry.AuthIndex,
			Label:     firstNonEmptyString(entry.Label, entry.Name, entry.ID),
			Status:    entry.Status,
			Disabled:  entry.Disabled,
		}
		// realm 从凭证里读；读不到时按配置兜底。
		if raw, okRaw := getAuthJSONByIndex(ctx, callbackID, entry.AuthIndex); okRaw {
			if credential, errParse := cb.ParseCredential(raw, defaultRealmForParse(cfg)); errParse == nil {
				summary.Realm = string(credential.Realm())
				if nickname := credential.NicknameValue(); nickname != "" {
					summary.Label = nickname
				}
				if uid := credential.UIDValue(); uid != "" {
					if record, okRecord := state.Checkin[uid]; okRecord {
						summary.Checkin = &record
					}
				}
			}
		}
		if summary.Realm == "" {
			summary.Realm = string(defaultRealmForParse(cfg))
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
func handleCheckinRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body := managementBody(req)
	targets := stringSliceField(body, "account_ids")
	ctx, cancel := managementContext(req)
	defer cancel()

	results := runForAccounts(ctx, hostCallbackID(req), targets, func(ctx context.Context, credential *cb.Credential) (string, error) {
		client := newUpstreamClient(ctx)
		result, errCheckin := client.DailyCheckin(credential)
		if errCheckin != nil {
			return "", errCheckin
		}
		recordCheckinResult(credential, result)
		if result.Already {
			return "今天已签到（幂等）", nil
		}
		return fmt.Sprintf("签到成功：+%d 积分 +%d 能量，连续 %d 天",
			result.Credit, result.Energy, result.Streak), nil
	})
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
}

// recordCheckinResult 把签到结果写入状态。
func recordCheckinResult(credential *cb.Credential, result *cb.CheckinResult) {
	if credential == nil || result == nil {
		return
	}
	uid := credential.UIDValue()
	if uid == "" {
		return
	}
	today := todayString()
	mutateState(func(state *pluginState) {
		record := state.Checkin[uid]
		record.LastDate = today
		if result.Streak > 0 {
			record.Streak = result.Streak
		}
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
	AuthID   string `json:"auth_id"`
	Label    string `json:"label"`
	Realm    string `json:"realm"`
	OK       bool   `json:"ok"`
	Message  string `json:"message,omitempty"`
	Remain   int64  `json:"remain"`
	Total    int64  `json:"total"`
	Packages string `json:"packages,omitempty"`
}

// handleQuotasRequest 批量查询额度。
func handleQuotasRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body := managementBody(req)
	targets := stringSliceField(body, "account_ids")
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
	wanted := make(map[string]bool, len(targets))
	for _, target := range targets {
		wanted[strings.TrimSpace(target)] = true
	}

	cfg := loadedConfig()
	type job struct {
		entry      hostAuthEntry
		credential *cb.Credential
	}
	jobs := make([]job, 0, len(entries))
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		if len(wanted) > 0 && !wanted[entry.ID] && !wanted[entry.Name] && !wanted[entry.AuthIndex] {
			continue
		}
		raw, okRaw := getAuthJSONByIndex(ctx, callbackID, entry.AuthIndex)
		if !okRaw {
			continue
		}
		credential, errParse := cb.ParseCredential(raw, defaultRealmForParse(cfg))
		if errParse != nil {
			continue
		}
		jobs = append(jobs, job{entry: entry, credential: credential})
	}

	results := make([]quotaResult, len(jobs))
	semaphore := make(chan struct{}, managementBatchConcurrency)
	done := make(chan int, len(jobs))
	for index := range jobs {
		go func(slot int) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			current := jobs[slot]
			result := quotaResult{
				AuthID: firstNonEmptyString(current.entry.ID, current.entry.Name),
				Label:  firstNonEmptyString(current.credential.NicknameValue(), current.entry.Label, current.entry.Name),
				Realm:  string(current.credential.Realm()),
			}
			client := newUpstreamClient(ctx)
			balance, errBalance := client.FetchBalance(current.credential)
			if errBalance != nil {
				result.Message = errBalance.Error()
			} else {
				result.OK = true
				result.Remain = balance.Remain
				result.Total = balance.Total
				result.Packages = summarizePackages(balance)
			}
			results[slot] = result
			done <- slot
		}(index)
	}
	for range jobs {
		<-done
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
}

// summarizePackages 把资源包明细压成一行文字（供表格展示）。
func summarizePackages(balance *cb.Balance) string {
	if balance == nil || len(balance.Packages) == 0 {
		return ""
	}
	parts := make([]string, 0, len(balance.Packages))
	for _, pkg := range balance.Packages {
		name := strings.TrimSpace(pkg.Name)
		if name == "" {
			name = "资源包"
		}
		parts = append(parts, fmt.Sprintf("%s %d/%d", name, pkg.Remain, pkg.Total))
	}
	return strings.Join(parts, " · ")
}

// accountActionResult 是单账号操作的执行结果。
type accountActionResult struct {
	AuthID  string `json:"auth_id"`
	Label   string `json:"label"`
	Realm   string `json:"realm"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// runForAccounts 对指定账号（或全部账号）并发执行一个动作。
//
// 并发度有上限：任务类操作会在上游留下行为记录，全账号瞬间并发容易被风控注意到。
func runForAccounts(ctx context.Context, callbackID string, targets []string, action func(context.Context, *cb.Credential) (string, error)) []accountActionResult {
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
		credential *cb.Credential
	}
	jobs := make([]job, 0, len(entries))
	for _, entry := range entries {
		if len(wanted) > 0 {
			if !wanted[entry.ID] && !wanted[entry.Name] && !wanted[entry.AuthIndex] {
				continue
			}
		}
		if entry.Disabled {
			continue
		}
		raw, okRaw := getAuthJSONByIndex(ctx, callbackID, entry.AuthIndex)
		if !okRaw {
			continue
		}
		credential, errParse := cb.ParseCredential(raw, defaultRealmForParse(cfg))
		if errParse != nil {
			continue
		}
		if !realmEnabled(cfg, string(credential.Realm())) {
			continue
		}
		jobs = append(jobs, job{entry: entry, credential: credential})
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
				AuthID: firstNonEmptyString(current.entry.ID, current.entry.Name),
				Label:  firstNonEmptyString(current.credential.NicknameValue(), current.entry.Label),
				Realm:  string(current.credential.Realm()),
			}
			message, errAction := action(ctx, current.credential)
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
func handleSettingsRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body := managementBody(req)
	changed := map[string]any{}

	mutateState(func(state *pluginState) {
		if raw, okRaw := body["auto_checkin"]; okRaw {
			if value, okValue := raw.(bool); okValue {
				state.AutoCheckin = &value
				changed["auto_checkin"] = value
			}
		}
		if raw, okRaw := body["auto_checkin_at"].(string); okRaw && strings.TrimSpace(raw) != "" {
			if _, _, errClock := parseClock(raw); errClock != nil {
				return
			}
			state.AutoCheckinAt = strings.TrimSpace(raw)
			changed["auto_checkin_at"] = state.AutoCheckinAt
		}
		if raw, okRaw := body["auto_tasks"]; okRaw {
			if value, okValue := raw.(bool); okValue {
				state.AutoTasks = &value
				changed["auto_tasks"] = value
			}
		}
		// 任务排程：五组整点小时列表。
		for key, target := range map[string]*[]int{
			"checkin_hours":   &state.CheckinHours,
			"travel_hours":    &state.TravelHours,
			"activity_hours":  &state.ActivityHours,
			"keepalive_hours": &state.KeepaliveHours,
			"blackcat_hours":  &state.BlackcatHours,
		} {
			hours, okHours := intSliceField(body, key)
			if !okHours {
				continue
			}
			*target = hours
			changed[key] = hours
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

// handleSchoolStatus 返回开学季活动状态。
func handleSchoolStatus(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := managementContext(req)
	defer cancel()
	statuses := schoolStatusForAccounts(ctx, hostCallbackID(req))
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "accounts": statuses})
}

// stringSliceField 从 body 里取字符串数组字段。
func stringSliceField(body map[string]any, key string) []string {
	raw, okRaw := body[key]
	if !okRaw {
		return nil
	}
	switch typed := raw.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, okText := item.(string); okText && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return out
	case string:
		return splitList(typed)
	}
	return nil
}

// intSliceField 从 body 里取整数数组字段。
func intSliceField(body map[string]any, key string) ([]int, bool) {
	raw, okRaw := body[key]
	if !okRaw {
		return nil, false
	}
	items, okItems := raw.([]any)
	if !okItems {
		return nil, false
	}
	out := make([]int, 0, len(items))
	for _, item := range items {
		switch typed := item.(type) {
		case float64:
			hour := int(typed)
			if hour < 0 || hour > 23 {
				return nil, false
			}
			out = append(out, hour)
		case int:
			if typed < 0 || typed > 23 {
				return nil, false
			}
			out = append(out, typed)
		}
	}
	return out, true
}
