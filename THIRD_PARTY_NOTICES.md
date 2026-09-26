# 第三方来源与许可说明

本插件（`workbuddy2api-plugin`）是把 [workbuddy2api](https://github.com/DGZSbot/workbuddy2api) 的
能力打包成 CLIProxyAPI 原生动态库插件。协议实现与任务体系**移植自该项目**，不是从头重写。

## 1. workbuddy2api（上游业务实现）

- 仓库：https://github.com/DGZSbot/workbuddy2api
- 许可：**MIT**

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
| `internal/cb/catalog*.go`、`catalog_seed.json`、`model.json` | `internal/upstream/model_catalog.go`、`global_models.go`、`context_catalog.go`、`effort_catalog.go`、`model.json` | 保留多路探测与四级查找链；静态表转为内嵌 JSON |
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

## 3. 本插件的许可

MIT。移植部分沿用上游的 MIT 条款。
