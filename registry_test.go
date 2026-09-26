package main

// 本文件用测试锁死 registry.json 的格式。
//
// registry.json 是用户配置三方源后真正会被读取的文件，手改坏 = 商店里
// 直接看不见插件（而且宿主对此是静默的），因此值得用测试守住。
//
// 宿主 CLIProxyAPI/internal/pluginstore 对三方源清单的校验规则：
//   - 顶层 schema_version 必须等于 1
//   - 每个条目必填 id / name / description / author；github-release 类型还要 repository
//   - id 匹配 ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$
//   - version 可选（宿主从 latest release 推导），有则必须匹配 ^[0-9][0-9A-Za-z.+-]*$
//   - repository 必须是 https://github.com/{owner}/{repo}

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	// unwrapRe 匹配「再剥一层信封」的写法：.result 后面不接 s（区分合法的 .results）。
	unwrapRe             = regexp.MustCompile(`\.result\s*[;,)\n]`)
	pluginIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	pluginVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)
	githubRepositoryRe   = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+$`)
)

// registryManifest 是三方源清单的结构。
type registryManifest struct {
	SchemaVersion int `json:"schema_version"`
	Plugins       []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Author      string   `json:"author"`
		Repository  string   `json:"repository"`
		Homepage    string   `json:"homepage"`
		License     string   `json:"license"`
		Version     string   `json:"version"`
		Tags        []string `json:"tags"`
	} `json:"plugins"`
}

// TestRegistryManifestIsValid 验证 registry.json 满足宿主的校验规则。
func TestRegistryManifestIsValid(t *testing.T) {
	raw, errRead := os.ReadFile("registry.json")
	if errRead != nil {
		t.Fatalf("read registry.json: %v", errRead)
	}
	var manifest registryManifest
	if errUnmarshal := json.Unmarshal(raw, &manifest); errUnmarshal != nil {
		t.Fatalf("registry.json is not valid JSON: %v", errUnmarshal)
	}
	if manifest.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", manifest.SchemaVersion)
	}
	if len(manifest.Plugins) == 0 {
		t.Fatal("registry must declare at least one plugin")
	}

	for _, entry := range manifest.Plugins {
		if entry.ID != pluginID {
			t.Fatalf("registry id = %q, must match the plugin id %q (the library file name)", entry.ID, pluginID)
		}
		if !pluginIDPattern.MatchString(entry.ID) {
			t.Fatalf("id %q does not match the host pattern", entry.ID)
		}
		for field, value := range map[string]string{
			"name": entry.Name, "description": entry.Description, "author": entry.Author,
		} {
			if strings.TrimSpace(value) == "" {
				t.Fatalf("required field %q is empty", field)
			}
		}
		if !githubRepositoryRe.MatchString(entry.Repository) {
			t.Fatalf("repository %q must be https://github.com/{owner}/{repo}", entry.Repository)
		}
		if entry.Version != "" && !pluginVersionPattern.MatchString(entry.Version) {
			t.Fatalf("version %q does not match the host pattern", entry.Version)
		}
	}
}

// TestPluginIDMatchesLibraryName 验证插件 ID 与产物名一致。
//
// 宿主按 {id}{ext} 找动态库；ID 与文件名不一致会让插件静默不被加载。
func TestPluginIDMatchesLibraryName(t *testing.T) {
	if pluginID != "workbuddy2api" {
		t.Fatalf("pluginID = %q; changing it requires renaming the build output and registry", pluginID)
	}
	// providerKey 在 cb 包与 main 包各定义一次（凭证归属判定与注册都用它），必须一致。
	if providerKey != cbProviderKey {
		t.Fatalf("providerKey (%q) must match the cb package value (%q)", providerKey, cbProviderKey)
	}
}

// TestConsolePagePathsAreDeclared 验证控制台页引用的路径都在路由表里声明。
//
// 页面引用了未声明的路由会表现为「点了没反应」，而宿主不会报错。
func TestConsolePagePathsAreDeclared(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)

	declared := map[string]bool{}
	for _, route := range managementRegistration().Routes {
		suffix := strings.TrimPrefix(route.Path, managementRoutePrefix)
		declared[suffix] = true
	}

	// 页面里通过 MGMT + path 拼接调用接口，因此按已知清单核对。
	referenced := []string{
		"/status", "/logs", "/checkin", "/quotas", "/models/refresh",
		"/settings", "/tasks/scan", "/tasks/run", "/tasks/queue", "/tasks/auto",
		"/keepalive",
	}
	for _, path := range referenced {
		if !strings.Contains(page, `"`+path) {
			t.Fatalf("console page does not reference %q (test list is stale?)", path)
		}
		if !declared[path] {
			t.Fatalf("console page calls %q but the route is not declared", path)
		}
	}
}

