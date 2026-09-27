// Package core 是本插件的供应商无关骨架：共享类型、供应商接口与注册表。
//
// 分层纪律：
//   - core 不 import 任何供应商包（internal/vendors/*），也不 import package main；
//   - 供应商特有逻辑（端点、请求头、指纹、模型目录、签到流程）必须实现在
//     internal/vendors/<name> 里，通过 Vendor 接口被根层调用；
//   - 根层（package main）是 ABI 适配层，负责把宿主 RPC 翻译成 Vendor 调用。
//
// 为什么需要这一层：宿主 ABI 限制一个插件进程只能声明一个 provider key
// （见 ProviderKey 的说明），因此多个供应商必须共存于一个插件内。若没有
// 这层抽象，每加一个供应商都要在根层的能力文件里改一处 switch。
package core

import (
	"context"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// ProviderKey 是本插件在 CPA 里占用的 provider 键，**所有供应商共用**。
//
// 宿主的 auth.identifier 只返回单个字符串、注册期调一次永久缓存，且与
// auth 文件的 type 字段严格相等才认领该文件（internal/pluginhost/auth_provider.go
// 的 authProviderRecord）。ModelsForAuth 也用同一个值匹配 auth.Provider，
// 不匹配即跳过（该账号不可用）。
//
// 因此供应商的区分落在**凭证文件名前缀**（workbuddycn-<uid>.json）与文件内的
// vendor 字段上，而不是 provider key。
const ProviderKey = "freetier"

// VendorKey 是凭证内标识供应商的字段名。
//
// 与文件名前缀冗余是刻意的：文件名让用户一眼看出归属，vendor 字段让判定
// 不依赖文件命名（用户手工改名后仍能正确归属）。
const VendorKey = "vendor"

// FormatChatCompletions 是本插件唯一声明支持的协议格式。
//
// 所有供应商统一只处理 chat-completions：Claude / Codex / Gemini 客户端由
// 宿主翻译成 chat-completions 再交给插件。插件重复实现协议转换会在两侧
// 产生不一致的分帧行为，且两套代码都要跟着上游协议变化维护。
const FormatChatCompletions = "chat-completions"

// Vendor 是一个供应商实例（= 供应商协议 × 区域）。
//
// 实例化在 internal/vendors/registry.go：2 个协议 × 2 个区域 = 4 个实例
// （workbuddycn / workbuddyglobal / qodercn / qoderglobal）。区域在**构造期**
// 固定，因此实现里不再按区域分支取端点。
//
// 约定：实现必须无状态且并发安全——宿主会并发调用这些方法。
type Vendor interface {
	// ID 是供应商实例标识，同时是凭证文件名前缀（workbuddycn 等）。
	ID() string
	// Name 是管理端展示名（如「WorkBuddy 国内版」）。
	Name() string
	// Region 是区域标识：cn / global。
	Region() string
	// ModelPrefix 是注册模型时的 ID 前缀，空表示用裸名。
	ModelPrefix() string

	// Match 判断一份凭证文件是否属于本实例。
	//
	// **必须偏严**：宿主会把所有非内建格式的凭证依次喂给插件，误吞别家的
	// 会让宿主用本插件的结构覆盖对方账号（且是静默的）。
	Match(fileName, provider string, raw map[string]any) bool
	// Parse 把凭证 JSON 解析成归一化凭证。
	Parse(raw []byte, fileName string) (*Credential, error)

	// StaticModels 返回本供应商的全部模型（供 /v1/models 展示）。
	StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error)
	// ModelsForAuth 返回某凭证可用的模型。
	//
	// 这是「凭证 ↔ 模型」绑定的执行点：只返回该凭证真正能用的模型，
	// 宿主据此在选号阶段淘汰不匹配的凭证。返回空清单会让宿主跳过该凭证。
	ModelsForAuth(ctx context.Context, cred *Credential) ([]pluginapi.ModelInfo, error)

	// Execute 执行一次非流式对话。
	Execute(ctx context.Context, cred *Credential, req *ExecuteRequest) (*pluginapi.ExecutorResponse, error)
	// ExecuteStream 执行一次流式对话，分片经 sink 投递给宿主。
	//
	// 实现只产出上游的原生分片内容（裸 SSE data 载荷），不做 `data: ` 包装
	// 与多协议分帧——那些由宿主按声明格式完成。
	ExecuteStream(ctx context.Context, cred *Credential, req *ExecuteRequest, sink StreamSink) error
	// CountTokens 估算一次请求的 token 数（宿主只当参考值）。
	CountTokens(ctx context.Context, cred *Credential, req *ExecuteRequest) (int, error)

	// Quota 查询账号额度。
	Quota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error)
	// SupportsCheckin 报告本实例是否提供签到（用于 UI 提前隐藏入口）。
	SupportsCheckin() bool
	// Checkin 执行一次签到。返回 nil 表示本实例不支持。
	Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error)

	// LoginStart 发起一次登录，返回用户需打开的授权链接。
	LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error)
	// LoginPoll 轮询一次登录状态。
	LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error)
	// Refresh 校验并刷新凭证。
	//
	// refreshed 报告上游是否真的下发了新凭证（false 表示仅校验通过，
	// 此时不应重写文件，否则 mtime 会无意义地变动）。
	Refresh(ctx context.Context, cred *Credential) (updated *Credential, refreshed bool, err error)

	// Tasks 返回本实例支持的任务动作（可为空）。
	Tasks() []Task
}

