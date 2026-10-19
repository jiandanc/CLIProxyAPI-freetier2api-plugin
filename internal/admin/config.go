// 本文件解析与校验插件的全局配置（宿主下发的 config.yaml 片段）。
//
// 放在 admin 包内：配置只有两个消费方——组合根读取它启动，以及管理接口
// 读取/回写运行期设置，二者都已经依赖 admin。
package admin

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PluginConfig 是 plugins.configs.freetier2api 解析出的有效全局配置。
type PluginConfig struct {
	// EnabledRealms 是启用的域：逗号分隔的 cn / global 子集。
	EnabledRealms []string
	// ExtraModels 是额外注册的模型名（不含 realm 前缀）。
	ExtraModels []string
	// DisabledModels 是配置声明的禁用模型（原始写法，展开由模型层完成）。
	DisabledModels []string
	// StateDir 是插件状态目录（机器盐、模型缓存、任务记录、日志）。
	StateDir string
	// LogLevel 控制插件日志：debug / info / error。
	LogLevel string
	// LogToFile 为 true 时把日志写入 <StateDir>/logs。
	LogToFile bool

	// AutoCheckin 开启每日自动签到。
	AutoCheckin bool
	// AutoCheckinAt 是自动签到时间（本地时区 HH:MM）。
	AutoCheckinAt string
	// AutoTasks 开启成长任务的每日自动闭环。
	AutoTasks bool

	// Raw 是配置的原始键值表（key → 原文），供供应商解析专属键。
	Raw map[string]string
}

const (
	// DefaultLogLevel 是默认日志级别。
	DefaultLogLevel = "info"
	// DefaultCheckinAt 是默认签到时间。
	DefaultCheckinAt = "10:00"
	// DefaultStateDirName 是默认状态目录名。
	DefaultStateDirName = ".freetier2api-plugin"
	// StateDirEnvOverride 是状态目录的环境变量覆盖项。
	StateDirEnvOverride = "FREETIER2API_PLUGIN_HOME"
	// StateFileName 是状态文件名。
	StateFileName = "state.json"
	// SaltFileName 是机器盐文件名。
	SaltFileName = "machine_salt"
	// DefaultRealmList 是默认启用的域。
	DefaultRealmList = "cn,global"

	// RealmCN / RealmGlobal 是受支持的域。
	RealmCN     = "cn"
	RealmGlobal = "global"

	// PromptModeCustom / PromptModeAppend / PromptModePassThrough 是合法模式。
	PromptModeCustom      = "custom"
	PromptModeAppend      = "append"
	PromptModePassThrough = "passthrough"
)

// listValuedConfigKeys 是「允许用 YAML 列表写法」的配置键。
var listValuedConfigKeys = map[string]bool{
	"enabled_realms":  true,
	"extra_models":    true,
	"disabled_models": true,
}

// Default 返回内置默认配置（不含供应商默认值）。
func DefaultPluginConfig() PluginConfig {
	return PluginConfig{
		EnabledRealms: SplitList(DefaultRealmList),
		StateDir:      DefaultStateDir(),
		LogLevel:      DefaultLogLevel,
		AutoCheckin:   true,
		AutoCheckinAt: DefaultCheckinAt,
		AutoTasks:     true,
	}
}

// DefaultStateDir 推断默认状态目录：环境变量 > 用户主目录 > 临时目录。
//
// 三级回退是为了让插件在只读 HOME（容器、CI）里也能起来。
func DefaultStateDir() string {
	if env := strings.TrimSpace(os.Getenv(StateDirEnvOverride)); env != "" {
		return env
	}
	if home, errHome := os.UserHomeDir(); errHome == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, DefaultStateDirName)
	}
	return filepath.Join(os.TempDir(), DefaultStateDirName)
}

// Decode 解析宿主传入的配置 YAML，base 为起始默认值（供应商默认值由调用方补）。
//
// 支持 `key: value` 标量行、# 注释、单双引号字符串，以及列表字段的块/流式写法。
func DecodePluginConfig(raw []byte, base PluginConfig) (PluginConfig, error) {
	cfg := base
	if cfg.Raw == nil {
		cfg.Raw = map[string]string{}
	}
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
	if errValidate := ValidatePluginConfig(cfg); errValidate != nil {
		return cfg, errValidate
	}
	return cfg, nil
}

func applyConfigLine(cfg *PluginConfig, key, value string) error {
	if cfg.Raw == nil {
		cfg.Raw = map[string]string{}
	}
	cfg.Raw[key] = value
	switch key {
	case "enabled_realms":
		realms, errRealms := ParseRealms(value)
		if errRealms != nil {
			return errRealms
		}
		cfg.EnabledRealms = realms
	case "extra_models":
		cfg.ExtraModels = SplitList(stripInlineList(value))
	case "disabled_models":
		cfg.DisabledModels = SplitList(stripInlineList(value))
	case "state_dir":
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cfg.StateDir = trimmed
		}
	case "log_level":
		if trimmed := strings.ToLower(strings.TrimSpace(value)); trimmed != "" {
			cfg.LogLevel = trimmed
		}
	case "log_to_file":
		cfg.LogToFile = ParseBool(value, cfg.LogToFile)
	case "prompt_mode":
		// 提示词体系属于供应商标属，但枚举值在全局层校验：
		// 写错时明确报错，而不是让供应商静默回落 passthrough。
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", PromptModeCustom, PromptModeAppend, PromptModePassThrough:
		default:
			return fmt.Errorf("prompt_mode %q is not supported (want passthrough, custom or append)", value)
		}
	case "auto_checkin":
		cfg.AutoCheckin = ParseBool(value, cfg.AutoCheckin)
	case "auto_checkin_at":
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cfg.AutoCheckinAt = trimmed
		}
	case "auto_tasks":
		cfg.AutoTasks = ParseBool(value, cfg.AutoTasks)
	}
	return nil
}

// Validate 校验配置。错误信息直接指向有问题的键，便于运维定位。
func ValidatePluginConfig(cfg PluginConfig) error {
	if len(cfg.EnabledRealms) == 0 {
		return fmt.Errorf("enabled_realms is empty (want a subset of cn,global)")
	}
	if _, _, errClock := ParseClock(cfg.AutoCheckinAt); errClock != nil {
		return fmt.Errorf("auto_checkin_at: %w", errClock)
	}
	switch cfg.LogLevel {
	case "debug", "info", "error":
	default:
		return fmt.Errorf("log_level %q is not supported (want debug, info or error)", cfg.LogLevel)
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return fmt.Errorf("state_dir is empty")
	}
	return nil
}

// ParseRealms 解析 realm 列表并去重。
func ParseRealms(raw string) ([]string, error) {
	items := SplitList(stripInlineList(raw))
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		realm := strings.ToLower(strings.TrimSpace(item))
		switch realm {
		case RealmCN, RealmGlobal:
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

// ParseBool 解析布尔值，无法解析时保留 fallback。
func ParseBool(raw string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	}
	return fallback
}

// ParseClock 解析 HH:MM 格式的本地时间。
func ParseClock(raw string) (hour, minute int, err error) {
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

// SplitList 按逗号或空白切分列表项。
func SplitList(raw string) []string {
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

// normalizeListConfigLines 把列表字段的块写法归一化成单行标量。
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
