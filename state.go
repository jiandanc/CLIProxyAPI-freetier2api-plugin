package main

// 本文件负责插件自己的落盘状态：
//   - 状态目录（机器指纹盐、state.json、日志）；
//   - 上游模型清单缓存（避免每次启动都打上游）；
//   - 签到记录、任务执行记录、页面可改的设置覆盖；
//   - 设备指纹盐的加载/生成。
//
// **状态里不保存任何凭证**：CodeBuddy 凭证只存在于 CPA 的 auth 文件中。
// 这带来一个重要后果：所有需要 token 的操作都必须通过 host.auth.get 实时回源。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// stateVersion 是状态文件结构版本，用于将来迁移。
const stateVersion = 1

// taskRecord 是某账号某任务的最近一次执行记录（仅用于展示，不作幂等依据）。
type taskRecord struct {
	// LastRunAt 是最近一次执行时间（RFC3339）。
	LastRunAt string `json:"last_run_at,omitempty"`
	// LastStatus 是最近一次执行结果：done / skipped / error。
	LastStatus string `json:"last_status,omitempty"`
	// LastMessage 是最近一次执行的可读说明。
	LastMessage string `json:"last_message,omitempty"`
	// Credit / Energy 是最近一次领奖所得。
	Credit int64 `json:"credit,omitempty"`
	Energy int64 `json:"energy,omitempty"`
}

// checkinRecord 是某账号的签到记录。
type checkinRecord struct {
	// LastDate 是最近一次签到成功的本地日期（YYYY-MM-DD），用于幂等。
	LastDate string `json:"last_date,omitempty"`
	// Streak 是连续签到天数（来自上游）。
	Streak int64 `json:"streak,omitempty"`
	// Credit / Energy 是最近一次签到所得。
	Credit int64 `json:"credit,omitempty"`
	Energy int64 `json:"energy,omitempty"`
}

// cachedModel 是缓存的单条上游模型元数据。
type cachedModel struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	Realm           string   `json:"realm"`
	ContextLength   int64    `json:"context_length,omitempty"`
	MaxOutputTokens int64    `json:"max_output_tokens,omitempty"`
	Efforts         []string `json:"efforts,omitempty"`
	DefaultEffort   string   `json:"default_effort,omitempty"`
	SupportsImages  bool     `json:"supports_images,omitempty"`
	SupportsTool    bool     `json:"supports_tool_call,omitempty"`
	CanDisableThink bool     `json:"can_disable_thinking,omitempty"`
	OnlyReasoning   bool     `json:"only_reasoning,omitempty"`
	Description     string   `json:"description,omitempty"`
}

// pluginState 是 state.json 的完整结构。
//
// 页面设置用指针字段区分「未设置」与「设为 false/空」：
// 未设置时回落 YAML 配置，设置了就以页面为准。
type pluginState struct {
	Version int `json:"version"`

	// AutoCheckin / AutoCheckinAt / AutoTasks 是页面上可改的任务设置。
	AutoCheckin   *bool   `json:"auto_checkin,omitempty"`
	AutoCheckinAt string  `json:"auto_checkin_at,omitempty"`
	AutoTasks     *bool   `json:"auto_tasks,omitempty"`
	PromptMode    *string `json:"prompt_mode,omitempty"`

	// CheckinHours / TravelHours / ActivityHours / KeepaliveHours / BlackcatHours
	// 是各定时任务的整点小时列表（0-23）。
	CheckinHours   []int `json:"checkin_hours,omitempty"`
	TravelHours    []int `json:"travel_hours,omitempty"`
	ActivityHours  []int `json:"activity_hours,omitempty"`
	KeepaliveHours []int `json:"keepalive_hours,omitempty"`
	BlackcatHours  []int `json:"blackcat_hours,omitempty"`

	// Models 是上游模型清单缓存（按 realm 分桶存于 Realm 字段）。
	Models []cachedModel `json:"models,omitempty"`
	// ModelsFetchedAt 是清单缓存时间（RFC3339）。
	ModelsFetchedAt string `json:"models_fetched_at,omitempty"`

	// DisabledModels 是被禁用的模型 ID（带 cn:/global: 前缀的完整 ID）。
	// 被禁用的模型不再注册给宿主，客户端因此选不到、也请求不到。
	DisabledModels []string `json:"disabled_models,omitempty"`

	// ModelAliases 是模型别名表：<vendorID>:<官方模型 ID> → 对外别名。
	// 别名即注册给宿主的对外 ID；出站前会还原成官方 ID（见 modelalias.go）。
	ModelAliases map[string]string `json:"model_aliases,omitempty"`

	// RestartPending 标记「已有模型变更，但宿主的模型注册表尚未刷新」。
	//
	// 为什么需要它：禁用/启用与别名改的是**状态文件**，宿主只在
	// plugin.register / 配置重载时构建模型注册表，因此 /v1/models 的增删
	// 要等下一次重载（重启宿主或触发一次配置重载）才生效——而调用拦截是即时的。
	// 页面据此常驻提示用户，直到变更被应用（见 modelrestart.go）。
	RestartPending bool `json:"restart_pending,omitempty"`

	// Checkin 按账号 UID 记录签到状态。
	Checkin map[string]checkinRecord `json:"checkin,omitempty"`
	// Tasks 按「uid\x1f任务码」记录任务执行历史。
	Tasks map[string]taskRecord `json:"tasks,omitempty"`
}

