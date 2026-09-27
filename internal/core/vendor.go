// Package core 是本插件的通用核心层：它承载所有供应商共用的骨架，
// 且**完全不知道任何具体供应商**。
//
// 分层纪律：
//   - core 只依赖 cpasdk（宿主契约）与 internal/httpx（出站 HTTP 桥）；
//   - 任何供应商特有的协议细节（端点、请求头、指纹、模型目录、签到流程）
//     都必须实现在 internal/vendors/<name> 里，通过 Vendor 接口被 core 调用；
//   - core 绝不 import vendors，避免循环依赖。
//
// 为什么需要这一层：宿主 ABI 限制一个插件进程只能声明一个 provider key
// （见 main.go 的 providerKey 注释），因此多个供应商必须共存于一个插件内。
// 若没有这层抽象，每加一个供应商都要在根目录的能力文件里改 switch。
package core

import (
	"context"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// ProviderKey 是本插件在 CPA 里占用的 provider 键。
//
// **所有供应商共用它**：宿主的 auth.identifier 只返回单个字符串、注册期调一次
// 永久缓存，且与 auth 文件的 type 字段严格相等才认领该文件。因此供应商的区分
// 落在凭证文件名前缀与文件内的 vendor 字段上，而不是 provider key。
const ProviderKey = "freetier"

// FormatChatCompletions 是本插件唯一声明支持的协议格式。
//
// 所有供应商统一只处理 chat-completions：Claude / Codex / Gemini 客户端由宿主
// 翻译成 chat-completions 再交给插件。插件不重复做协议转换——重复实现会在
// 宿主与插件两侧产生不一致的分帧行为，且两套代码都要跟着上游协议变化维护。
const FormatChatCompletions = "chat-completions"

// Vendor 是一个供应商实例（= 供应商 × 区域）。
//
// 实例化在 internal/vendors/registry.go：2 个供应商包 × 2 个区域 = 4 个实例。
// 每个实例自带区域信息，因此实现里不需要再按区域分支取端点。
//
// 约定：实现必须是**无状态且并发安全**的——宿主会并发调用这些方法。
type Vendor interface {
	// ID 是供应商实例标识，同时是凭证文件名前缀。
	//
	// 取值：workbuddycn / workbuddyglobal / qodercn / qoderglobal。
	// 它被写入凭证的 vendor 字段，也是模型禁用键与 UI 分组键。
	ID() string
	// Name 是管理端展示名（如「WorkBuddy 国内版」）。
	Name() string
	// Region 是区域标识：cn / global。
	Region() string
	// ModelPrefix 是注册模型时的 ID 前缀，空表示用裸名。
	//
	// 当前所有供应商都用裸名（跨供应商同名模型由宿主按 EqualFold 合并）。
	ModelPrefix() string

	// ---- 凭证 ----

	// Match 判断一份凭证文件是否属于本供应商。
	//
	// **必须偏严**：宿主会把所有非内建格式的凭证依次喂给插件，误吞别家凭证
	// 会让宿主用本插件的结构覆盖对方的账号（且是静默的）。fileName 是文件名
	// （含 .json），provider 是宿主从文件 type 字段推出的提示。
	Match(fileName, provider string, raw map[string]any) bool
	// Parse 把凭证 JSON 解析成归一化凭证。fileName 用于补齐缺失的标识。
	Parse(raw []byte, fileName string) (*Credential, error)

	// ---- 模型 ----

	// StaticModels 返回本供应商的全部模型（供 /v1/models 展示）。
	StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error)
	// ModelsForAuth 返回某个凭证可用的模型。
	//
	// 这是「凭证 ↔ 模型」绑定的执行点：只返回该凭证真正能用的模型，
	// 宿主据此在选号阶段淘汰不匹配的凭证（ClientSupportsModel）。
	// 返回空清单会让宿主跳过该凭证。
	ModelsForAuth(ctx context.Context, cred *Credential) ([]pluginapi.ModelInfo, error)

	// ---- 执行 ----

	// Execute 执行一次非流式对话。
	Execute(ctx context.Context, cred *Credential, req *ExecuteRequest) (*pluginapi.ExecutorResponse, error)
	// ExecuteStream 执行一次流式对话，分片通过 sink 投递给宿主。
	//
	// 实现只负责产出上游的原生分片内容（裸 SSE data 载荷），
	// 不做 `data: ` 包装与多协议分帧——那些由宿主按声明格式完成。
	ExecuteStream(ctx context.Context, cred *Credential, req *ExecuteRequest, sink StreamSink) error

	// ---- 额度与签到 ----

	// Quota 查询账号额度。
	Quota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error)
	// Checkin 执行一次签到。返回 nil 表示本供应商/区域不支持签到。
	Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error)
	// SupportsCheckin 报告本供应商实例是否提供签到。
	//
	// 与 Checkin 分开是为了让 UI 能在调用前就隐藏入口，
	// 而不是让用户点了才看到「不支持」。
	SupportsCheckin() bool

	// ---- 登录与续期 ----

	// LoginStart 发起一次登录，返回用户需要打开的授权链接。
	LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error)
	// LoginPoll 轮询一次登录状态。
	LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error)
	// Refresh 校验并刷新凭证。
	//
	// refreshed 报告上游是否真的下发了新凭证（false 表示仅校验通过，
	// 此时不应重写文件，否则会让 mtime 无意义地变动）。
	Refresh(ctx context.Context, cred *Credential) (updated *Credential, refreshed bool, err error)

	// ---- 任务与扩展 ----

	// Tasks 返回本供应商支持的任务动作（可为空）。
	Tasks() []Task
	// Routes 返回本供应商自注册的管理路由。
	//
	// 供应商特有接口（签到、做任务、开学季…）走这里注册，而不是在根层
	// 堆一个越来越长的 switch——新增供应商不需要改 core。
	Routes() []Route
	// Actions 返回控制台页要渲染的动作按钮。
	Actions() []Action
}

