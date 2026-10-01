package zcode

// Plan 通道的请求体变换（对齐官方客户端行为）。
//
// 三项变换的作用与必要性：
//  1. system 身份块：上游网关做内容审查，请求缺少官方身份块会被拒为
//     3012「unusual activity」；官客户端固定前置这三个块。
//  2. cache_control：最后一条非 system 消息的末块追加 ephemeral 标记。
//     低于缓存门槛时上游静默忽略，因此无条件追加是安全的。
//  3. metadata.user_id：官方客户端注入了登录用户标识。
//
// 所有变换对畸形输入保持 no-op：变换失败绝不能放大成请求失败。
// 参考 zcode2api/app/body_transform.py。

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

//go:embed zcode_system.json
var zcodeSystemJSON []byte

var (
	zcodeSystemOnce   sync.Once
	zcodeSystemBlocks []any
)

// systemBlocks 返回官方身份块（解析失败时为空，此时跳过注入）。
func systemBlocks() []any {
	zcodeSystemOnce.Do(func() {
		var blocks []any
		if errUnmarshal := json.Unmarshal(zcodeSystemJSON, &blocks); errUnmarshal != nil {
			blocks = nil
		}
		zcodeSystemBlocks = blocks
	})
	return zcodeSystemBlocks
}

// applyStartPlanSystem 前置官方身份块与当前模型块，保留客户端原有 system。
//
// 幂等：首块已是官方标识时直接返回，重复调用不会重复拼接。
func applyStartPlanSystem(body map[string]any, model string) {
	blocks := systemBlocks()
	if len(blocks) == 0 {
		return
	}

	existing := body["system"]
	if list, ok := existing.([]any); ok && len(list) > 0 {
		if first, ok := list[0].(map[string]any); ok {
			if stringField(first, "text") == stringField(blocks[0].(map[string]any), "text") {
				return
			}
		}
	}

	official := make([]any, 0, len(blocks)+2)
	for _, block := range blocks {
		official = append(official, cloneBlock(block))
	}
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		official = append(official, map[string]any{
			"type":          "text",
			"text":          "- You are powered by the model named " + trimmed + ".",
			"cache_control": map[string]any{"type": "ephemeral"},
		})
	}
	body["system"] = append(official, normalizeUserSystem(existing)...)
}

// normalizeUserSystem 把客户端原有 system 归一成 text 块列表。
//
// Anthropic 要求 system 是 block 数组（或字符串），官方客户端注入后也是数组，
// 因此这里统一成数组形态，避免出现「数组 + 字符串」混合。
func normalizeUserSystem(system any) []any {
	switch v := system.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": v}}
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			switch block := item.(type) {
			case string:
				if strings.TrimSpace(block) != "" {
					out = append(out, map[string]any{"type": "text", "text": block})
				}
			case map[string]any:
				if stringField(block, "type") != "text" || strings.TrimSpace(stringField(block, "text")) == "" {
					continue
				}
				entry := map[string]any{"type": "text", "text": stringField(block, "text")}
				if cc, ok := block["cache_control"]; ok {
					entry["cache_control"] = cc
				}
				out = append(out, entry)
			}
		}
		return out
	}
	return nil
}

// applyCacheControl 给最后一条非 system 消息的末块加 ephemeral 缓存标记。
//
// 幂等：已有 cache_control 时不动。
func applyCacheControl(body map[string]any) {
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok || stringField(msg, "role") == "system" {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			msg["content"] = []any{map[string]any{
				"type":          "text",
				"text":          content,
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
		case []any:
			if len(content) == 0 {
				return
			}
			last, ok := content[len(content)-1].(map[string]any)
			if !ok || last["cache_control"] != nil {
				return
			}
			last["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		return
	}
}

// cloneBlock 深拷贝一个 system 块，避免调用方改动污染缓存的原型。
func cloneBlock(block any) any {
	raw, errMarshal := json.Marshal(block)
	if errMarshal != nil {
		return block
	}
	var cloned any
	if errUnmarshal := json.Unmarshal(raw, &cloned); errUnmarshal != nil {
		return block
	}
	return cloned
}
