# 验证记录

本文件记录移植过程中的验证方式与发现的偏差，便于后续排查与接手。

## 一、自动化验证

```
gofmt -l .            # 干净（cpasdk/ 按约定豁免）
go vet ./...          # 无告警
go test ./... -count=1  # 全绿
CGO_ENABLED=1 go build -buildmode=c-shared  # 产出 freetier2api.so（约 10 MB）
```

测试覆盖（无需真实 CPA 进程，全部走内存假宿主）：

| 文件 | 覆盖主题 |
| --- | --- |
| `plugin_test.go` | 注册响应契约（含 `executor_model_scope`）、热更新、未知方法、凭证归属判定、**realm 隔离**、静态模型元数据、非流式聚合、流式分帧与错误上报 |
| `registry_test.go` | 三方源清单格式、插件 ID 与产物名一致、控制台页路由声明、页面静态自包含、能力与实现一致 |
| `internal/vendors/workbuddy/cb_test.go` | 错误分类 15 级顺序、模型避让的字段边界、重试与状态码映射、等待信息解析、凭证双形态、合并写回、指纹稳定性、缓存键隔离、改写管线各步、工具配对、SSE 聚合与白名单重建、脱敏 |
| `internal/httpx/transport_test.go` | 宿主 HTTP 桥的**双形态键名兼容**、缺状态码的显式报错 |
| `internal/tasks/tasks_test.go` | 各事件链结构与关键字段、小程序事件形态、指纹注入、夜猫子窗口边界 |

## 二、关键机制验证（读宿主源码确认）

realm 隔离是本插件最容易失效的机制，已逐环节验证：

| 环节 | 证据 |
| --- | --- |
| 插件按凭证注册模型 | `sdk/cliproxy/service_executors.go:454` `RegisterClient(a.ID, providerKey, models)` |
| **选凭证时按模型过滤** | `sdk/cliproxy/auth/conductor_selection.go:1202/1294/1756/2090/2314` → `:993` `ClientSupportsModel(auth.ID, routeKey)` |
| 入站模型 → provider | `internal/registry/model_registry.go:1585` `r.models[modelID]`（map 精确匹配） |
| `cn:` 冒号安全性 | `internal/thinking/suffix.go:25` 只解析尾部括号，不受冒号影响 |
| `model.for_auth` 生效前提 | `internal/pluginhost/adapters.go:317` `executorScopeAllowsOAuthModels`——**必须声明 `executor_model_scope` 为 `oauth`/`both`**，否则该能力被静默跳过 |
| 插件错误 → 宿主处置 | `internal/pluginhost/rpc_client.go:368` 只保留 `code` 与 `http_status`，其中 **`code` 被丢弃、只有 `http_status` 影响冷却** |
| (凭证, 模型) 级冷却 | `sdk/cliproxy/auth/conductor_cooldown.go:872`——插件返回 429 天然是 per-(auth, model) 冷却（`rpcError` 无 `IsCredentialScoped()`） |

`registry_test.go` 里的 `TestCapabilitiesMatchImplementedHandlers` 与
`plugin_test.go` 里的 `TestModelsForAuthIsolatesRealms` 把其中两条锁进了回归测试。

## 三、移植中发现的上游偏差

### 3.1 `sanitize.go` 的三个正则名不副实

上游 `internal/upstream/sanitize.go` 里三个正则的实际内容与名字/注释不符：

| 名字 | 注释声称 | 实际内容 | 实际行为 |
| --- | --- | --- | --- |
| `sanitizeHdrRe` | 「剥离层：header 键名即触发，整段删除」 | `` `(?i)\n]*;?\s*` `` | 只匹配 `\n]*` + 可选 `;` + 空白；**不含键名**，删不掉键值段 |
| `sanitizeKvRe` | 「剥离层：尾随裸键值（cc_xxx=...;）循环清理」 | `` `(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*` `` | 只删 `cc_` 前缀的键值对，不是通用的 `x-anthropic-billing-hdr: ...` 形态 |
| `sanitizeBareHdrRe` | 「裸键名（无冒号无值）同样是指纹」 | `` `(?i)x-anthropic-billing-hdr` `` | 只匹配那个完整键名 |

