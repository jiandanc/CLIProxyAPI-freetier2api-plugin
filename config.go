package main

// 本文件负责插件配置：结构体定义、宿主注入 YAML 的解析、校验与生效。
//
// 宿主在 plugin.register / plugin.reconfigure 时把 plugins.configs.freetier2api
// 下的配置渲染成一段 YAML 传进来（会额外追加 enabled / priority 两键）。
// 本插件只解析自己声明的字段，未知字段忽略——这样宿主新增字段不会让插件失效。
//
// 刻意不引入 yaml.v3：只需支持 `key: value` 标量行与块/流式列表，
// 保持 go.mod 零依赖（CGO 构建不需要网络拉包）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// pluginConfig 是 plugins.configs.freetier2api 解析出的有效配置。
type pluginConfig struct {
	// EnabledRealms 是启用的域：逗号分隔的 cn / global 子集。
	// 只留 cn 时插件只注册 cn:* 模型，global 账号不会被路由到。
	EnabledRealms []string
	// ExtraModels 是额外注册的模型名（不含 realm 前缀），用于手打绕过动态目录。
	ExtraModels []string
	// StateDir 是插件状态目录（机器盐、模型缓存、任务记录、日志）。
	StateDir string
	// LogLevel 控制插件日志：debug / info / error。
	LogLevel string
	// LogToFile 为 true 时把日志写入 <StateDir>/logs。
	LogToFile bool

	// PromptMode 决定系统提示词处理：passthrough / custom / append。
	PromptMode string
	// PromptFile 是自定义提示词文件；空则用内置默认提示词。
	PromptFile string

	// SanitizeFingerprints 开启出站请求体黑名单指纹脱敏。
	SanitizeFingerprints bool
	// PassthroughIP 决定是否把客户端 IP 透传给上游。
	PassthroughIP bool

	// UserAgent 显式覆盖出站 UA（非空时全路径生效）。
	UserAgent string
	// ClientVersion / CLIVersion 分别覆盖 UA 中的 WorkBuddy 段与 CLI 段。
	ClientVersion string
	CLIVersion    string
	// ClientName 覆盖归属头取值；空表示默认 WorkBuddy。
	ClientName string

	// DeviceToken 是 X-Device-Token 的全局兜底（账号级 device_token 优先）。
	DeviceToken string
	// DeviceTokenFile 是 device token 文件路径兜底（桌面端 token）。
	DeviceTokenFile string

	// MachineSalt 覆盖自动生成的设备指纹盐。
	// 从 workbuddy2api 迁移且希望指纹不变时，填原 settings 里的 machine_salt。
	MachineSalt string

	// AutoCheckin 开启每日自动签到。
	AutoCheckin bool
	// AutoCheckinAt 是自动签到时间（本地时区 HH:MM）。
	AutoCheckinAt string
	// AutoTasks 开启成长任务的每日自动闭环（连登兑换 + 抽奖 + 旅行 + 夜猫子等）。
	AutoTasks bool

	// ZenBaseURL 覆盖 OpenCode ZEN 的上游基地址（留空用内置默认值）。
	//
	// 上游换域名或用户要走自建网关时不必改代码重编译。
	ZenBaseURL string
	// ClineBaseURL 覆盖 Cline 的上游基地址（留空用内置默认值）。
	ClineBaseURL string
}

const (
	defaultLogLevel      = "info"
	defaultPromptMode    = "passthrough"
	defaultCheckinAt     = "10:00"
	defaultStateDirName  = ".freetier2api-plugin"
	stateDirEnvOverride  = "FREETIER2API_PLUGIN_HOME"
	stateFileName        = "state.json"
	saltFileName         = "machine_salt"
	configKeyEnabled     = "enabled"
	configKeyPriority    = "priority"
	defaultRealmList     = "cn,global"
	realmCN              = "cn"
	realmGlobal          = "global"
	promptModeCustom     = "custom"
	promptModeAppend     = "append"
	promptModePassThru   = "passthrough"
	maxMissingPromptSize = 1 << 20
)

