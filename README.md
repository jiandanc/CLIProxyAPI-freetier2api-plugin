# freetier2api-plugin

把**多个免费额度供应商**接入 **CLIProxyAPI**（下称 CPA）的原生动态库插件。

插件通过宿主的 `cliproxy_plugin_init` ABI 加载，一次性声明五类能力，
宿主的路由、鉴权、调度、日志与代理全部复用——**不需要再单独跑一个网关服务**：

| 能力 | 作用 |
| --- | --- |
| `auth_provider` | 识别并加载各家凭证，支持设备授权登录与定时刷新 |
| `model_provider` | 注册模型（裸名，跨供应商同名合并），清单从上游动态探测 |
| `executor` | 转发 chat-completions（含流式）；Claude / Codex 客户端由宿主翻译 |
| `quota_provider` | 查询账号额度，供管理端展示 |
| `management` | 自有管理接口 + 内嵌控制台页（账号、模型、任务、设置、日志） |

## 支持的供应商

| 供应商 ID | 展示名 | 凭证文件名前缀 |
| --- | --- | --- |
| `workbuddycn` | WorkBuddy 国内版 | `workbuddycn-*.json` |
| `workbuddyglobal` | WorkBuddy 国际版 | `workbuddyglobal-*.json` |
| `qodercn` | Qoder 国内版 | `qodercn-*.json` |
| `qoderglobal` | Qoder 国际版 | `qoderglobal-*.json` |
| `opencodezen` | OpenCode ZEN | `opencodezen-*.json` |
| `cline` | Cline | `cline-*.json` |

## 架构：按供应商插桩

宿主 ABI 有一个硬性约束：**一个插件进程只能声明一个 provider key**
（`auth.identifier` 返回单个字符串、注册期调一次永久缓存，且与 auth 文件的
`type` 字段严格相等才认领该文件）。因此本插件用统一的 `providerKey = "freetier"`，
**供应商的区分落在凭证文件名前缀与文件内的 `vendor` 字段上**。

代码分三层：

```
package main            ABI 适配层：把宿主 RPC 翻译成 Vendor 调用
  ├─ vendor_workbuddy.go   WorkBuddy 的 core.Vendor 实现（两个区域实例）
  ├─ vendor_qoder.go       Qoder 的 core.Vendor 实现（两个区域实例）
  ├─ vendor_opencodezen.go OpenCode ZEN 的 core.Vendor 实现
  └─ vendor_cline.go       Cline 的 core.Vendor 实现

internal/core           供应商无关的骨架：Vendor 接口、供应商注册表、
                        共享 Credential、模型 ID 协议、信封、错误

internal/vendors/<name> 纯协议层：各供应商目录结构严格对齐（catalog.go、
                        chat.go、checkin.go、credential.go、endpoints.go、
                        errors.go、login.go、quota.go、refresh.go）。
                        出站 HTTP 一律经 internal/httpx 走宿主桥。
```

新增一个供应商只需实现 `core.Vendor` 并在 `init` 里注册，**不必改 ABI 适配层的
任何 switch**——根层的 auth / model / executor / quota / management 五类处理器
全部经 `core.Vendor` 分发。

### 凭证归属判定

`core.ResolveVendor` 按证据由强到弱判定：

1. **凭证内的 `vendor` 字段** —— 插件自己写的，最可靠；
2. **文件名前缀** —— 用户可读，且登录落盘时就按供应商命名；
3. **各供应商自己的 `Match`** —— 内容嗅探兜底（兼容手写/改名凭证）。

刻意不用「第一个认领的就收下」：供应商之间的结构有重叠（都可能有 token 字段），
靠遍历顺序决定归属会让结果随注册顺序漂移。

**归属判定必须偏严**：宿主会把所有非内建格式的凭证依次喂给每个插件，误吞别家的
会让宿主用本插件的结构覆盖对方账号（且是静默的）。

## 特性

- **供应商隔离**：每个凭证只注册它自己供应商的模型，宿主在选号阶段据此淘汰
  不匹配的凭证——插件不需要自己实现任何调度逻辑。
- **模型清单动态探测**：WorkBuddy 国内版两路（`/v3/config` + 企业目录）、
  国际版三路（v3 的 IDE UA + CLI UA + 企业目录），Qoder 走自己的模型列表接口。