// ExecuteRequest 是一次对话执行的输入。
type ExecuteRequest struct {
	// Model 是剥掉供应商前缀后的裸模型名。
	Model string
	// Payload 是宿主翻译好的 chat-completions 请求体。
	Payload []byte
	// Headers 是客户端请求头（用于透传 IP、会话头族等）。
	Headers map[string][]string
	// ClientIP 是从 Headers 提取的客户端 IP（未开启透传时为空）。
	ClientIP string
	// Stream 报告是否流式。
	Stream bool
}

// StreamSink 是插件向宿主投递流式分片的出口。
//
// 实现由根层提供（包装 host.stream.emit / host.stream.close），
// 供应商只负责产出分片内容。
type StreamSink interface {
	// Emit 投递一个分片。payload 是裸 SSE 数据载荷（不含 `data: ` 前缀）。
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

// Task 是一个可执行的任务动作。
type Task struct {
	// Code 是任务标识（供应商内唯一）。
	Code string
	// Desc 是任务说明（供管理页展示）。
	Desc string
	// UsesChat 标记该任务是否消耗真实对话额度。
	UsesChat bool
}

// Route 是供应商自注册的管理路由。
type Route struct {
	Method string
	Path   string
	// Description 供管理端展示。
	Description string
	// Handle 处理请求。req 已由 core 解好，返回体直接序列化为 JSON。
	Handle func(ctx context.Context, req *ManagementRequest) (any, error)
}

// Action 是控制台页的一个动作按钮。
//
// 页面按这些描述动态渲染按钮，因此新增供应商不需要改 HTML 模板。
type Action struct {
	// ID 是按钮标识（页面用于绑定事件）。
	ID string
	// Label 是按钮文字。
	Label string
	// VendorID 限定该动作适用的供应商实例（空表示全部）。
	VendorID string
	// Path 是动作请求的管理路径（相对插件前缀，如 /checkin）。
	Path string
	// Method 是 HTTP 方法。
	Method string
	// Primary 标记主按钮样式。
	Primary bool
}

// ManagementRequest 是 core 解好的管理请求，传给供应商路由。
type ManagementRequest struct {
	Method  string
	Path    string
	Query   map[string][]string
	Body    []byte
	Headers map[string][]string
	// CallbackID 是宿主回调 ID（供应商发起出站请求时要带上）。
	CallbackID string
}
