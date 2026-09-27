package qoder

// 本文件实现 Qoder 的模型清单。
//
// 与 WorkBuddy 的多路探测不同，Qoder 的模型清单只有一条来源：上游的
// model/list 接口（需要 cosy 会话）。探测失败时退回内置清单——
// 没有模型注册比注册一份可能过时的清单更糟（客户端会看不到任何模型）。

import (
	"freetier2api-plugin/internal/vendors/qoder/bridge"
)

// BundledQoderModels 是内置的兜底模型清单。
//
// 来源：移植自 qoder2api 的 HandleListModels 兜底清单，不是凭空构造的；
// 真实可用集合以「刷新模型」拉到的实时清单为准。
func BundledQoderModels() []bridge.QoderModel {
	return []bridge.QoderModel{
		{Key: "auto", DisplayName: "Auto", Enable: true, IsDefault: true},
		{Key: "ultimate", DisplayName: "Ultimate", Enable: true},
		{Key: "performance", DisplayName: "Performance", Enable: true},
		{Key: "efficient", DisplayName: "Efficient", Enable: true},
		{Key: "qmodel_38max", DisplayName: "Qwen3.8-Max", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "qmodel_latest", DisplayName: "Qwen3.7-Max", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "qmodel", DisplayName: "Qwen3.7-Plus", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "kmodel_latest", DisplayName: "Kimi-K3", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "kmodel", DisplayName: "Kimi-K2.8-Preview", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "gmodel", DisplayName: "GLM-5.3", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "gfmodel", DisplayName: "GLM-5.3-Flash", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro", Enable: true, IsReasoning: true, ContextWindow: 180000, MaxOutputTokens: 32768},
		{Key: "dfmodel", DisplayName: "DeepSeek-Flash", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "mmodel", DisplayName: "MiniMax-M3", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
	}
}
