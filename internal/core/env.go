package core

import "context"

// Env 是 core 提供给供应商实现的宿主与插件能力。
//
// 为什么走注入而不是让供应商直接 import 根包：根包是 ABI 适配层（package main），
// 供应商包 import 它会成环。根层在启动时构造一次并传给每个 Vendor 实例。
//
// 刻意保持窄：只暴露供应商真正需要的能力，避免 core 变成一个什么都往里塞的
// 上帝对象。新增能力时先问「供应商自己能不能做」，能就不加。
type Env interface {
	// HostAuthGet 按 auth_index 或 auth_id 取回凭证原始 JSON 与物理路径。
	//
	// 宿主的 host.auth.get 只认 auth_index；传 id 时由实现负责先映射。
	// ok 为 false 表示查不到（凭证已删或被别的插件取走）。
	HostAuthGet(ctx context.Context, callbackID, authID string) (raw []byte, path string, ok bool)

	// HostAuthList 列出本插件名下的全部凭证。
	HostAuthList(ctx context.Context, callbackID string) ([]HostAuth, error)

	// HostAuthSave 把凭证写回宿主的物理 auth 文件。
	//
	// 优先用它而不是直接写文件：宿主会据此热更新内存注册表，
	// 只写文件会让内存态与磁盘态不一致（表现为「续期了但还报凭证失效」）。
	HostAuthSave(ctx context.Context, callbackID, fileName string, payload []byte) error

	// Logf 输出插件日志。
	Logf(level LogLevel, format string, args ...any)
}

// LogLevel 是日志级别（与 internal/logger 对齐）。
type LogLevel int

const (
	// LogDebug 是调试级别。
	LogDebug LogLevel = iota
	// LogInfo 是信息级别。
	LogInfo
	// LogError 是错误级别。
	LogError
)

// HostAuth 是宿主凭证列表里的一条（pluginapi.HostAuthFileEntry 的子集）。
type HostAuth struct {
	ID          string
	AuthIndex   string
	Name        string
	Provider    string
	Label       string
	Status      string
	Disabled    bool
	Unavailable bool
	RuntimeOnly bool
	Path        string
	// ModTime 是凭证文件最后修改时间（RFC3339），用于回答「多久没换过 token」。
	ModTime string
}
