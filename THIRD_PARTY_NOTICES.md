# 第三方来源与许可说明

本插件（`freetier2api-plugin`）把多个免费额度供应商接入 CLIProxyAPI：WorkBuddy
（腾讯 CodeBuddy）与 Qoder。两家的协议实现与任务体系**均移植自各自的上游项目**，
不是从头重写。

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
| `internal/vendors/workbuddy/endpoints.go` | `internal/upstream/client.go` 的域名与路径常量 | 抽成按域索引的端点表 |
| `internal/vendors/workbuddy/errors.go` | `internal/upstream/client.go` 的 `Classify` 与关键词表 | 保留全部判定顺序与关键词；输出改为「Kind + HTTP 状态码」以便宿主处置 |
| `internal/vendors/workbuddy/headers.go` | `internal/upstream/headers.go` | 保留四类头与会话头族；去掉网关侧参数注入 |
| `internal/vendors/workbuddy/payload.go` | `internal/upstream/payload.go` | 保留改写管线的全部步骤与顺序 |
| `internal/vendors/workbuddy/thinking.go` | `internal/upstream/thinking.go` | 保留思维链注入与 `reasoning_content` 回填规则 |
| `internal/vendors/workbuddy/toolpair.go` | `internal/upstream/tool_pairing.go`、`truncation.go` | 保留重排、孤儿裁剪与截断丢弃逻辑 |
| `internal/vendors/workbuddy/sse.go` | `internal/upstream/sse.go` | 保留白名单重建与聚合；分帧改为「裸 JSON」（由宿主补 `data:` 与 `[DONE]`） |
| `internal/vendors/workbuddy/sanitize.go` | `internal/upstream/sanitize.go` | 规则外置为 `sanitize_rules.json`（便于审计改动过的字面量） |
| `internal/vendors/workbuddy/catalog*.go`、`catalog_seed.json` | `internal/upstream/model_catalog.go`、`global_models.go`、`context_catalog.go`、`effort_catalog.go` | 保留多路探测与四级查找链；静态表转为内嵌 JSON |
| `internal/vendors/workbuddy/growth.go`、`activity.go`、`desktop.go`、`report.go` | `internal/upstream/tasks.go`、`streak.go`、`travel.go`、`school.go`、`blackcat.go`、`desktop.go`、`report.go`、`trial.go`、`global_register.go` | 保留全部端点、事件链与节流参数 |
| `internal/vendors/workbuddy/tasks/` | `internal/panel/autotask.go`、`internal/scheduler/` | 保留 17 个任务的动作与依赖序、四套指纹、异步计分轮询与幂等规则 |
| `internal/vendors/workbuddy/prompt_default.md` | `internal/prompt/defaultprompt.md` | 原文件 |
| `auth_login.go` | `internal/panel/login.go`、`cmd/login` | OAuth 设备授权流程；改为返回宿主的 `AuthData` |
| `internal/vendors/workbuddy/credential.go` | `internal/auth/auth.go` | 保留双形态解析与加锁访问契约；写回改为「合并更新」以免丢失用户字段 |

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

## 3. qoder2api（Qoder 供应商实现）

- 上游仓库：https://github.com/Zhengyuuuui/qoder2api
- 二次开发上游：https://github.com/wangtufly/QCCG
- 许可：**GPL-3.0**（qoder2api 依据 QCCG 的开源协议二次开发）

本插件的 Qoder 供应商**移植自 qoder2api**：

| 本插件路径 | 上游对应 | 改造内容 |
| --- | --- | --- |
| `internal/vendors/qoder/cosy/` | `internal/cosy/` | 去掉账户存储耦合，保留签名、设备指纹、加密算法 |
| `internal/vendors/qoder/bridge/` | `internal/bridge/` | 保留 chat-completions 协议转换与 SSE 信封语义；出站 HTTP 改为走宿主桥 |
| `internal/vendors/qoder/qoderapi/endpoints.go` | 端点常量 | 抽成按区域索引的端点表 |
| `internal/vendors/qoder/template.go` + `baseprompt.json` | 同名文件 | 原文件（Qoder 上游要求的基础提示词） |
| `internal/vendors/qoder/checkin.go` | `checkin.go` | 签到流程与窗口判定；改为走宿主 HTTP 桥 |
| `internal/vendors/qoder/quota.go` | `quota.go` | 额度查询与归一 |
| `internal/vendors/qoder/login.go` | `auth_login.go` | PKCE 设备授权流程；改为返回宿主的 `AuthData` |

