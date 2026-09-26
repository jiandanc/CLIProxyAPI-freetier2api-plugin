# workbuddy2api-plugin

把腾讯 CodeBuddy（WorkBuddy）账号接入 **CLIProxyAPI**（下称 CPA）的原生动态库插件。

插件通过宿主的 `cliproxy_plugin_init` ABI 加载，一次性声明五类能力，
宿主的路由、鉴权、调度、日志与代理全部复用——**不需要再单独跑一个网关服务**：

| 能力 | 作用 |
| --- | --- |
| `auth_provider` | 识别并加载 `workbuddy-*.json` 凭证，支持 OAuth 设备授权登录与定时刷新 |
| `model_provider` | 注册模型（`cn:` / `global:` 前缀区分国内版与国际版），清单从上游动态探测 |
| `executor` | 转发 chat-completions（含流式）；Claude / Codex 客户端由宿主翻译 |
| `quota_provider` | 查询账号额度，供管理端展示 |
| `management` | 自有管理接口 + 内嵌控制台页（账号、任务、设置、日志） |

## 特性

- **双域隔离**：CodeBuddy 的国内版与国际版是两套独立部署，**凭证不通用**。
  插件用 `cn:` / `global:` 前缀把模型与凭证强绑定——`model.for_auth` 按凭证的域
  只返回同域模型，宿主在选凭证阶段据此淘汰跨域凭证。
- **模型清单动态探测**：国内版两路（`/v3/config` + 企业目录）、国际版三路
  （v3 的 IDE UA + CLI UA + 企业目录）。实测两路各有独有模型，缺一不可。
- **额度与签到**：每日签到、余额查询，签到后自动跑连登兑换与抽奖闭环。
- **任务自动化**：猫猫旅行、夜猫子、活跃上报、token 保活，
  以及**成长任务一键完成**（17 个任务）与开学季活动。
- **提示词与脱敏**：系统提示词三模式（透传 / 替换 / 追加）+ 出站请求体指纹脱敏，
  两层叠加降低上游内容审核拦截率。
- **零第三方依赖**：`go.mod` 只有 module 与 go 版本两行，构建不需要拉包。

## 构建

需要 Go 1.22+ 与 C 编译器（cgo）。

```bash
./build.sh          # 先跑 go test ./...，再产出 dist/workbuddy2api.<ext>
```

产物按平台命名：Linux `workbuddy2api.so`、macOS `workbuddy2api.dylib`、Windows `workbuddy2api.dll`。

发行版号由 CI 用 `-ldflags -X main.pluginVersion=<tag>` 注入，本地构建保留默认值。

## 安装

### 通过 CPA 插件商店（推荐）

在 CPA 的 `config.yaml` 里把本仓库的 `registry.json` 加为三方源：

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/jiandanc/CLIProxyAPI-workbuddy2api-plugin/main/registry.json"
```

重启 CPA → 管理面板 → 插件商店 → 搜 **WorkBuddy 2API** → 一键安装。

### 手动安装

把产物放进 CPA 的插件目录，**文件名必须是 `workbuddy2api.<ext>`**
（宿主按 `{pluginID}{ext}` 找动态库）：

```yaml
plugins:
  enabled: true
  dir: "/path/to/cpa/plugins"
  configs:
    workbuddy2api:
      enabled: true
      enabled_realms: "cn,global"
      # ⚠️ 装了其它插件时，这个 priority 是必须的，见下方「与其它插件共存」
      priority: 100
```

### ⚠️ 与其它插件共存（重要）

宿主解析凭证文件的顺序是**按插件 priority 降序、同优先级按 id 升序**，
并且遍历时**一旦某个插件报错或认领成功就中止整个循环**：

```go
// internal/pluginhost/auth_provider.go
for _, record := range h.activeRecords() {
    auths, handled, errParse := h.callParseAuths(ctx, record, req)
    if errParse != nil || handled {
        return auths, handled, errParse   // ← 后面的插件轮不到
    }
}
```

同时宿主会把**当前正在询问的插件的 identifier** 当作 `provider` 传给 `auth.parse`：

```go
req.Provider = normalizeProviderID(req.Provider)
if req.Provider == "" {
    req.Provider = normalizeProviderID(provider.Identifier())
}
```

于是装了另一个「认领一切」的插件时（例如某些插件的归属判定写成了
「provider 等于自己就认领」，而那正是宿主兜底填的值），它会先认领你的凭证文件、
解析失败报错、循环中止——**本插件永远拿不到你的凭证，表现为账号列表为空**
（插件本身的注册日志一切正常，因此极难定位）。

**对策：给本插件更高的 `priority`。** 不配 priority 时 id 靠后的插件会被压住
（`qoder2api` < `workbuddy2api`，所以装了 qoder 插件时本插件会被压制）。

```yaml
plugins:
  configs:
    workbuddy2api:
      priority: 100    # 数值越大越先被询问
```

## 添加账号

两种方式，都产出一个 `workbuddy-<uid>.json` 凭证文件：

1. **控制台页登录**（推荐）：打开
   `http://<CPA>/v0/resource/plugins/workbuddy2api/console` →
   「账号」视图 → 选择国内版或国际版 → 在浏览器里完成设备授权。