// TestConsolePageDecodesPanelKey 验证控制台页能解开 CPA 面板保存的管理密钥。
//
// 本页与面板同源，因此可以复用面板已保存的密钥，避免用户重复输入。
// 面板把密钥存在 localStorage 的 "managementKey" 键下，并做了可逆的 XOR 混淆：
//
//	前缀 "enc::v1::" + base64( utf8(明文) XOR key )
//	key = utf8("cli-proxy-api-webui::secure-storage|<host>|<userAgent>")
//
// 密钥由公开值派生，所以同源页面能解出来。这里断言页面确实实现了这套算法
// （算法细节若与面板不一致，用户会看到 401 且原因极难定位）。
func TestConsolePageDecodesPanelKey(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)

	// 混淆格式的常量必须与面板一致。
	for _, literal := range []string{
		`"enc::v1::"`,                           // 前缀
		`"cli-proxy-api-webui::secure-storage"`, // 种子
		`localStorage.getItem("managementKey")`, // 面板的存储键
		"location.host",                         // 密钥派生输入
		"navigator.userAgent",                   // 密钥派生输入
		"atob(",                                 // base64 解码
		"TextDecoder",                           // utf8 解码
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing %s (key decoding would break)", literal)
		}
	}

	// 密钥必须校验字符集：面板的混淆密钥由 userAgent 派生，浏览器升级后会
	// 解出乱码；把乱码放进 Authorization 头会被浏览器拒绝（Safari 报
	// "The string did not match the expected pattern."），且极难定位。
	if !strings.Contains(page, "KEY_RE") {
		t.Fatal("console page must validate the key charset before using it in a header")
	}

	// 自动读取失败时必须能退回手填，否则用户会面对一片空白。
	for _, literal := range []string{`id="keyBox"`, `id="keyInput"`, `id="btnUseKey"`, "revealKeyInput"} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the manual key fallback (%s)", literal)
		}
	}
}

// TestConsolePageDoesNotUnwrapHostBody 验证页面不会误剥响应字段。
//
// 宿主把插件的响应体**原样**透传给浏览器：{ok, result} 那层信封是插件与宿主
// 之间的 RPC 格式，不会到达前端。若页面再剥一层 "result"，所有业务字段都会
// 变成 undefined——表现就是「接口 200 但界面全空」，且极难定位。
func TestConsolePageDoesNotUnwrapHostBody(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)

	// 注意用正则精确匹配：字符串包含 "data.result" 也会命中 "data.results"（合法字段）。
	if unwrapRe.MatchString(page) {
		t.Fatal("console page must not unwrap a second envelope layer (the host passes the body through verbatim)")
	}
	// 业务字段必须是从响应顶层直接取的。
	for _, field := range []string{
		"status.accounts",       // 账号列表
		"payload.entries",       // 日志
		"payload.pending_count", // 扫描结果
		"data.models",           // 模型清单
		"data.results",          // 额度/任务结果
	} {
		if !strings.Contains(page, field) {
			t.Fatalf("console page must read %q directly from the response body", field)
		}
	}
}