**删除的部分**（本插件统一只做 chat-completions，多协议由 CPA 翻译）：
上游的 `internal/bridge/claude.go`（Claude 协议）、`codex.go`（Codex 协议）、
多格式 SSE 分帧逻辑，以及零调用点的 `cn_probe.go`（它绕过宿主 HTTP 桥且污染 stdout）。

## 4. CLIProxyAPI-qoder2api-plugin（插件形态的参考）

- 仓库：https://github.com/Hkxtor/CLIProxyAPI-qoder2api-plugin
- 许可：**GPL-3.0**

本插件把它作为「CPA 插件应当长什么样」的参考实现，用于对齐插件侧的工程约定
（分层结构、`cpasdk/` vendored 副本、C ABI 桥的写法、宿主回调的并发准入与关闭排空、
控制台页复用管理密钥、出站 HTTP 双形态键名兼容）。

## 5. opencode2api（OpenCode ZEN 协议参考）

- 仓库参考：https://github.com/anomalyco/opencode 衍生之 `opencode2api`
- 作用：OpenCode ZEN 供应商实现参考

本插件中参考 opencode2api 的部分：

| 本插件路径 | 参考内容 | 说明 |
| --- | --- | --- |
| `internal/vendors/opencodezen/endpoints.go` | 上游基地址 `https://opencode.ai/zen` 与 User-Agent 规范、会话 ID 规范 | 伪装官方客户端与规范会话头 |
| `internal/vendors/opencodezen/catalog.go` | 动态拉取 `/v1/models` 模型列表 | 动态模型目录探测 |
| `internal/vendors/opencodezen/chat.go` | chat-completions 转发与请求头组装 | 纯 chat-completions 转发（裁剪非 chat 路径） |
| `internal/vendors/opencodezen/credential.go` | API Key 鉴权与前缀识别 | 静态 API Key 解析与脱敏 |

## 6. cline2api（Cline 登录与协议参考）

- 仓库参考：`cline2api`
- 作用：Cline 供应商实现参考

本插件中参考 cline2api 的部分：

| 本插件路径 | 参考内容 | 说明 |
| --- | --- | --- |
| `internal/vendors/cline/login.go` | WorkOS OAuth 2.0 设备授权码流程（RFC 8628） | 设备码申请、用户轮询授权、换取 Cline 令牌 |
| `internal/vendors/cline/refresh.go` | `/auth/refresh` 令牌刷新与过期解析 | 驼峰 `grantType: refresh_token` 自动续期 |
| `internal/vendors/cline/endpoints.go` | 上游基地址 `https://api.cline.bot/api/v1` 与官方客户端 UA / 请求头 | 完整复刻 Cline 客户端请求头 |
| `internal/vendors/cline/catalog.go` | 免认证推荐模型目录 `recommended-models` 探测 | 分离 free 与 clinePass 计费档模型 |
| `internal/vendors/cline/credential.go` | `workos:` 前缀注入与持久化管理 | 规范出站 Bearer 令牌与双格式兼容 |

## 7. zcode2api（ZCode 协议与网关参考）

- 仓库参考：https://github.com/anomalyco/zcode2api 衍生之 `zcode2api`
- 作用：ZCode (Z.AI) 供应商实现参考

本插件中参考 zcode2api 的部分：

| 本插件路径 | 参考内容 | 说明 |
| --- | --- | --- |
| `internal/vendors/zcode/endpoints.go` | 上游基地址 `https://zcode.z.ai` 与 `https://api.z.ai` 常量 | 规范端点与 UA 标识 |
| `internal/vendors/zcode/login.go` | `/api/v1/oauth/cli/init` 与 `/poll` CLI 授权 | OAuth 授权与凭证换取 |
| `internal/vendors/zcode/chat.go` | OpenAI ↔ Anthropic Messages 协议双向转换 | 规范 chat-completions 转换转发 |
| `internal/vendors/zcode/quota.go` | `/api/v1/zcode-plan/billing/balance` 余额查询 | Plan 余额与配额获取 |

