package app

// freetier2api —— 多供应商免费额度插件。
//
// 本文件包含插件身份、RPC 方法分发与注册响应。C ABI 在 cgo.go；
// 供应商注册表由 App 构造，组合根在 vendors.go。

import (
	"encoding/json"
	"fmt"
	"strings"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

const (
	pluginID          = "freetier2api"
	pluginDisplayName = "FreeTier 2API"
	pluginAuthor      = "jiandanc"
	pluginRepository  = "https://github.com/jiandanc/CLIProxyAPI-freetier2api-plugin"

	providerKey           = core.ProviderKey
	managementRoutePrefix = "/plugins/" + pluginID
	jsonContentType       = "application/json; charset=utf-8"
	htmlContentType       = "text/html; charset=utf-8"
	formatChatCompletions = core.FormatChatCompletions
)

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ResponseBeforeTranslator                  bool `json:"response_before_translator"`
	ResponseBeforeTranslatorOmitRequestBodies bool `json:"response_before_translator_omit_request_bodies,omitempty"`

	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	QuotaProvider         bool     `json:"quota_provider"`
	ManagementAPI         bool     `json:"management_api"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

// HandleMethod 是插件 ABI 的总入口。
func (a *App) HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister:
		return a.handlePluginRegister(request)
	case pluginabi.MethodPluginReconfigure:
		return a.handlePluginReconfigure(request)
	case pluginabi.MethodPluginQuiesce:
		a.StopBackgroundWork()
		return core.OKEnvelope(struct{}{})

	case pluginabi.MethodModelRegister:
		return a.handleModelRegister(request)
	case pluginabi.MethodModelStatic:
		return a.handleModelStatic(request)
	case pluginabi.MethodModelForAuth:
		return a.handleModelForAuth(request)

	case pluginabi.MethodAuthIdentifier:
		return core.OKEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodAuthParse:
		return a.handleAuthParse(request)
	case pluginabi.MethodAuthLoginStart:
		return a.handleAuthLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return a.handleAuthLoginPoll(request)
	case pluginabi.MethodAuthRefresh:
		return a.handleAuthRefresh(request)

	case pluginabi.MethodExecutorIdentifier:
		return core.OKEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodExecutorExecute:
		return a.handleExecutorExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return a.handleExecutorExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return a.handleExecutorCountTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return a.handleExecutorHTTPRequest(request)
	case pluginabi.MethodResponseNormalizeBefore:
		return a.handleResponseNormalizeBefore(request)

	case pluginabi.MethodQuotaIdentifier:
		return core.OKEnvelope(identifierResponse{Identifier: providerKey})
	case pluginabi.MethodQuotaDescribe:
		return a.handleQuotaDescribe(request)
	case pluginabi.MethodQuotaFetch:
		return a.handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return a.handleQuotaReset(request)

	case pluginabi.MethodManagementRegister:
		return core.OKEnvelope(a.managementRegistration())
	case pluginabi.MethodManagementHandle:
		return a.handleManagement(request)

	default:
		return core.ErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

func (a *App) handlePluginRegister(request []byte) ([]byte, error) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()

	lifecycle := lifecycleRequestFrom(request)
	cfg, errConfig := a.decodeConfig(lifecycle.ConfigYAML)
	if errConfig != nil {
		return nil, fmt.Errorf("plugin.register: %w", errConfig)
	}
	if errApply := a.ApplyConfig(cfg); errApply != nil {
		return nil, fmt.Errorf("plugin.register: %w", errApply)
	}
	if _, errState := a.loadState(cfg); errState != nil {
		logger.Error("load state failed: %v", errState)
	}
	a.applyModelCatalog(cfg)
	a.hostSchemaVersion.Store(lifecycle.SchemaVersion)
	if !a.registered {
		a.registered = true
		a.StartBackgroundWork()
	}
	logger.Info("registered: realms=%v state_dir=%s", cfg.EnabledRealms, cfg.StateDir)
	return core.OKEnvelope(a.Registration())
}

func (a *App) handlePluginReconfigure(request []byte) ([]byte, error) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()

	lifecycle := lifecycleRequestFrom(request)
	cfg, errConfig := a.decodeConfig(lifecycle.ConfigYAML)
	if errConfig != nil {
		return nil, fmt.Errorf("plugin.reconfigure: %w", errConfig)
	}
	previous := a.Config()
	if errApply := a.ApplyConfig(cfg); errApply != nil {
		return nil, fmt.Errorf("plugin.reconfigure: %w", errApply)
	}
	if previous.StateDir != cfg.StateDir {
		if _, errState := a.loadState(cfg); errState != nil {
			logger.Error("reload state failed: %v", errState)
		}
	}
	a.applyModelCatalog(cfg)
	if lifecycle.SchemaVersion != 0 {
		a.hostSchemaVersion.Store(lifecycle.SchemaVersion)
	}
	logger.Info("reconfigured: realms=%v state_dir=%s", cfg.EnabledRealms, cfg.StateDir)
	return core.OKEnvelope(a.Registration())
}

func lifecycleRequestFrom(raw []byte) lifecycleRequest {
	var req lifecycleRequest
	if len(raw) != 0 {
		_ = json.Unmarshal(raw, &req)
	}
	return req
}

// schemaVersionResponseBeforeOmitRequestBodies is negotiated with the host;
// legacy hosts continue to receive the schema-6 contract.
const schemaVersionResponseBeforeOmitRequestBodies = pluginabi.SchemaVersionResponseBeforeOmitRequestBodies

// Registration 构造注册响应。
func (a *App) Registration() registration {
	omitRequestBodies := a.hostSchemaVersion.Load() >= schemaVersionResponseBeforeOmitRequestBodies
	schemaVersion := pluginabi.SchemaVersionRawManagementResponse
	if omitRequestBodies {
		schemaVersion = schemaVersionResponseBeforeOmitRequestBodies
	}
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginDisplayName,
			Version:          a.version,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			ConfigFields:     a.configFields(),
		},
		Capabilities: registrationCapabilities{
			ResponseBeforeTranslator:                  true,
			ResponseBeforeTranslatorOmitRequestBodies: omitRequestBodies,

			ModelProvider: true,
			AuthProvider:  true,
			Executor:      true,
			QuotaProvider: true,
			ManagementAPI: true,
			// 本插件的模型全部按凭证下发（免费福利额度是按账号、按模型授予的），
			// 没有「无凭证也可以用的静态目录」。声明 both 会让宿主把 static 目录
			// 额外注册成一个 model-registry 客户端（plugin:<id>:<provider>:executor），
			// 与 model.for_auth 的客户端各记一次引用；删除凭证只注销后者，前者留在
			// 注册表里就是 /v1/models 上永不消失的幽灵模型（issue #58）。
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeOAuth),
			ExecutorInputFormats:  []string{formatChatCompletions},
			ExecutorOutputFormats: []string{formatChatCompletions},
		},
	}
}

// configFields 汇总全局字段与供应商自描述字段。
func (a *App) configFields() []pluginapi.ConfigField {
	fields := []pluginapi.ConfigField{
		{Name: "enabled_realms", Type: pluginapi.ConfigFieldTypeString, Description: "启用的域（逗号分隔）：cn 国内 / global 国际。默认 cn,global。"},
		{Name: "extra_models", Type: pluginapi.ConfigFieldTypeString, Description: "额外注册的模型名（不含前缀），逗号分隔。"},
		{Name: "disabled_models", Type: pluginapi.ConfigFieldTypeString, Description: "禁用的模型列表。"},
		{Name: "state_dir", Type: pluginapi.ConfigFieldTypeString, Description: "插件状态目录。"},
		{Name: "log_level", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"debug", "info", "error"}, Description: "插件日志级别。"},
		{Name: "log_to_file", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否把日志写入 <state_dir>/logs。"},
		{Name: "auto_checkin", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否每日自动签到。"},
		{Name: "auto_checkin_at", Type: pluginapi.ConfigFieldTypeString, Description: "自动签到时间（HH:MM）。"},
		{Name: "auto_tasks", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否每日自动跑任务闭环。"},
		{Name: "checkin_history_limit", Type: pluginapi.ConfigFieldTypeInteger, Description: "每个账号保留的签到历史条数，默认 30。"},
	}
	for _, vendor := range a.registry.Vendors() {
		if provider, ok := core.As[core.ConfigFieldsProvider](vendor); ok {
			fields = append(fields, provider.ConfigFields()...)
		}
	}
	return fields
}

func decodeRequest(raw []byte, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if errUnmarshal := json.Unmarshal(raw, target); errUnmarshal != nil {
		return fmt.Errorf("decode request: %w", errUnmarshal)
	}
	return nil
}

func isChatFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai", "chat-completions", "chat_completions", "openai-chat-completions":
		return true
	}
	return false
}
