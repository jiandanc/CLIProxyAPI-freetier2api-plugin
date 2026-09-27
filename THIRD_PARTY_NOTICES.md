# 第三方来源与许可说明

本插件（`workbuddy2api-plugin`）是把 **workbuddy2api** 的能力打包成 CLIProxyAPI 原生动态库插件。
协议实现与任务体系**移植自该项目**，不是从头重写。

## 1. workbuddy2api（上游业务实现）

本插件的移植基准是下面这个 fork（`workbuddy2api-panel`），它本身又是
[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的增强分支：

- 移植基准：https://github.com/linguo2625469/workbuddy2api-panel
- 原始项目：https://github.com/Sliverkiss/workbuddy2api
- 许可：**MIT**（两个项目均为 MIT）

> 本插件移植的是 **`workbuddy2api-panel` 的代码**（`internal/upstream/`、`internal/scheduler/`、
> `internal/panel/autotask.go`、`internal/auth/` 等），因此下面表格里的「上游对应」列
> 指的是该 fork 的路径。原始项目 `Sliverkiss/workbuddy2api` 提供账号池调度、错误分类、
> 提示词体系等基础设计，fork 在其上补了面板与任务体系——两者都是本插件的来源。

本插件中源自/改造自 workbuddy2api 的部分：

| 本插件路径 | 上游对应 | 改造内容 |
| --- | --- | --- |
| `internal/cb/endpoints.go` | `internal/upstream/client.go` 的域名与路径常量 | 抽成按域索引的端点表 |
| `internal/cb/errors.go` | `internal/upstream/client.go` 的 `Classify` 与关键词表 | 保留全部判定顺序与关键词；输出改为「Kind + HTTP 状态码」以便宿主处置 |
| `internal/cb/headers.go` | `internal/upstream/headers.go` | 保留四类头与会话头族；去掉网关侧参数注入 |
| `internal/cb/payload.go` | `internal/upstream/payload.go` | 保留改写管线的全部步骤与顺序 |
| `internal/cb/thinking.go` | `internal/upstream/thinking.go` | 保留思维链注入与 `reasoning_content` 回填规则 |
| `internal/cb/toolpair.go` | `internal/upstream/tool_pairing.go`、`truncation.go` | 保留重排、孤儿裁剪与截断丢弃逻辑 |
| `internal/cb/sse.go` | `internal/upstream/sse.go` | 保留白名单重建与聚合；分帧改为「裸 JSON」（由宿主补 `data:` 与 `[DONE]`） |
| `internal/cb/sanitize.go` | `internal/upstream/sanitize.go` | 规则外置为 `sanitize_rules.json`（便于审计改动过的字面量） |
| `internal/cb/catalog*.go`、`catalog_seed.json` | `internal/upstream/model_catalog.go`、`global_models.go`、`context_catalog.go`、`effort_catalog.go` | 保留多路探测与四级查找链；静态表转为内嵌 JSON |
| `internal/cb/growth.go`、`activity.go`、`desktop.go`、`report.go` | `internal/upstream/tasks.go`、`streak.go`、`travel.go`、`school.go`、`blackcat.go`、`desktop.go`、`report.go`、`trial.go`、`global_register.go` | 保留全部端点、事件链与节流参数 |
| `internal/tasks/` | `internal/panel/autotask.go`、`internal/scheduler/` | 保留 17 个任务的动作与依赖序、四套指纹、异步计分轮询与幂等规则 |
| `prompt_default.md` | `internal/prompt/defaultprompt.md` | 原文件 |
| `auth_login.go` | `internal/panel/login.go`、`cmd/login` | OAuth 设备授权流程；改为返回宿主的 `AuthData` |
| `internal/cb/credential.go` | `internal/auth/auth.go` | 保留双形态解析与加锁访问契约；写回改为「合并更新」以免丢失用户字段 |

## 2. CLIProxyAPI（宿主 ABI）

- 仓库：https://github.com/router-for-me/CLIProxyAPI
- 许可：**MIT**

本插件中源自/对齐宿主的部分：

| 本插件路径 | 上游对应 | 改造内容 |
| --- | --- | --- |
| `cpasdk/pluginabi/types.go` | `sdk/pluginabi/types.go` | 逐字复制（ABI 方法名、Schema 版本、信封与错误结构） |
| `cpasdk/pluginapi/types.go` | `sdk/pluginapi/types.go` | 逐字复制（注册、模型、auth、executor、quota、management 的 RPC 结构） |
| `internal/httpx/` | `internal/pluginhost/` 的 host HTTP 桥协议 | 按宿主契约重写为插件侧客户端 |
| `bridge.h` / `bridge.c` | `internal/pluginhost/loader_*.go` 的 C ABI 声明 | 按宿主 ABI 实现插件侧的函数表桥接 |

`cpasdk` 是**对齐宿主契约的本地副本**，不是 import 宿主的内部包（内部包不可外部导入）。
宿主升级若改动 ABI/JSON 契约，需要同步这里。该目录豁免 `gofmt` 检查，以保持与上游一致的排版。

## 3. CLIProxyAPI-qoder2api-plugin（插件形态的参考实现）

- 仓库：https://github.com/Hkxtor/CLIProxyAPI-qoder2api-plugin
- 许可：**GPL-3.0**（该项目自身声明）

本插件**不包含**该项目的任何代码，仅把它作为「CPA 插件应当长什么样」的参考实现，
用于对齐插件侧的工程约定：

| 借鉴的约定 | 说明 |
| --- | --- |
| 分层结构 | 根目录 = ABI 适配层 + 能力实现层；`internal/*` 为纯逻辑层，对配置/宿主的能力走函数注入，不反向 import main |
| `cpasdk/` vendored 副本 | 从宿主 SDK 复制 ABI/契约类型并豁免 gofmt，避免依赖相邻仓库 |
| C ABI 桥的写法 | `//export` 的 preamble 只放声明，C 函数定义放独立的 `.c`/`.h`（cgo 会把 preamble 复制进多个生成文件，带定义会重复符号） |
| 宿主回调的并发准入与关闭排空 | 宿主回调不可取消，需限流 + Shutdown 时等待排空，否则 dlopen 卸载会崩 |
| 控制台页复用管理密钥 | 与 CPA 面板同源，可解码其 localStorage 中混淆过的 `managementKey`，免去重复输入 |
| 出站 HTTP 双形态键名兼容 | 宿主 `host.http.do` 回的 `pluginapi.HTTPResponse` 无 json tag，键名是 Go 字段名，必须兼容 snake_case |

> 该项目的 GPL-3.0 不传染到本插件：以上是**思路与工程约定**的借鉴，没有复制其代码。
> 若后续需要搬运其具体实现，必须先按 GPL-3.0 重新评估本插件的许可。

## 4. 本插件的许可

MIT。移植部分沿用上游（`workbuddy2api-panel` 与 `Sliverkiss/workbuddy2api`）的 MIT 条款。