// TestConsolePageWiresHostOAuthLogin 验证「添加账号」接的是宿主的 OAuth 端点。
//
// 宿主为插件提供两段式登录：
//
//	GET /v0/management/<provider>-auth-url?realm=...   → 转给 auth.login.start
//	GET /v0/management/get-auth-status?state=...       → 转给 auth.login.poll
//
// 页面必须走这两个端点（而不是插件自己的路由），否则「添加账号」不可用。
func TestConsolePageWiresHostOAuthLogin(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)

	for _, literal := range []string{
		"/workbuddy-auth-url", // 与 providerKey 拼接，宿主按此匹配 auth provider
		"get-auth-status",     // 宿主提供的状态轮询端点
		`id="btnAddCn"`,       // 国内版入口
		`id="btnAddGlobal"`,   // 国际版入口
		"startLogin",          // 登录流程
		"pollLogin",           // 轮询流程
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the add-account wiring (%s)", literal)
		}
	}
	// 这两个接口在**宿主根路径**下（/v0/management/<provider>-auth-url），
	// 不能带插件前缀：宿主按路径提取 provider，带前缀会得到
	// "plugins/workbuddy2api/workbuddy"，校验失败直接 404。
	if !strings.Contains(page, "hostPath(") {
		t.Fatal("host-level OAuth endpoints must not be prefixed with the plugin path")
	}
	if strings.Contains(page, `api("GET", "/workbuddy-auth-url`) {
		t.Fatal("auth-url must go through hostPath() (the host extracts the provider from the path)")
	}
	if !strings.Contains(page, `var HOST_MGMT = "/v0/management"`) {
		t.Fatal("console page must know the host management prefix")
	}

	// 域必须区分：两域凭证不通用，登录入口必须能选。
	if !strings.Contains(page, `startLogin("cn")`) || !strings.Contains(page, `startLogin("global")`) {
		t.Fatal("console page must offer both cn and global login entries")
	}
}

// TestListHostAuthsClaimsHijackedCredentials 验证被前置插件抢走的凭证仍被列出。
//
// 宿主遍历插件解析凭证时，一旦某个插件报错或认领就中止循环，且会把
// 「当前询问的插件的 identifier」兜底填进 provider。归属判定过宽的前置插件
// 会把本插件的文件抢走（provider 变成对方），只按 provider 过滤会让这些
// 账号从页面凭空消失。文件名仍是本插件约定，因此按文件名兜底认领。
func TestListHostAuthsClaimsHijackedCredentials(t *testing.T) {
	if !fileNameBelongsToPlugin("workbuddy-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json") {
		t.Fatal("workbuddy-<uuid>.json must be recognized by file name")
	}
	if !fileNameBelongsToPlugin("my-workbuddy.json") {
		t.Fatal("file name containing the workbuddy segment must be recognized")
	}
	// 别家的文件名不能被认领，否则会把别人的账号当成自己的。
	for _, foreign := range []string{"qoder-login-x.json", "claude-1.json", "codex-a@b.com.json", "auth.json"} {
		if fileNameBelongsToPlugin(foreign) {
			t.Fatalf("foreign file %q must not be claimed", foreign)
		}
	}

	// provider 被抢走（qoder）但文件名是本插件的：仍要列出来。
	entries := filterPluginAuths([]hostAuthEntry{
		{Name: "workbuddy-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json", Provider: "qoder", AuthIndex: "1"},
		{Name: "workbuddy-110693a0-d32d-4d81-8a0a-7a3445069f96.json", Provider: "workbuddy", AuthIndex: "2"},
		{Name: "qoder-login-f34c07f7.json", Provider: "qoder", AuthIndex: "3"},
	})
	if len(entries) != 2 {
		t.Fatalf("want 2 workbuddy credentials (one hijacked), got %d: %+v", len(entries), entries)
	}
	for _, entry := range entries {
		if entry.Provider == "qoder" && strings.Contains(entry.Name, "qoder-login") {
			t.Fatalf("foreign credential leaked in: %+v", entry)
		}
	}
}

