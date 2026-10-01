package main

// 本文件处理模型变更的「待生效」标记。
//
// 背景：禁用/启用与别名改的是**状态文件**，而宿主的模型注册表只在
// plugin.register / 配置重载时构建一次。因此：
//   - 调用拦截是**即时**的（executor 每次请求查内存里的禁用表与别名表）；
//   - 但 /v1/models 列表的增删要等宿主重建注册表——即重启 CPA。
//
// 所以变更发生时置一个标记，控制台页据此常驻提示用户重启；宿主重建注册表时
// （applyModelCatalog，即 plugin.register / reconfigure 路径）自动清除它。

// clearRestartPending 清除「待生效」标记。
//
// 由 applyModelCatalog 在宿主重建模型注册表时调用——那一刻变更已经生效。
func clearRestartPending() {
	if !snapshotState().RestartPending {
		return
	}
	mutateState(func(state *pluginState) {
		state.RestartPending = false
	})
}
