package bridge

import "sync/atomic"

// 模型映射提供方 —— 由插件注入（YAML 配置 + 管理页设置），
// 让移植过来的 MapModel 不再依赖 qoder2api 的 settings.json。
//
// 注入的是"取值函数"而不是映射表本身：配置热更新后，后续请求立刻生效，
// 不需要重建 Bridge。
type ModelMappingProvider func() (agentTables map[string]map[string]string, flat map[string]string)

var modelMappingProvider atomic.Value // ModelMappingProvider

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