## 8. trae2api-more（TRAE SOLO 协议参考）

- 仓库参考：https://github.com/Sliverkiss/traework2api 衍生之 `trae2api-more`
- 作用：TRAE SOLO 供应商实现参考

本插件中参考 trae2api-more 的部分：

| 本插件路径 | 参考内容 | 说明 |
| --- | --- | --- |
| `internal/vendors/traesolo/endpoints.go` | `https://trae-api-cn.mchost.guru` 与 AppID / 常量 | 规范 SOLO 端点与指纹 |
| `internal/vendors/traesolo/chat.go` | `llm_utils_chat` 请求与 SSE 增量转换 | 改写 function="solo_work_lite" 与流式解析 |
| `internal/vendors/traesolo/checkin.go` | 签到 claim 接口与 16 位稳定设备 ID 算法 | SHA256 账号派生设备号与 9074 退避重试 |
| `internal/vendors/traesolo/quota.go` | `ide_user_ent_usage` 额度查询 | 账号积分与用量统计 |
| `internal/vendors/traesolo/refresh.go` | `ExchangeToken` 令牌轮换 | 自动刷新与 StorageJSON 嵌套合并 |

## 9. trae2api（Trae 国内版 / 国际版参考）

- 仓库参考：`trae2api`
- 作用：Trae 客户端（非 SOLO 版）国内版与国际版多区域实现参考

本插件中参考 trae2api 的部分：

| 本插件路径 | 参考内容 | 说明 |
| --- | --- | --- |
| `internal/vendors/trae/endpoints.go` | 国内版 (`cn`) 与国际版 (`global`/`sg`) 双部署配置 | 区分 chatHost / authHost / IDEVersion |
| `internal/vendors/trae/chat.go` | `chat_v3` 聊天通道转发与流式处理 | 规范客户端请求头与多区域路由 |
| `internal/vendors/trae/catalog.go` | 区域专属模型列表定义 | 严格区分国内/海外不同模型清单 |
| `internal/vendors/trae/refresh.go` | 多区域 ExchangeToken 令牌轮换 | 自动续期与持久化 |

## 10. tabbit2api（Tabbit 网关参考）

- 仓库参考：`tabbit2api`
- 作用：Tabbit 供应商模型与网关转发参考

本插件中参考 tabbit2api 的部分：

| 本插件路径 | 参考内容 | 说明 |
| --- | --- | --- |
| `internal/vendors/tabbit/endpoints.go` | 端点与本地网关地址配置 | 规范请求路由 |
| `internal/vendors/tabbit/catalog.go` | 智能优选通道与优先级模型路由表 | 注册 `tabbit/priority` 与全套主流模型 |
| `internal/vendors/tabbit/chat.go` | OpenAI 格式直连转发 | 快速直发与流式透传 |

## 11. codearts2api（CodeArts 华为云盘古助手参考）

- 仓库参考：`codearts2api`
- 作用：华为云 CodeArts Agent 供应商实现参考

本插件中参考 codearts2api 的部分：

| 本插件路径 | 参考内容 | 说明 |
| --- | --- | --- |
| `internal/vendors/codearts/chat.go` | 华为云 `SDK-HMAC-SHA256` 算法签名与 OpenAI 原生转发 | 纯 Go 零依赖请求签名与鉴权 |
| `internal/vendors/codearts/login.go` | 华为云 OAuth2 (Ticket + PKCE + DPoP) 流程 | Ticket 申请、轮询与 STS 凭证换取 |
| `internal/vendors/codearts/refresh.go` | 基于 ES256 P-256 签名的 DPoP 证明与 refresh_token 轮换 | 规范 DPoP 生成与 STS 令牌自动续期 |
| `internal/vendors/codearts/catalog.go` | 内置模型探测与限时福利模型目录 | 区分内置与限时福利模型 |
| `internal/vendors/codearts/checkin.go` | 限时免费福利套餐领取接口 | 自动领取活动配额 |

## 12. 本插件的许可

**GPL-3.0**。WorkBuddy 部分源自 MIT 的 `workbuddy2api-panel`（可并入 GPL），
Qoder 部分源自 **GPL-3.0** 的 `qoder2api` / QCCG，因此整体必须以 GPL-3.0 分发。