2. **手写凭证**：放进 CPA 的 `auths/` 目录。

**文件名必须以 `workbuddy` 开头**（如 `workbuddy-myaccount.json`），否则插件不会认领它。

> 为什么必须这样命名：宿主会把 `auths/` 下**所有非内建格式**的凭证文件依次交给每个插件
> 判断归属。插件只能用文件里的特征字段作判据，而 `accessToken` / `refreshToken` /
> `device_token` 这类是**通用字段**（其它 provider 也用），拿它们判定会导致跨插件误吞。
> 因此归属判定改为「显式 `type` / 文件名约定 / 本插件独有的嵌套结构」三者之一，
> 其中文件名是最可靠、最容易控制的信号。

文件内容（嵌套形，字段名区分大小写）：

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
| `auth.realm` | 否 | `cn` / `global`；缺失时按 `domain` 推断，再回落插件配置 |
| `account.uid` | 建议 | 设备指纹的派生种子；缺失会导致指纹不稳定 |
| `account.nickname` | 否 | 展示名 |
| `device_token` | 否 | `X-Device-Token`；桌面端凭证需要 |

扁平形（顶层直接放 `accessToken` / `refreshToken` / `uid` / `domain`）也支持，
但**同样必须用 `workbuddy` 开头的文件名**，因为扁平形态没有任何本插件独有的特征。

也可以加 `"type": "workbuddy"` 字段显式声明归属，这样文件名就不受限制。

## 客户端接入

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <CPA API KEY>" -H "Content-Type: application/json" \
  -d '{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

模型名带 `cn:` 或 `global:` 前缀，与账号的域必须匹配。不带前缀时由宿主任选的凭证决定域。

## 配置项

宿主 `plugins.configs.workbuddy2api` 下：

| 键 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `enabled_realms` | string | `cn,global` | 启用的域；只留 `cn` 时国际版账号不会被路由到 |
| `extra_models` | string | 空 | 额外注册的模型名（不含前缀），逗号或空格分隔 |
| `state_dir` | string | `~/.workbuddy2api-plugin` | 状态目录（机器盐、模型缓存、任务记录、日志） |
| `log_level` | enum | `info` | `debug` / `info` / `error` |
| `log_to_file` | bool | `false` | 是否把日志写入 `<state_dir>/logs` |
| `prompt_mode` | enum | `passthrough` | `passthrough` 透传 / `custom` 替换 / `append` 追加 |
| `prompt_file` | string | 空 | 自定义提示词文件；留空用内置 |
| `sanitize_fingerprints` | bool | `true` | 出站请求体指纹脱敏 |
| `passthrough_ip` | bool | `false` | 是否把客户端 IP 透传给上游 |
| `user_agent` / `client_version` / `cli_version` | string | 空 | 出站标识覆盖 |
| `device_token` / `device_token_file` | string | 空 | `X-Device-Token` 兜底 |
| `machine_salt` | string | 自动生成 | 设备指纹盐；从 workbuddy2api 迁移时填原值可保持指纹不变 |
| `auto_checkin` / `auto_checkin_at` | bool / string | **`true`** / `10:00` | 每日自动签到（默认开启） |
| `auto_tasks` | bool | **`true`** | 每日自动跑任务闭环（连登兑换、抽奖、旅行、夜猫子、活跃上报、token 保活） |

任务排程（五组整点小时列表）在控制台页的「设置」里改，存插件状态目录、立即生效。

## 管理接口

需要 CPA 的管理密钥（`remote-management.secret-key`）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v0/management/plugins/workbuddy2api/status` | 账号、额度与任务状态概览（**不含 token**） |
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
| GET | `/v0/resource/plugins/workbuddy2api/console` | 内嵌控制台页 |

控制台页是纯静态 HTML，**不含任何账号或凭证数据**；所有数据都通过上面的管理接口按需拉取。
页面在遇到 401/403 时会**立即停止自动刷新**——CPA 按客户端 IP 统计管理鉴权失败次数，
连续失败会封禁该 IP。

## 已知限制

- **模型清单变更后需要重启宿主**：宿主的模型注册表只在插件加载/重载时读取。
- **国际版没有签到与成长任务**：这套体系只在国内版提供，国际版账号在所有任务入口
  都会被跳过（不报错、不发请求）。
- **部分任务不可自动化**：`Expert_Philanthropy` 需要真实捐款（服务端校验捐赠回执）。
- **宿主「OAuth 模型禁用」弹窗列不出本插件的模型**：该弹窗读的是宿主内置的
  `/model-definitions/<channel>`，其 channel 是**硬编码的原生 provider 白名单**
  （claude / gemini / codex / kimi / xai / devin 等），插件的 provider 不在其中，
  因此会显示「无法获取模型列表」。这是宿主限制，与插件无关。
  **请用本插件控制台页的「模型」区块查看模型清单。**
- **消耗额度的任务**：`Model_chat_GLM5.2`、专家类（`expert_5`、`Expert_team_use_3`、
  `Expert_lighthouse`）、`skill_1`、`black_cat` 会发真实对话；其余任务为纯事件上报。
- **上游风控依赖设备指纹**：换机器或换状态目录会被视为新设备。

## 许可

MIT。第三方来源与署名见 [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。
