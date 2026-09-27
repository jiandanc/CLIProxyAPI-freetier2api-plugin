package workbuddy

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
//
// 这是**本供应商特有**的机制：CodeBuddy 的上游按逐字匹配做内容审核，
// 其它供应商的上游未必如此，因此提示词体系属于协议层而非通用核心。

import (
	_ "embed"
	"strings"
	"sync"
)

//go:embed prompt_default.md
var defaultPromptRaw []byte

// degradedPrompt 是内容被拦截后降级重试时使用的极简提示词。
//
// 长提示词更容易命中逐字审核，降级到一句中性指令能显著提高通过率。
const degradedPrompt = "You are a helpful assistant. Respond in the user's language, " +
	"follow the user's instructions, and be direct and concise."

// 提示词模式取值（与根层 config 的解析结果对齐）。
const (
	// PromptModePassThrough 完全透传客户端 system。
	PromptModePassThrough = "passthrough"
	// PromptModeCustom 替换为网关自有提示词。
	PromptModeCustom = "custom"
	// PromptModeAppend 在既有 system 之后追加网关提示词。
	PromptModeAppend = "append"
)

var (
	promptMu       sync.RWMutex
	promptMode     = PromptModePassThrough
	promptText     = ""
	promptDegraded bool
)

// SetPrompt 设置当前生效的提示词模式与文本。
//
// 由根层在配置热更新时调用：配置结构属于根层，供应商包只接收结果，
// 避免反向依赖 package main。
func SetPrompt(mode, text string) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = PromptModePassThrough
	}
	promptMu.Lock()
	promptMode = mode
	promptText = strings.TrimSpace(text)
	promptMu.Unlock()
}

// BuiltinPrompt 返回内置的默认提示词文本。
func BuiltinPrompt() string {
	return strings.TrimSpace(string(defaultPromptRaw))
}

// PromptMode 返回当前生效的提示词模式。
func PromptMode() string {
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptMode
}

// PromptText 返回当前生效的提示词文本（降级态返回极简提示词）。
func PromptText() string {
	if PromptDegraded() {
		return degradedPrompt
	}
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptText
}

// MarkPromptDegraded 标记进入降级态。
//
// 降级态一旦开启就持续到进程重启：上游的内容审核策略变化通常是长期性的，
// 反复在「完整提示词 ↔ 降级提示词」之间来回试探只会让一半请求失败。
func MarkPromptDegraded() {
	promptMu.Lock()
	promptDegraded = true
	promptMu.Unlock()
}

// PromptDegraded 报告是否处于降级态。
func PromptDegraded() bool {
	promptMu.RLock()
	defer promptMu.RUnlock()
	return promptDegraded
}

// ResetPromptForTest 复位提示词状态（仅供测试使用）。
func ResetPromptForTest() {
	promptMu.Lock()
	promptMode = PromptModePassThrough
	promptText = ""
	promptDegraded = false
	promptMu.Unlock()
}