// ExecuteRequest 是一次对话执行的输入。
type ExecuteRequest struct {
	// Model 是剥掉供应商前缀后的裸模型名。
	Model string
	// Payload 是宿主翻译好的 chat-completions 请求体。
	Payload []byte
	// Headers 是客户端请求头。
	Headers map[string][]string
	// ClientIP 是客户端 IP（未开启透传时为空）。
	ClientIP string
	// Stream 报告是否流式。
	Stream bool
}

// StreamSink 是插件向宿主投递流式分片的出口（由根层实现）。
type StreamSink interface {
	// Emit 投递一个分片（裸 SSE 数据载荷，不含 `data: ` 前缀）。
	Emit(payload []byte) error
	// Close 结束流。errMsg 非空时作为错误帧投递给客户端。
	Close(errMsg string) error
}

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	// Already 报告本次是否为重复签到（幂等命中）。
	Already bool
	// Credit / Energy 是本次所得。
	Credit int64
	Energy int64
	// Message 是可读说明。
	Message string
}

// LoginSupport 由「是否有登录流程」需要被提前告知的供应商实现。
//
// 页面要在渲染「添加账号」下拉时就知道点哪一项会发起登录、点哪一项该提示
// 手填凭证——OpenCode ZEN 是纯 API key，点它发起登录只会拿到一个「不支持」。
//
// **刻意做成声明式而不是探测式**：探测的唯一办法是调一次 LoginStart，
// 而那对支持的供应商会真的向上游申请一个设备码（有副作用）。一个只读的
// 列表接口不该产生上游请求。
//
// 未实现该接口的供应商按「支持登录」处理：绝大多数供应商都有登录，
// 不实现的默认行为应当是最常见的那个。
type LoginSupport interface {
	// SupportsLogin 报告本供应商是否提供登录流程。
	SupportsLogin() bool
}

// AuthModeReporter 由「凭证本身就是敏感材料」的供应商实现。
//
// 目前只有 apikey 型供应商需要它：OAuth / 设备令牌账号展示的是昵称或邮箱
// （本就不敏感），而 API key 就是凭证本身——页面是明文 HTML，把完整 key
// 渲染进去等于把它写进浏览器缓存、截图和录屏里。
//
// 做成可选接口而不是塞进 Vendor：绝大多数供应商的凭证不是敏感材料，
// 不必被迫实现一个恒返回 "oauth" 的方法。
type AuthModeReporter interface {
	// AuthMode 返回凭证形态：apikey / oauth / device。
	AuthMode() string
}

// Task 是一个可执行的任务动作。
type Task struct {
	// Code 是任务标识（供应商内唯一）。
	Code string
	// Desc 是任务说明（供管理页展示）。
	Desc string
	// UsesChat 标记该任务是否消耗真实对话额度。
	UsesChat bool
}

// StorageMerger 由「刷新后需要把新凭证合并回原文件」的供应商实现。
//
// 为什么需要这个接口：各家的凭证 JSON 结构不同（WorkBuddy 是嵌套的
// auth/account，Qoder 是扁平字段，Cline 是 accessToken/refreshToken 平铺），
// 通用合并会把顶层字段写乱，而**只让一家实现**又会让其它家的续期永远无法
// 落盘（表现为「明明刷新成功了，重启后又变回旧 token」）。
//
// 做成可选接口而不是塞进 Vendor：不需要合并的供应商（如纯 API key 的
// OpenCodeZEN，凭证没有会变的字段）不必被迫实现一个恒等变换。
type StorageMerger interface {
	// MergeStorageJSON 把刷新后的凭证合并回 original，返回新的完整 JSON。
	//
	// credential 是刷新后的归一化凭证，实现应从它的 Native 取回协议层凭证。
	MergeStorageJSON(original []byte, credential *Credential) ([]byte, error)
}