// listValuedConfigKeys 是「允许用 YAML 列表写法」的配置键。
//
// 宿主的 PATCH 接口按 ConfigFieldTypeString 存字符串，但运维手写 config.yaml 时
// 写成块列表（`- item`）或流式列表（`[a, b]`）都很自然。不做归一化的话，
// 这两种写法会被静默忽略（配置写进去了、插件看不见）。
var listValuedConfigKeys = map[string]bool{
	"enabled_realms": true,
	"extra_models":   true,
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		EnabledRealms:        splitList(defaultRealmList),
		StateDir:             defaultStateDir(),
		LogLevel:             defaultLogLevel,
		PromptMode:           defaultPromptMode,
		SanitizeFingerprints: true,
		ClientVersion:        workbuddy.DefaultClientVersion,
		CLIVersion:           workbuddy.DefaultCLIVersion,
		ClientName:           workbuddy.DefaultClientName,
		// 默认开启：装了插件就希望它自己把签到与任务跑起来，
		// 让用户先去配置里找开关再打开是多余的。
		AutoCheckin:   true,
		AutoCheckinAt: defaultCheckinAt,
		AutoTasks:     true,
	}
}

// defaultStateDir 推断默认状态目录：环境变量 > 用户主目录 > 进程临时目录。
//
// 三级回退是为了让插件在只读 HOME（容器、CI）里也能起来。
func defaultStateDir() string {
	if env := strings.TrimSpace(os.Getenv(stateDirEnvOverride)); env != "" {
		return env
	}
	if home, errHome := os.UserHomeDir(); errHome == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, defaultStateDirName)
	}
	return filepath.Join(os.TempDir(), defaultStateDirName)
}

// 全局配置：原子指针 + 两个访问器（读多写少，且 reconfigure 与请求并发）。
var currentConfig atomic.Pointer[pluginConfig]

// loadedConfig 返回当前生效的配置副本。
func loadedConfig() pluginConfig {
	if cfg := currentConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultPluginConfig()
}

func storeConfig(cfg pluginConfig) { currentConfig.Store(&cfg) }

// decodeConfig 解析宿主传入的配置 YAML。
//
// 支持 `key: value` 标量行、# 注释、单双引号字符串，以及列表字段的块/流式写法。
// 嵌套块不需要（本插件的配置全部是扁平字段）。
func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if len(raw) == 0 {
		return cfg, nil
	}
	lines := normalizeListConfigLines(strings.Split(string(raw), "\n"))
	for lineNo, line := range lines {
		key, value, ok := splitConfigLine(line)
		if !ok {
			continue
		}
		if errLine := applyConfigLine(&cfg, key, value); errLine != nil {
			return cfg, fmt.Errorf("config line %d: %w", lineNo+1, errLine)
		}
	}
	if errValidate := validateConfig(cfg); errValidate != nil {
		return cfg, errValidate
	}
	return cfg, nil
}

// applyConfigLine 把一行 key/value 应用到配置上。
func applyConfigLine(cfg *pluginConfig, key, value string) error {
	switch key {
	case "enabled_realms":
		realms, errRealms := parseRealms(value)
		if errRealms != nil {
			return errRealms
		}
		cfg.EnabledRealms = realms
	case "extra_models":
		cfg.ExtraModels = splitList(stripInlineList(value))
	case "zen_base_url":
		cfg.ZenBaseURL = strings.TrimSpace(value)
	case "cline_base_url":
		cfg.ClineBaseURL = strings.TrimSpace(value)
	case "state_dir":
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cfg.StateDir = trimmed
		}
	case "log_level":
		if trimmed := strings.ToLower(strings.TrimSpace(value)); trimmed != "" {
			cfg.LogLevel = trimmed
		}
	case "log_to_file":
		cfg.LogToFile = parseBool(value, cfg.LogToFile)
	case "prompt_mode":
		if trimmed := strings.ToLower(strings.TrimSpace(value)); trimmed != "" {
			cfg.PromptMode = trimmed
		}
	case "prompt_file":
		cfg.PromptFile = strings.TrimSpace(value)
	case "sanitize_fingerprints":
		cfg.SanitizeFingerprints = parseBool(value, cfg.SanitizeFingerprints)
	case "passthrough_ip":
		cfg.PassthroughIP = parseBool(value, cfg.PassthroughIP)
	case "user_agent":
		cfg.UserAgent = strings.TrimSpace(value)
	case "client_version":
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cfg.ClientVersion = trimmed
		}
	case "cli_version":
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cfg.CLIVersion = trimmed
		}
	case "client_name":
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cfg.ClientName = trimmed
		}
	case "device_token":
		cfg.DeviceToken = strings.TrimSpace(value)
	case "device_token_file":
		cfg.DeviceTokenFile = strings.TrimSpace(value)
	case "machine_salt":
		cfg.MachineSalt = strings.TrimSpace(value)
	case "auto_checkin":
		cfg.AutoCheckin = parseBool(value, cfg.AutoCheckin)
	case "auto_checkin_at":
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cfg.AutoCheckinAt = trimmed
		}
	case "auto_tasks":
		cfg.AutoTasks = parseBool(value, cfg.AutoTasks)
	case configKeyEnabled, configKeyPriority:
		// 宿主追加的插件启用开关与优先级：由宿主负责，这里只确认字段存在。
	}
	return nil
}