// TestManagementResponseUsesHostWireFormat 验证管理响应沿用宿主的线格式。
//
// pluginapi.ManagementResponse **没有 json tag**，宿主按 Go 字段名
// （StatusCode / Headers / Body）解码，且 Body 是 []byte——Go 会把它编码成
// base64，宿主解码后把原始字节写给浏览器。
//
// 曾试图改成 snake_case + json.RawMessage 的自有格式，结果宿主反序列化失败，
// 页面报 "plugin resource handler failed" 且什么都不显示。这条测试锁住
// 「必须用宿主结构体、键名必须是 Go 字段名」这个契约。
func TestManagementResponseUsesHostWireFormat(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	resp := jsonResponse(http.StatusOK, map[string]any{"hello": "world"})
	raw, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	// 键名必须是宿主认得的 Go 字段名，不能是 snake_case。
	if _, okStatus := decoded["StatusCode"]; !okStatus {
		t.Fatalf("response must use the host's Go field names, got %s", raw)
	}
	if _, okBad := decoded["status_code"]; okBad {
		t.Fatalf("snake_case keys break the host decoder, got %s", raw)
	}
	if _, okBody := decoded["Body"]; !okBody {
		t.Fatalf("Body must be present, got %s", raw)
	}
	// HTML 响应同样走宿主结构体。
	htmlRaw, errHTML := json.Marshal(htmlResponse(http.StatusOK, []byte("<html>x</html>")))
	if errHTML != nil {
		t.Fatalf("marshal html: %v", errHTML)
	}
	if !strings.Contains(string(htmlRaw), `"StatusCode":200`) {
		t.Fatalf("html response must use the host wire format, got %s", htmlRaw)
	}
}

// TestConsolePageIsSingleScrollLayout 验证页面是单页滚动而非标签页。
//
// 标签页会把额度、模型、任务藏起来，管理页应当一眼看全。
func TestConsolePageIsSingleScrollLayout(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)

	// 不应再有导航与视图切换。
	for _, forbidden := range []string{"<nav>", "data-view=", "switchView", `id="view-accounts" hidden`} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("console page must be a single scrolling page, found %q", forbidden)
		}
	}
	// 各区块都必须在页面上直接可见（不带 hidden）。
	for _, section := range []string{"账号", "额度", "模型", "任务", "设置", "运行日志"} {
		if !strings.Contains(page, section) {
			t.Fatalf("console page is missing the %q section", section)
		}
	}
	// 额度与模型必须有自己的容器与按钮。
	for _, literal := range []string{
		`id="btnLoadQuotas"`, // 额度：按钮 + 并入账号表的列
		`id="models"`, `id="btnLoadModels"`, `id="modelFilter"`,
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing %s", literal)
		}
	}
	// 任务执行进度不再单独一张表：并入各账号的任务清单。
	if strings.Contains(page, `id="queue"`) {
		t.Fatal("queue progress must be merged into the per-account task lists")
	}
	if !strings.Contains(page, `id="queueProgress"`) {
		t.Fatal("console page must show a compact queue progress summary")
	}

	// 额度不再单独占一张表：结果并入账号表的「剩余 / 总额」与「使用率」两列。
	if strings.Contains(page, `id="quotas"`) {
		t.Fatal("quota must be merged into the accounts table, not a separate section")
	}
	if !strings.Contains(page, "剩余 / 总额") || !strings.Contains(page, "使用率") {
		t.Fatal("accounts table must carry the quota columns")
	}
}

// TestConsolePageHasBulkTaskButton 验证「一键完成」是批量按钮而非每账号一个。
//
// 逐账号一个按钮在账号多时既难用也容易误点；统一成「全部账号依次执行」。
func TestConsolePageHasBulkTaskButton(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)

	if !strings.Contains(page, `id="btnAutoAll"`) {
		t.Fatal("console page must expose a bulk task button")
	}
	if !strings.Contains(page, "/tasks/auto_all") {
		t.Fatal("bulk task button must call the /tasks/auto_all endpoint")
	}
	// 表格里不应再逐账号渲染「一键完成」按钮（签到按钮保留）。
	if strings.Contains(page, `el("button", "一键完成"`) {
		t.Fatal("per-account task buttons must be replaced by the bulk button")
	}
}

