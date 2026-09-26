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
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptText
}

// promptModeFor 返回当前生效的提示词模式。
func promptModeFor() string {
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
//
// 任何异常（坏 JSON、空 body、空提示词）都原样返回，绝不失败：
// 提示词处理是优化项，不该成为请求失败的原因。
func ApplyPrompt(body []byte) []byte {
	mode := promptModeFor()
	text := promptTextFor(loadedConfig())
	if mode == promptModePassThru {
		return body
	}
	if promptDegradedActive() {
		// 降级态：无论哪种模式都换成极简提示词。
		return rewriteSystem(body, degradedPrompt)
	}
	if strings.TrimSpace(text) == "" {
		return body
	}
	if mode == promptModeCustom {
		return rewriteSystem(body, text)
	}
	return appendSystem(body, text)
}

// rewriteSystem 删除全部 system/developer 消息并在开头插入一条新的 system。
func rewriteSystem(body []byte, text string) []byte {
	payload := decodeJSONMap(body)
	if payload == nil {
		return body
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return body
	}
	kept := make([]any, 0, len(messages)+1)
	kept = append(kept, map[string]any{"role": "system", "content": text})
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			kept = append(kept, item)
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer":
			continue
		}
		kept = append(kept, message)
	}
	payload["messages"] = kept
	return encodeJSONMap(payload, body)
}

// appendSystem 在开头连续的 system/developer 块之后插入一条网关 system。
//
// 插在块之后而不是最前面：客户端常把「身份 + 工具说明」拆成多条 system，
// 从中间插入会破坏它们之间的上下文关系。
func appendSystem(body []byte, text string) []byte {
	payload := decodeJSONMap(body)
	if payload == nil {
		return body
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return body
	}
	insertAt := 0
	for insertAt < len(messages) {
		message, okMessage := messages[insertAt].(map[string]any)
		if !okMessage {
			break
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer":
			insertAt++
			continue
		}
		break
	}
	out := make([]any, 0, len(messages)+1)
	out = append(out, messages[:insertAt]...)
	out = append(out, map[string]any{"role": "system", "content": text})
	out = append(out, messages[insertAt:]...)
	payload["messages"] = out
	return encodeJSONMap(payload, body)
}
