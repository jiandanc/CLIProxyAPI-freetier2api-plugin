package app

// 本文件负责全局配置与落盘状态的 App 级入口。
//
// 配置解析/校验在 internal/admin，state.json 的读写/快照在
// internal/state；App 持有实例，不再使用包级全局。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"freetier2api-plugin/internal/admin"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/state"
	"freetier2api-plugin/internal/tasks"
)

type pluginConfig = admin.PluginConfig

type pluginState = state.State

func defaultPluginConfig() pluginConfig { return admin.DefaultPluginConfig() }

func defaultStateDir() string { return admin.DefaultStateDir() }

// Config 返回当前生效的配置副本。
func (a *App) Config() pluginConfig {
	if cfg := a.cfg.Load(); cfg != nil {
		return *cfg
	}
	return defaultPluginConfig()
}

func (a *App) storeConfig(cfg pluginConfig) { a.cfg.Store(&cfg) }

func (a *App) decodeConfig(raw []byte) (pluginConfig, error) {
	cfg, errDecode := admin.DecodePluginConfig(raw, defaultPluginConfig())
	if errDecode != nil {
		return cfg, errDecode
	}
	cfg.DisabledModels = a.models.ExpandDisabledList(cfg.DisabledModels)
	return cfg, nil
}

// promptModeFor 返回当前生效的提示词模式（WorkBuddy 运行时设置）。
func (a *App) promptModeFor() string {
	if mode := strings.TrimSpace(a.SnapshotState().VendorSettings["workbuddy_prompt_mode"]); mode != "" {
		return mode
	}
	if mode := strings.TrimSpace(a.Config().Raw["workbuddy_prompt_mode"]); mode != "" {
		return mode
	}
	return "passthrough"
}

// ApplyConfig 应用配置：初始化日志与状态目录、设置指纹盐、下发供应商配置。
func (a *App) ApplyConfig(cfg pluginConfig) error {
	if errDir := ensureStateDir(cfg); errDir != nil {
		return errDir
	}
	logger.SetLevel(cfg.LogLevel)
	if errLog := logger.InitFile(cfg.LogToFile, filepath.Join(cfg.StateDir, "logs")); errLog != nil {
		logger.Error("init log file sink failed: %v", errLog)
	}
	a.store.Configure(cfg.StateDir)
	salt, errSalt := a.ensureMachineSalt(cfg)
	if errSalt != nil {
		return errSalt
	}
	a.machineSalt = salt
	a.storeConfig(cfg)
	a.models.Configure(admin.ModelSettings{
		EnabledRealms:    cfg.EnabledRealms,
		ExtraModels:      cfg.ExtraModels,
		DisabledModels:   cfg.DisabledModels,
		ContextWindowFor: core.DefaultContextWindow,
	})
	a.models.RefreshFromState()
	a.configureVendors(cfg)
	return nil
}

func ensureStateDir(cfg pluginConfig) error {
	dir := strings.TrimSpace(cfg.StateDir)
	if dir == "" {
		return fmt.Errorf("state_dir is empty")
	}
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create state dir %s: %w", dir, errMkdir)
	}
	migrateLegacyStateDir(dir)
	return nil
}

func migrateLegacyStateDir(targetDir string) {
	home, errHome := os.UserHomeDir()
	if errHome != nil || strings.TrimSpace(home) == "" {
		return
	}
	legacyDir := filepath.Join(home, ".cli-proxy-api", "freetier2api")
	if filepath.Clean(targetDir) == filepath.Clean(legacyDir) {
		return
	}
	legacyState := filepath.Join(legacyDir, admin.StateFileName)
	if _, errStat := os.Stat(legacyState); errStat != nil {
		return
	}
	targetState := filepath.Join(targetDir, admin.StateFileName)
	if _, errStat := os.Stat(targetState); errStat == nil {
		return
	}

	logger.Info("migrating legacy state dir from %s to %s", legacyDir, targetDir)
	for _, name := range []string{admin.StateFileName, admin.SaltFileName} {
		src := filepath.Join(legacyDir, name)
		dst := filepath.Join(targetDir, name)
		if data, errRead := os.ReadFile(src); errRead == nil {
			if errWrite := os.WriteFile(dst, data, 0o600); errWrite == nil {
				_ = os.Remove(src)
			}
		}
	}
	_ = os.Remove(legacyDir)
}

func saltFilePath(cfg pluginConfig) string {
	return filepath.Join(cfg.StateDir, admin.SaltFileName)
}

func (a *App) loadState(cfg pluginConfig) (*pluginState, error) {
	a.store.Configure(cfg.StateDir)
	loaded, errLoad := a.store.Load()
	if errLoad != nil {
		return nil, errLoad
	}
	a.models.ReloadFromState(*loaded)
	return loaded, nil
}

// SnapshotState 返回状态副本。
func (a *App) SnapshotState() pluginState { return a.store.Snapshot() }

// MutateState 在锁内修改状态并立即落盘。
func (a *App) MutateState(mutate func(*pluginState)) { a.store.Mutate(mutate) }

func (a *App) ensureMachineSalt(cfg pluginConfig) (string, error) {
	if override := strings.TrimSpace(cfg.Raw["machine_salt"]); override != "" {
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

func (a *App) nowRFC3339() string { return a.now().Format(time.RFC3339) }

// taskConfig 把全局配置投影成任务引擎需要的最小形状。
//
// 投影而不是直接传 admin.PluginConfig，是为了让 internal/tasks 不必依赖
// 管理接口包（仓库分层守卫禁止 tasks → admin）。
func taskConfig(cfg admin.PluginConfig) tasks.Config {
	return tasks.Config{
		EnabledRealms: cfg.EnabledRealms,
		AutoCheckin:   cfg.AutoCheckin,
		AutoCheckinAt: cfg.AutoCheckinAt,
		AutoTasks:     cfg.AutoTasks,
	}
}
