package main

// 本文件实现系统提示词体系。
//
// 为什么需要它：客户端的 system prompt 里带着大量固定模板句（工具说明、
// 身份声明、环境描述），上游的内容审核按逐字匹配拦截。把 system 换成
// 网关自有提示词是从源头解决，比逐句改写（sanitize）更彻底。
//
// 三种模式：
//   - passthrough：完全透传客户端 system（默认，最不干扰）；
//   - custom：删除全部 system/developer 消息，换成网关自有提示词；
//   - append：在开头连续的 system/developer 块之后追加一条网关提示词，
//     既有消息逐字保留。

import (
	_ "embed"
	"strings"
	"sync"

	"workbuddy2api-plugin/internal/cb"
	"workbuddy2api-plugin/internal/logger"
)

//go:embed prompt_default.md
var defaultPromptRaw []byte

// degradedPrompt 是内容被拦截后降级重试时使用的极简提示词。
//
// 长提示词更容易命中逐字审核，降级到一句中性指令能显著提高通过率。
const degradedPrompt = "You are a helpful assistant. Respond in the user's language, " +
	"follow the user's instructions, and be direct and concise."

var (
	promptMu       sync.RWMutex
	promptMode     = promptModePassThru
	promptText     = ""
	promptDegraded bool
)

// applyPromptConfig 应用提示词配置。
//
// 读不到自定义提示词文件时**不静默降级**：运维会以为提示词生效了，
// 实际还在用内置的，这种静默失败比直接报错更难排查。
func applyPromptConfig(cfg pluginConfig) {
	text := strings.TrimSpace(string(defaultPromptRaw))
	if path := strings.TrimSpace(cfg.PromptFile); path != "" {
		loaded, errLoad := loadPromptFromFile(path)
		if errLoad != nil {
			logger.Error("load prompt file failed, falling back to built-in prompt: %v", errLoad)
		} else if loaded != "" {
			text = loaded
		}
	}
	promptMu.Lock()
	promptMode = cfg.PromptMode
	promptText = text
	promptMu.Unlock()
}

// promptTextFor 返回当前生效的提示词文本（供出站构造使用）。
func promptTextFor(cfg pluginConfig) string {
	if promptDegradedActive() {
		return degradedPrompt
	}
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptText
}

// promptModeFor 返回当前生效的提示词模式（页面设置覆盖 > YAML 配置）。
func promptModeFor() string {
	state := snapshotState()
	if state.PromptMode != nil && strings.TrimSpace(*state.PromptMode) != "" {
		return strings.TrimSpace(*state.PromptMode)
	}
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptMode
}

// markPromptDegraded 标记进入降级态。
//
// 降级态一旦开启就持续到进程重启：上游的内容审核策略变化通常是
// 长期性的，反复在「完整提示词 ↔ 降级提示词」之间来回试探只会让
// 一半请求失败。
func markPromptDegraded() {
	promptMu.Lock()
	promptDegraded = true
	promptMu.Unlock()
}

// promptDegradedActive 报告是否处于降级态。
func promptDegradedActive() bool {
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptDegraded
}

// ApplyPrompt 按当前模式改写请求体的 system 消息。
func ApplyPrompt(body []byte) []byte {
	return cb.PrepareBody(body, cb.PrepareOptions{
		PromptMode: promptModeFor(),
		PromptText: promptTextFor(loadedConfig()),
	})
}