// validateConfig 校验配置。错误信息直接指向有问题的键，便于运维定位。
func validateConfig(cfg pluginConfig) error {
	if len(cfg.EnabledRealms) == 0 {
		return fmt.Errorf("enabled_realms is empty (want a subset of cn,global)")
	}
	if _, _, errClock := parseClock(cfg.AutoCheckinAt); errClock != nil {
		return fmt.Errorf("auto_checkin_at: %w", errClock)
	}
	switch cfg.LogLevel {
	case "debug", "info", "error":
	default:
		return fmt.Errorf("log_level %q is not supported (want debug, info or error)", cfg.LogLevel)
	}
	switch cfg.PromptMode {
	case promptModeCustom, promptModeAppend, promptModePassThru:
	default:
		return fmt.Errorf("prompt_mode %q is not supported (want passthrough, custom or append)", cfg.PromptMode)
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return fmt.Errorf("state_dir is empty")
	}
	return nil
}

// parseRealms 解析 realm 列表并去重。
func parseRealms(raw string) ([]string, error) {
	items := splitList(stripInlineList(raw))
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		realm := strings.ToLower(strings.TrimSpace(item))
		switch realm {
		case realmCN, realmGlobal:
		default:
			return nil, fmt.Errorf("enabled_realms contains unsupported realm %q (want cn or global)", item)
		}
		if seen[realm] {
			continue
		}
		seen[realm] = true
		out = append(out, realm)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("enabled_realms is empty (want a subset of cn,global)")
	}
	return out, nil
}

// realmEnabled 判断某个区域是否被配置启用。
//
// **空区域一律视为启用**：区域是「同一协议的不同部署」（WorkBuddy 国内版 /
// 国际版），只对这类供应商有意义。Cline / OpenCodeZEN 这类单一部署的供应商
// 没有区域概念，Region() 返回空串——若也拿 enabled_realms 去卡，它们会因为
// "cn,global" 里没有空串而被静默禁用，表现为「账号添加成功但模型列表为空」。
func realmEnabled(cfg pluginConfig, realm string) bool {
	if strings.TrimSpace(realm) == "" {
		return true
	}
	for _, item := range cfg.EnabledRealms {
		if item == realm {
			return true
		}
	}
	return false
}

// splitConfigLine 拆出 `key: value`；跳过空行、注释、列表项与非标量行。
func splitConfigLine(line string) (key, value string, ok bool) {
	trimmed := strings.TrimRight(line, "\r")
	trimmed = strings.TrimLeft(trimmed, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
		return "", "", false
	}
	idx := strings.Index(trimmed, ":")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(trimmed[:idx])
	value = strings.TrimSpace(trimmed[idx+1:])
	// 去掉行尾注释：只在值不是引号串时才处理，避免吃掉引号里的 ` #`。
	if !strings.HasPrefix(value, "\"") && !strings.HasPrefix(value, "'") {
		if hash := strings.Index(value, " #"); hash >= 0 {
			value = strings.TrimSpace(value[:hash])
		}
	}
	value = strings.Trim(value, `"'`)
	if key == "" {
		return "", "", false
	}
	return key, value, true
}

// normalizeListConfigLines 把列表字段的块写法归一化成单行标量：
//
//	enabled_realms:        enabled_realms: cn,global
//	- cn              →
//	- global
//
// 不属于列表字段的 `- ` 行原样保留（后续会因不是 key: value 而被忽略）。
func normalizeListConfigLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	index := -1
	key := ""
	var items []string
	flush := func() {
		if index >= 0 && len(items) > 0 {
			out[index] = key + ": " + strings.Join(items, ",")
		}
		index, key, items = -1, "", nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-") {
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			if index >= 0 && listValuedConfigKeys[key] && item != "" {
				items = append(items, strings.Trim(item, `"'`))
				continue
			}
		}
		lineKey, _, ok := splitConfigLine(line)
		if !ok {
			out = append(out, line)
			continue
		}
		flush()
		index, key = len(out), lineKey
		out = append(out, line)
	}
	flush()
	return out
}