var (
	stateMu    sync.Mutex
	stateCache *pluginState
)

// loadState 从磁盘读取状态。文件缺失或损坏时返回空状态（不致命）。
func loadState(cfg pluginConfig) (*pluginState, error) {
	stateMu.Lock()
	defer stateMu.Unlock()

	path := stateFilePath(cfg)
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			stateCache = newPluginState()
			return stateCache, nil
		}
		return nil, fmt.Errorf("read state %s: %w", path, errRead)
	}
	var loaded pluginState
	if errUnmarshal := json.Unmarshal(raw, &loaded); errUnmarshal != nil {
		// 状态损坏不阻止插件启动：宁可丢掉签到记录，也不要让用户无法使用。
		logger.Error("state file %s is corrupt, starting with empty state: %v", path, errUnmarshal)
		stateCache = newPluginState()
		return stateCache, nil
	}
	normalizeState(&loaded)
	stateCache = &loaded
	// 锁内只做纯计算合并（不调用 refreshDisabledModelCache：它会重入 stateMu）。
	reloadDisabledModelCache(mergeDisabledLists(loaded.DisabledModels))
	reloadModelAliasCache(loaded.ModelAliases)
	return stateCache, nil
}

func newPluginState() *pluginState {
	return &pluginState{
		Version:      stateVersion,
		Checkin:      map[string]checkinRecord{},
		Tasks:        map[string]taskRecord{},
		ModelAliases: map[string]string{},
	}
}

// normalizeState 补齐反序列化后可能缺失的 map。
func normalizeState(state *pluginState) {
	state.Version = stateVersion
	if state.Checkin == nil {
		state.Checkin = map[string]checkinRecord{}
	}
	if state.Tasks == nil {
		state.Tasks = map[string]taskRecord{}
	}
	if state.ModelAliases == nil {
		state.ModelAliases = map[string]string{}
	}
}

// snapshotState 返回状态副本，调用方可安全读取。
//
// 手工深拷贝 map/slice：pluginState 含 map 与指针字段，
// 直接浅拷贝会让调用方读到内部结构、与 mutateState 竞争。
func snapshotState() pluginState {
	stateMu.Lock()
	defer stateMu.Unlock()
	if stateCache == nil {
		return *newPluginState()
	}
	out := *stateCache
	out.Models = append([]cachedModel(nil), stateCache.Models...)
	out.CheckinHours = append([]int(nil), stateCache.CheckinHours...)
	out.TravelHours = append([]int(nil), stateCache.TravelHours...)
	out.ActivityHours = append([]int(nil), stateCache.ActivityHours...)
	out.KeepaliveHours = append([]int(nil), stateCache.KeepaliveHours...)
	out.BlackcatHours = append([]int(nil), stateCache.BlackcatHours...)
	out.DisabledModels = append([]string(nil), stateCache.DisabledModels...)
	if stateCache.AutoCheckin != nil {
		enabled := *stateCache.AutoCheckin
		out.AutoCheckin = &enabled
	}
	if stateCache.AutoTasks != nil {
		enabled := *stateCache.AutoTasks
		out.AutoTasks = &enabled
	}
	if stateCache.PromptMode != nil {
		m := *stateCache.PromptMode
		out.PromptMode = &m
	}
	out.Checkin = make(map[string]checkinRecord, len(stateCache.Checkin))
	for key, value := range stateCache.Checkin {
		out.Checkin[key] = value
	}
	out.Tasks = make(map[string]taskRecord, len(stateCache.Tasks))
	for key, value := range stateCache.Tasks {
		out.Tasks[key] = value
	}
	out.ModelAliases = make(map[string]string, len(stateCache.ModelAliases))
	for key, value := range stateCache.ModelAliases {
		out.ModelAliases[key] = value
	}
	return out
}

