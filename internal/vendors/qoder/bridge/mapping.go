package bridge

import (
	"strings"
	"sync"
	"sync/atomic"
)

// 模型映射提供方 —— 由插件注入（YAML 配置 + 管理页设置），
// 让移植过来的 MapModel 不再依赖 qoder2api 的 settings.json。
//
// 注入的是"取值函数"而不是映射表本身：配置热更新后，后续请求立刻生效，
// 不需要重建 Bridge。
type ModelMappingProvider func() (agentTables map[string]map[string]string, flat map[string]string)

var modelMappingProvider atomic.Value // ModelMappingProvider

// dynamicModelMap 存储动态从上游探测到的模型名称（小写）到 Qoder 内部 key 的映射。
var dynamicModelMap sync.Map

// builtinNameKeyMap 是内置的已知通用模型名到 Qoder 上游内部 key 的兜底字典。
var builtinNameKeyMap = map[string]string{
	"auto":              "auto",
	"ultimate":          "ultimate",
	"performance":       "performance",
	"efficient":         "efficient",
	"qwen3.8-max":       "qmodel_38max",
	"qwen3.8-flash":     "qfmodel",
	"qwen3.7-max":       "qmodel_latest",
	"qwen3.7-plus":      "qmodel",
	"qwen3.6-flash":     "q36fmodel",
	"kimi-k3":           "kmodel_latest",
	"kimi-k2.8-preview": "kmodel",
	"kimi-k2.7-code":    "kmodel",
	"glm-5.3":           "gmodel",
	"glm-5.3-flash":     "gfmodel",
	"glm-5.2":           "gm51model",
	"glm-5.1":           "gm51model",
	"deepseek-v4-pro":   "dmodel",
	"deepseek-flash":    "dfmodel",
	"deepseek-v4-flash": "dfmodel",
	"minimax-m3":        "mmodel",
	"minimax-m2.7":      "mmodel",
}

func init() {
	for _, key := range builtinNameKeyMap {
		builtinNameKeyMap[strings.ToLower(key)] = key
	}
}

// RegisterKnownModels 注册一组已知模型条目，建立规范名称与内部 key 的映射。
func RegisterKnownModels(models []QoderModel) {
	for _, m := range models {
		key := strings.TrimSpace(m.Key)
		if key == "" {
			continue
		}
		// 1. 内部 key 自身小写直接映射为 key (如 "gfmodel" -> "gfmodel")
		dynamicModelMap.Store(strings.ToLower(key), key)

		// 2. DisplayName 小写映射为内部 key (如 "glm-5.3-flash" -> "gfmodel")
		if name := strings.TrimSpace(m.DisplayName); name != "" {
			dynamicModelMap.Store(strings.ToLower(name), key)
		}
	}
}

// ResolveQoderModelKey 将模型名称或标识符解析为 Qoder 上游所期望的内部 key。
// 若未命中内部映射则返回空字符串。
func ResolveQoderModelKey(model string) string {
	clean := strings.ToLower(strings.TrimSpace(model))
	if clean == "" {
		return ""
	}
	if val, ok := dynamicModelMap.Load(clean); ok {
		if s, okStr := val.(string); okStr && s != "" {
			return s
		}
	}
	if key, ok := builtinNameKeyMap[clean]; ok {
		return key
	}
	return ""
}

// SetModelMappingProvider 注册映射来源；传 nil 表示回到内置默认映射。
func SetModelMappingProvider(provider ModelMappingProvider) {
	if provider == nil {
		modelMappingProvider = atomic.Value{}
		return
	}
	modelMappingProvider.Store(provider)
}

func currentModelMappings() (map[string]map[string]string, map[string]string) {
	raw := modelMappingProvider.Load()
	if raw == nil {
		return nil, nil
	}
	provider, _ := raw.(ModelMappingProvider)
	if provider == nil {
		return nil, nil
	}
	agentTables, flat := provider()
	return agentTables, flat
}