- **额度查询**：各家的额度口径不同（WorkBuddy 是积分余额，Qoder 是套餐用量桶），
  在适配层统一折算成「剩余 / 总额」供页面展示。
- **签到与任务**：WorkBuddy 国内版支持每日签到、连登兑换、抽奖、猫猫旅行、
  夜猫子、活跃上报、token 保活，以及成长任务一键完成（17 个任务）与开学季活动；
  Qoder 国内版支持每日签到。
- **提示词防御与脱敏**（WorkBuddy）：系统提示词三模式（透传 / 替换 / 追加）
  + 出站请求体指纹脱敏，两层叠加降低上游内容审核拦截率。
- **敏感凭证脱敏与操作管理**：API key 类型账号在列表展示前4位与后4位、中间脱敏为 `***`，避免明文泄露；账号概览最后一列提供「操作」列，支持「删除」按钮直接删除本地凭证文件（带二次确认），宿主基于文件监听自动注销失效账号。
- **零第三方依赖**：`go.mod` 只有 module 与 go 版本两行，构建不需要拉包。

## 构建

需要一个 C 编译器（cgo）与 Go 1.22+。本仓库的 `build.sh` 经 Docker 构建
（本机可没有 Go 工具链）：

```bash
./build.sh          # 先跑 go test ./...，再产出 dist/freetier2api.<ext>
```

> **必须用 glibc 镜像构建**：宿主 CPA 镜像是 Debian(glibc)，用 alpine(musl)
> 构建出的 `.so` 会以 `libc.musl-aarch64.so.1: cannot open shared object file`
> 加载失败。`build.sh` 已固定用 `golang:1.24`（Debian 版，自带 gcc）。

产物按平台命名：Linux `freetier2api.so`、macOS `freetier2api.dylib`、Windows `freetier2api.dll`。

发行版号由 CI 用 `-ldflags -X main.pluginVersion=<tag>` 注入，本地构建保留默认值。

## 安装

### 通过 CPA 插件商店（推荐）

在 CPA 的 `config.yaml` 里把本仓库的 `registry.json` 加为三方源：

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/jiandanc/CLIProxyAPI-freetier2api-plugin/main/registry.json"
```

重启 CPA → 管理面板 → 插件商店 → 搜 **FreeTier 2API** → 一键安装。

### 手动安装

把产物放进 CPA 的插件目录，**文件名必须是 `freetier2api.<ext>`**
（宿主按 `{pluginID}{ext}` 找动态库）：

```yaml
plugins:
  enabled: true
  dir: "/path/to/cpa/plugins"
  configs:
    freetier2api:
      enabled: true
      enabled_realms: "cn,global"
```

## 添加账号

两种方式，在 CPA 的 auth 目录里产出一个 `<vendor>-<uid>.json` 凭证文件：

1. **控制台页登录**（推荐）：打开
   `http://<CPA>/v0/resource/plugins/freetier2api/console` →
   「添加账号 ▾」→ 从下拉里选供应商 → 在浏览器里完成设备授权。
2. **手写凭证**：放进 CPA 的 auths 目录。

**文件名必须以供应商 ID 开头**（如 `workbuddycn-myaccount.json`、
`qodercn-login-abc.json`），否则插件只能靠内容嗅探归属。

也可以在文件里加两个显式字段，这样文件名就不受限制：

```json
{
  "type": "freetier",
  "vendor": "workbuddycn"
}
```

- `type` 必须等于本插件的 provider key（`freetier`），宿主靠它把文件交给本插件；
- `vendor` 是供应商实例标识（见上面的支持表），是归属判定最强的证据。

### WorkBuddy 凭证字段

嵌套形（OAuth 登录产物）：