结论：header 键值段的实际剥离能力弱于注释所述。**本插件按注释所载的意图实现**
（`sanitizeHdrRe` 在 `sanitize_rules.json` 中按「键名 + 后续键值段整段删除」表达），
其余两条沿用原样。若上游修正了这三个正则，需要同步 `internal/cb/sanitize_rules.json`。

> 注：该文件的部分字符串字面量在读取工具的输出中会被脱敏改写，
> 因此规则表是**程序化提取**（而非手工转录）生成的，以保证字节精确。
> 已用哈希与源文件比对确认。

### 3.2 错误分类的一处顺序陷阱

`Classify` 把「含 `rate-limiting` 文案的 400」归为 `soft_rate` 而非 `account_fault`，
因为 `accountFaultMarkers` 只看 `request illegal` 等关键词、**刻意不按 code 11140 判定**
（该 code 同时承载模型级限流文案）。测试 `TestClassifyOrderingMatters` 固定了这个行为。

## 四、需要真实环境才能验证的部分

以下无法在无 CPA 的环境下验证，需用户自备 CPA 与真实 CodeBuddy 账号：

1. 插件加载与 `plugin.register` 的实际往返；
2. 真实上游的模型清单探测结果（模型数量与字段）；
3. 真实对话出字（流式与非流式）；
4. 签到、余额查询与任务上报的真实返回；
5. **双域隔离的端到端行为**：`cn:` 模型只被 cn 凭证服务。

建议的验证顺序：

```bash
# 1. 账号是否被识别（应带 realm 与余额）
curl -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/freetier2api/status

# 2. 模型清单是否拉到
curl -X POST -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/freetier2api/models/refresh

# 3. 对话（非流式与流式）
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}'
```

## 五、与其它插件共存时的凭证解析阻断（真实踩坑记录）

### 现象

插件注册日志完全正常（`plugin registered` / `registered: realms=[cn global]`），
管理接口 200，但**账号列表为空**，且宿主日志里**从未出现本插件的 `auth.parse` 调用**。

### 根因

宿主解析凭证文件时有两个行为叠加：

1. **遍历插件，一旦报错或认领就中止循环**（`internal/pluginhost/auth_provider.go`）：

   ```go
   for _, record := range h.activeRecords() {
       auths, handled, errParse := h.callParseAuths(ctx, record, req)
       if errParse != nil || handled {
           return auths, handled, errParse   // 后面的插件轮不到
       }
   }
   ```

2. **把「当前询问的插件的 identifier」兜底填进 `provider`**
   （`internal/pluginhost/auth_provider.go`）：

   ```go
   req.Provider = normalizeProviderID(req.Provider)   // 文件里没有 type 字段 → 空
   if req.Provider == "" {
       req.Provider = normalizeProviderID(provider.Identifier())   // 被填成"当前插件"
   }
   ```

因此一个归属判定写成「provider 等于自己就认领」的插件，会在被询问时**认领所有文件**；
若它解析不了目标插件格式的文件而返回错误，循环即中止。

遍历顺序是 `priority` 降序、同优先级按 `id` 升序。不配 priority 时
`qoder2api` < `workbuddy2api`，本插件排在后面 → 被压制。

### 修复

给本插件更高的 `priority`：

```yaml
plugins:
  configs:
    workbuddy2api:
      priority: 100
```

修复后宿主按预期调用本插件并认领全部凭证（实测 4 个账号，2 cn + 2 global）。

### 另需注意：动态库的 libc 必须与宿主一致

本插件是 cgo 构建的 c-shared 动态库，**libc 必须与宿主运行时一致**。
用 alpine（musl）构建的产物在 Debian（glibc）宿主上会加载失败：

```
failed to load plugin workbuddy2api: dlopen ...:
libc.musl-aarch64.so.1: cannot open shared object file
```

自己构建时请选与宿主同族的镜像（宿主是 `eceasy/cli-proxy-api`，基于 Debian）：

```bash
docker run --rm -v "$PWD":/app -w /app golang:1.24-bookworm \
  sh -c 'CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -buildmode=c-shared -o dist/freetier2api.so .'
```