// TestConsolePageStopsPollingOnAuthFailure 验证鉴权失败会停止轮询。
//
// CPA 按客户端 IP 统计管理鉴权失败次数，连续失败会封禁该 IP
// （连正确密钥也会被拒），因此必须停。
func TestConsolePageStopsPollingOnAuthFailure(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)
	if !strings.Contains(page, "haltOnAuthFailure") {
		t.Fatal("console page must stop polling on auth failure to avoid an IP ban")
	}
	if !strings.Contains(page, "401") || !strings.Contains(page, "403") {
		t.Fatal("console page must treat both 401 and 403 as auth failures")
	}
}

// TestConsolePageIsStaticAndSelfContained 验证页面不含任何凭证数据。
//
// 资源页不做管理鉴权，因此页面本身绝不能带账号或令牌。
func TestConsolePageIsStaticAndSelfContained(t *testing.T) {
	page := consolePageHTML(managementRoutePrefix)

	for _, forbidden := range []string{"accessToken", "refreshToken", "device_token", "Authorization: Bearer sk-"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("console page must not embed credential material (%q found)", forbidden)
		}
	}
	// 唯一的网络目标必须是 CPA 自身。
	if strings.Contains(page, "http://") && !strings.Contains(page, "http://127.0.0.1") {
		t.Fatal("console page must not reference external origins")
	}
	// 页面必须能停止自动刷新（避免触发 CPA 的按 IP 鉴权封禁）。
	if !strings.Contains(page, "haltOnAuthFailure") {
		t.Fatal("console page must stop polling on auth failure")
	}
}

// TestHostReservedQuotaPathNotShadowed 验证插件路由不与宿主保留路径冲突。
//
// 宿主的单账号额度路由是 /v0/management/plugins/<id>/quota，
// 插件自己的批量接口必须换路径（/quotas），否则会被宿主遮蔽。
func TestHostReservedQuotaPathNotShadowed(t *testing.T) {
	for _, route := range managementRegistration().Routes {
		suffix := strings.TrimPrefix(route.Path, managementRoutePrefix)
		if suffix == "/quota" {
			t.Fatal("plugin must not declare /quota (the host owns that path for single-account quota)")
		}
	}
}

// TestCapabilitiesMatchImplementedHandlers 验证声明的能力都有对应实现。
//
// 声明了能力却没有实现会让宿主调用到「未知方法」，表现为功能静默失效。
func TestCapabilitiesMatchImplementedHandlers(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	registration := pluginRegistration()
	if !registration.Capabilities.ModelProvider || !registration.Capabilities.AuthProvider ||
		!registration.Capabilities.Executor || !registration.Capabilities.QuotaProvider ||
		!registration.Capabilities.ManagementAPI {
		t.Fatal("all five capabilities must be declared")
	}

	// 每个能力的关键方法都必须能被分发（不返回 unknown_method）。
	methods := []string{
		"model.static", "model.for_auth",
		"auth.identifier", "auth.parse", "auth.login.start", "auth.login.poll", "auth.refresh",
		"executor.identifier", "executor.execute", "executor.execute_stream",
		"executor.count_tokens", "executor.http_request",
		"quota.identifier", "quota.describe", "quota.fetch", "quota.reset",
		"management.register", "management.handle",
	}
	for _, method := range methods {
		raw, errHandle := handleMethod(method, nil)
		if errHandle != nil {
			// 业务错误（如缺参数）是可接受的；协议层错误才是问题。
			continue
		}
		if strings.Contains(string(raw), "unknown_method") {
			t.Fatalf("method %s is declared but not dispatched", method)
		}
	}
}