// mutateState 在锁内修改状态并立即落盘。
//
// 修改函数不应做 IO（它在锁内执行）；落盘失败只记日志，不让调用方失败——
// 状态写不进去不应该让一次签到或一次对话失败。
func mutateState(mutate func(*pluginState)) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if stateCache == nil {
		stateCache = newPluginState()
	}
	mutate(stateCache)
	if errSave := saveStateLocked(); errSave != nil {
		logger.Error("save state failed: %v", errSave)
	}
}

// saveStateLocked 原子写入状态文件。调用方必须持有 stateMu。
func saveStateLocked() error {
	if stateCache == nil {
		return nil
	}
	stateCache.Version = stateVersion
	raw, errMarshal := json.MarshalIndent(stateCache, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal state: %w", errMarshal)
	}
	path := stateFilePath(loadedConfig())
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
		return fmt.Errorf("write state: %w", errWrite)
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace state file: %w", errRename)
	}
	return nil
}

// configureInstallSalt 计算并注入设备指纹盐。
//
// 盐一旦生成必须保持不变：变更会让全部账号的派生机器码漂移（等价于换设备），
// 上游风控会把它当成新设备。
func configureInstallSalt(cfg pluginConfig) error {
	salt, errSalt := ensureMachineSalt(cfg)
	if errSalt != nil {
		return errSalt
	}
	workbuddy.SetInstallSalt(salt)
	return nil
}

// ensureMachineSalt 返回本机指纹盐：配置覆盖 > 已有盐文件 > 首次生成后落盘。
func ensureMachineSalt(cfg pluginConfig) (string, error) {
	if override := strings.TrimSpace(cfg.MachineSalt); override != "" {
		return override, nil
	}
	path := saltFilePath(cfg)
	if raw, errRead := os.ReadFile(path); errRead == nil {
		if salt := strings.TrimSpace(string(raw)); salt != "" {
			return salt, nil
		}
	}
	buf := make([]byte, 24)
	if _, errRand := rand.Read(buf); errRand != nil {
		return "", fmt.Errorf("generate machine salt: %w", errRand)
	}
	salt := hex.EncodeToString(buf)
	if errWrite := os.WriteFile(path, []byte(salt+"\n"), 0o600); errWrite != nil {
		return "", fmt.Errorf("write machine salt: %w", errWrite)
	}
	logger.Info("machine salt generated at %s (do not change: it defines this deployment's device fingerprints)", path)
	return salt, nil
}

// todayString 返回当前本地日期（YYYY-MM-DD），用于签到与任务的按天幂等。
func todayString() string { return nowFunc().Format("2006-01-02") }

// nowFunc 允许测试注入固定时间：签到判定与「今天」的记录都依赖它。
var nowFunc = time.Now

// taskStateKey 构造任务记录的复合键（账号 + 任务码）。
func taskStateKey(uid, code string) string {
	return strings.TrimSpace(uid) + "\x1f" + strings.TrimSpace(code)
}

// nowRFC3339 返回当前时间的 RFC3339 字符串。
func nowRFC3339() string { return nowFunc().Format(time.RFC3339) }

// nowUnixMs 返回当前毫秒时间戳。
func nowUnixMs() int64 { return nowFunc().UnixMilli() }