```json
{
  "auth": {"accessToken": "...", "refreshToken": "...", "expiresAt": 1753600000,
           "domain": "www.codebuddy.cn", "realm": "cn"},
  "account": {"uid": "...", "nickname": "我的账号"},
  "device_token": "..."
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `auth.accessToken` | 是 | 出站 Bearer 令牌 |
| `auth.refreshToken` | **强烈建议** | 宿主靠它驱动自动刷新；缺失时账号不会被续期 |
| `auth.expiresAt` | 否 | Unix 秒；缺失时插件首次刷新后自动补上 |
| `auth.domain` | 否 | 出站 `X-Domain`；也用于推断域 |
| `auth.realm` | 否 | `cn` / `global`；缺失时按 `domain` 推断 |
| `account.uid` | 建议 | 设备指纹的派生种子；缺失会导致指纹不稳定 |
| `device_token` | 否 | `X-Device-Token`；桌面端凭证需要 |

扁平形（顶层直接放 `accessToken` / `refreshToken` / `uid` / `domain`）也支持。

### Qoder 凭证字段

```json
{
  "device_token": "dt-...",
  "refresh_token": "drt-...",
  "region": "cn",
  "label": "我的账号",
  "email": "..."
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `device_token` | 是 | 设备令牌（`dt-` 前缀）；PAT（`pt-` 前缀）也支持 |
| `refresh_token` | 建议 | 续期用（`drt-` 前缀） |
| `region` | 建议 | `cn` / `global`；缺失时按配置兜底（默认 global） |
| `secret` | 否 | qoder2api 导出格式：整对凭证放在这个 JSON 字符串里 |

### OpenCode ZEN 凭证字段

OpenCode ZEN 无登录流程，直接用官网申请的 API key 即可：

```json
{
  "type": "freetier",
  "vendor": "opencodezen",
  "api_key": "sk-...",
  "label": "我的 ZEN Key"
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `api_key` | 是 | API Key（`apikey` / `zen_key` 亦兼容） |
| `label` | 否 | 展示名（缺失时自动脱敏显示为 `前4位***后4位`） |

### Cline 凭证字段

通过控制台页的「添加账号 ▾」->「Cline」完成 WorkOS 设备码授权自动落盘，亦可手动填入：

```json
{
  "type": "freetier",
  "vendor": "cline",
  "refreshToken": "...",
  "accessToken": "...",
  "email": "user@example.com"
}
```

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `refreshToken` | 是 | 长期凭据（WorkOS 设备授权产物，续期核心） |
| `accessToken` | 否 | 出站令牌（缺失时插件首次刷新会自动用 refreshToken 换取） |
| `email` | 建议 | 账号展示邮箱 |

## 客户端接入

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <CPA API KEY>" -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

模型用**裸名**（不带供应商前缀）。跨供应商的同名模型由宿主按
`strings.EqualFold` 合并为一个条目，宿主再从各家凭证里选号。

> **注意**：同名模型的选号可能花到另一家供应商的额度。例如 `GLM-5.3`（Qoder）
> 与 `glm-5.3`（WorkBuddy）在宿主眼里是同一个模型。若需要精确指定，用
> `vendor:model` 形式（如 `workbuddycn:glm-5.2`）。

## 配置项

宿主 `plugins.configs.freetier2api` 下：

| 键 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `enabled_realms` | string | `cn,global` | 启用的区域；只留 `cn` 时国际版账号不会被路由到 |
| `extra_models` | string | 空 | 额外注册的模型名，逗号或空格分隔；可带 `vendor:` 前缀限定 |
| `state_dir` | string | `~/.freetier2api-plugin` | 状态目录（机器盐、模型缓存、任务记录、日志） |
| `log_level` | enum | `info` | `debug` / `info` / `error` |
| `log_to_file` | bool | `false` | 是否把日志写入 `<state_dir>/logs` |
| `prompt_mode` | enum | `passthrough` | `passthrough` 透传 / `custom` 替换 / `append` 追加（WorkBuddy） |
| `prompt_file` | string | 空 | 自定义提示词文件；留空用内置（WorkBuddy） |
| `sanitize_fingerprints` | bool | `true` | 出站请求体指纹脱敏（WorkBuddy） |
| `passthrough_ip` | bool | `false` | 是否把客户端 IP 透传给上游 |
| `user_agent` / `client_version` / `cli_version` | string | 空 | 出站标识覆盖（WorkBuddy） |
| `device_token` / `device_token_file` | string | 空 | `X-Device-Token` 兜底（WorkBuddy） |
| `machine_salt` | string | 自动生成 | 设备指纹盐；从旧插件迁移时填原值可保持指纹不变 |
| `auto_checkin` / `auto_checkin_at` | bool / string | **`true`** / `10:00` | 每日自动任务（默认开启） |
| `auto_tasks` | bool | **`true`** | 每日自动跑任务闭环（连登兑换、抽奖、旅行、夜猫子等） |
| `zen_base_url` | string | 空 | 覆盖 OpenCode ZEN 上游基地址（默认 `https://opencode.ai/zen`） |
| `cline_base_url` | string | 空 | 覆盖 Cline 上游基地址（默认 `https://api.cline.bot/api/v1`） |

## 从旧插件迁移

旧插件（`workbuddy2api` / `qoder2api`）的凭证文件名是 `workbuddy-<uid>.json`、
`qoder-login-<id>.json`，`type` 字段分别是 `workbuddy` / `qoder`，本插件认不出来。

用仓库里的迁移脚本转换（**只新增文件，绝不改动原文件**——旧插件还在读它们）：

```bash
python3 scripts/migrate_auths.py /path/to/cpa/auths          # 预演
python3 scripts/migrate_auths.py /path/to/cpa/auths --apply  # 执行
```

脚本会为每个旧凭证生成一份 `<vendor>-<uid>.json`（`type` 改成 `freetier`、
补上 `vendor` 字段），原文件原样保留。两套文件并存时，宿主按 `type` 字段
路由到不同插件，互不干扰。

## 管理接口

需要 CPA 的管理密钥（`remote-management.secret-key`）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v0/management/plugins/freetier2api/status` | 账号、额度与任务状态概览（**不含 token**） |
| GET | `/vendors` | 已启用的供应商清单（供页面下拉与分组） |
| POST | `/accounts/delete` | 删除指定账号凭证文件（宿主监听 auth 目录自动注销） |
| POST | `/checkin` | 签到（body `{"account_ids":[...]}`，省略即全部） |
| POST | `/quotas` | 批量查额度 |
| GET | `/models` | 读取当前注册的模型清单（读缓存，不打上游） |
| POST | `/models/refresh` | 从上游拉取模型清单并缓存 |
| GET | `/logs?since=&limit=` | 读插件日志（环形缓冲，增量拉取） |
| POST | `/settings` | 改插件运行期设置 |
| POST | `/tasks/scan` | 全账号任务扫描 |
| POST | `/tasks/run` | 执行待办队列（账号内串行、账号间并发 1-4） |
| GET | `/tasks/queue` | 队列进度 |
| POST | `/tasks/auto` | 单账号：单任务一键完成 / 一键完成全部 |
| POST | `/tasks/auto_all` | 全部账号依次一键完成（控制台页的批量按钮用它） |
| GET | `/school/status` | 开学季活动状态 |
| POST | `/travel` `/activity` `/keepalive` `/blackcat` `/school/run` | 手动触发定时任务 |
| GET | `/v0/resource/plugins/freetier2api/console` | 内嵌控制台页 |

控制台页是纯静态 HTML，**不含任何账号或凭证数据**；所有数据都通过上面的管理接口按需拉取。
页面在遇到 401/403 时会**立即停止自动刷新**——CPA 按客户端 IP 统计管理鉴权失败次数，
连续失败会封禁该 IP。

## 已知限制

- **模型清单变更后需要重启宿主**：宿主的模型注册表只在插件加载/重载时读取。
- **同名模型跨供应商合并**：宿主按 `EqualFold` 合并，选号可能落到另一家。
  用 `vendor:model` 精确指定。
- **WorkBuddy 国际版没有签到与成长任务**、**Qoder 国际版没有签到计划**：
  这些活动只在国内版提供，入口会被跳过（不报错、不发请求）。
- **Qoder 上游没有配额重置接口**：额度按计费周期自动恢复。
- **部分 WorkBuddy 任务不可自动化**：`Expert_Philanthropy` 需要真实捐款
  （服务端校验捐赠回执）。
- **宿主「OAuth 模型禁用」弹窗列不出本插件的模型**：该弹窗读的是宿主内置的
  `/model-definitions/<channel>`，其 channel 是**硬编码的原生 provider 白名单**，
  插件的 provider 不在其中，因此会显示「无法获取模型列表」。这是宿主限制，
  与插件无关。**请用本插件控制台页的「模型」区块查看模型清单。**
- **上游风控依赖设备指纹**：换机器或换状态目录会被视为新设备。

## 许可

GPL-3.0（并入的 Qoder 部分源自 GPL-3.0 的 qoder2api / QCCG）。
第三方来源与署名见 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。