// stripInlineList 去掉流式列表写法 `[a, b]` 的方括号。
func stripInlineList(value string) string {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	}
	return trimmed
}

// splitList 按逗号或空白切分列表项。
func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// parseBool 解析布尔值，无法解析时保留 fallback。
func parseBool(raw string, fallback bool) bool {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	switch trimmed {
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	}
	return fallback
}

// parseClock 解析 HH:MM 格式的本地时间。
func parseClock(raw string) (hour, minute int, err error) {
	trimmed := strings.TrimSpace(raw)
	parts := strings.Split(trimmed, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%q is not a HH:MM time", raw)
	}
	hour, errHour := strconv.Atoi(strings.TrimSpace(parts[0]))
	if errHour != nil {
		return 0, 0, fmt.Errorf("%q is not a HH:MM time", raw)
	}
	minute, errMinute := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errMinute != nil {
		return 0, 0, fmt.Errorf("%q is not a HH:MM time", raw)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%q is out of range (want 00:00-23:59)", raw)
	}
	return hour, minute, nil
}

// applyPromptConfig 应用提示词配置到供应商包。
//
// 提示词体系是 WorkBuddy 特有的（它的上游按逐字匹配做内容审核），
// 因此状态存在供应商包里，根层只负责解析配置并推送。
//
// 读不到自定义提示词文件时**不静默降级**：运维会以为提示词生效了，
// 实际还在用内置的，这种静默失败比直接报错更难排查。
func applyPromptConfig(cfg pluginConfig) {
	text := workbuddy.BuiltinPrompt()
	if path := strings.TrimSpace(cfg.PromptFile); path != "" {
		loaded, errLoad := loadPromptFromFile(path)
		if errLoad != nil {
			logger.Error("load prompt file failed, falling back to built-in prompt: %v", errLoad)
		} else if loaded != "" {
			text = loaded
		}
	}
	mode := strings.TrimSpace(cfg.PromptMode)
	// 页面设置覆盖 YAML 配置：用户在控制台页改过的模式以页面为准。
	if state := snapshotState(); state.PromptMode != nil && strings.TrimSpace(*state.PromptMode) != "" {
		mode = strings.TrimSpace(*state.PromptMode)
	}
	workbuddy.SetPrompt(mode, text)
}

// promptModeFor 返回当前生效的提示词模式（供管理端展示）。
func promptModeFor() string { return workbuddy.PromptMode() }

// applyConfig 应用配置：更新全局配置、初始化日志与状态目录、设置指纹盐。
//
// register 与 reconfigure 共用这一个入口，所有副作用都收敛在这里。
func applyConfig(cfg pluginConfig) error {
	if errDir := ensureStateDir(cfg); errDir != nil {
		return errDir
	}
	logger.SetLevel(cfg.LogLevel)
	if errLog := logger.InitFile(cfg.LogToFile, filepath.Join(cfg.StateDir, "logs")); errLog != nil {
		logger.Error("init log file sink failed: %v", errLog)
	}
	if errSalt := configureInstallSalt(cfg); errSalt != nil {
		return errSalt
	}
	applyPromptConfig(cfg)
	storeConfig(cfg)
	return nil
}

// ensureStateDir 创建状态目录。
func ensureStateDir(cfg pluginConfig) error {
	dir := strings.TrimSpace(cfg.StateDir)
	if dir == "" {
		return fmt.Errorf("state_dir is empty")
	}
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create state dir %s: %w", dir, errMkdir)
	}
	return nil
}

// configFilePath 返回配置在某状态目录下的文件路径。
func stateFilePath(cfg pluginConfig) string {
	return filepath.Join(cfg.StateDir, stateFileName)
}

func saltFilePath(cfg pluginConfig) string {
	return filepath.Join(cfg.StateDir, saltFileName)
}

// loadPromptFromFile 读取自定义提示词文件。
//
// 只在 prompt_mode 为 custom/append 且指定了文件时调用；读不到返回错误而不是静默降级，
// 否则运维会以为提示词生效了、实际还在用内置的。
func loadPromptFromFile(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", nil
	}
	raw, errRead := os.ReadFile(trimmed)
	if errRead != nil {
		return "", fmt.Errorf("read prompt file %s: %w", trimmed, errRead)
	}
	if len(raw) > maxMissingPromptSize {
		return "", fmt.Errorf("prompt file %s exceeds %d bytes", trimmed, maxMissingPromptSize)
	}
	return strings.TrimSpace(string(raw)), nil
}
