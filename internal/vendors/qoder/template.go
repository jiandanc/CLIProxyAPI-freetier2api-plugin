package qoder

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"freetier2api-plugin/internal/vendors/qoder/cosy"
)

// baseprompt.json 是 Qoder 上游 agent_chat_generation 的请求模板（移植自 qoder2api）。
// 模板里保留了 {UUIDn}/{TIME1} 占位符：每次建 Bridge 时替换为实际值再解析成 JSON。
//
//go:embed baseprompt.json
var basePromptRaw []byte

// TemplateBase 构造一次请求模板。
//
// 与 qoder2api 的差异：这里对解析失败返回错误而不是静默忽略。模板解析失败意味着
// 所有上游请求都会失真，静默继续会让问题表现成"模型答非所问"这种难以定位的现象。
func TemplateBase() (map[string]interface{}, error) {
	template := string(basePromptRaw)
	for _, placeholder := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
		template = strings.ReplaceAll(template, placeholder, cosy.NewUUID())
	}
	template = strings.ReplaceAll(template, "{TIME1}", fmt.Sprintf("%d", cosy.UnixMs()))

	var templateBase map[string]interface{}
	if errUnmarshal := json.Unmarshal([]byte(template), &templateBase); errUnmarshal != nil {
		return nil, fmt.Errorf("baseprompt.json 模板解析失败（内嵌资源损坏或占位符未替换）: %w", errUnmarshal)
	}
	if templateBase == nil {
		return nil, fmt.Errorf("baseprompt.json 模板为空")
	}
	return templateBase, nil
}
