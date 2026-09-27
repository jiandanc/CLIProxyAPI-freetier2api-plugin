package main

// 本文件实现 OAuth 设备授权登录。
//
// 流程（与原项目的 cmd/login 一致）：
//  1. 向 /v2/plugin/auth/state 取 state，拿到用户需要打开的授权链接；
//  2. 用户在浏览器里完成授权；
//  3. 轮询 /v2/plugin/auth/token 换 token，再用 /v2/plugin/login/account 取账号信息；
//  4. 组装凭证 JSON，交给宿主落盘成 auth 文件。
//
// 宿主只列它自己硬编码的 OAuth provider，插件不出现在那份列表里，
// 因此登录入口由插件的控制台页提供。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

const (
	// loginSessionTTL 是登录会话的有效期。
	loginSessionTTL = 15 * time.Minute
	// loginPollInterval 是插件侧轮询上游的最小间隔（防止过于频繁）。
	loginPollInterval = 2 * time.Second
	// loginHTTPTimeout 是单次登录相关请求的上限。
	loginHTTPTimeout = 30 * time.Second
	// loginUAPlatform 是登录链路的平台标识。
	loginUAPlatform = "CLI"
	// loginClientUA 是登录链路的 UA。
	loginClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// loginStartRPCRequest 与宿主的 auth.login.start 请求对齐。
type loginStartRPCRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// loginPollRPCRequest 与宿主的 auth.login.poll 请求对齐。
type loginPollRPCRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// pendingLogin 是一次进行中的登录会话。
type pendingLogin struct {
	region    workbuddy.Region
	state     string
	createdAt time.Time
	// lastPoll 记录上次向上游轮询的时刻（节流）。
	lastPoll time.Time
}

var (
	loginStoreMu     sync.Mutex
	loginStore       = map[string]*pendingLogin{}
	loginPollMinGap  = loginPollInterval
	loginStoreLastGC time.Time
)

// storeLoginSession 记录一次进行中的登录会话（含过期 GC）。
func storeLoginSession(sessionID string, session *pendingLogin) {
	loginStoreMu.Lock()
	gcLoginSessionsLocked()
	loginStore[sessionID] = session
	loginStoreMu.Unlock()
}

// takeLoginSession 取出登录会话，同时应用节流。
//
// 节流是必要的：控制台页会频繁轮询，但不该每次都打上游。
// 返回 ok=false 表示会话不存在或已过期。
func takeLoginSession(sessionID string) (*pendingLogin, bool) {
	loginStoreMu.Lock()
	defer loginStoreMu.Unlock()
	session, okSession := loginStore[sessionID]
	if okSession && time.Since(session.createdAt) > loginSessionTTL {
		delete(loginStore, sessionID)
		return nil, false
	}
	if !okSession {
		return nil, false
	}
	if time.Since(session.lastPoll) < loginPollMinGap {
		// 仍在节流窗口内：返回会话但标记为「本轮不打上游」。
		// 调用方据 lastPoll 判断——这里直接返回 pending 由调用方处理更清晰，
		// 因此用一个零值 lastPoll 表示需要等待。
		return session, false
	}
	session.lastPoll = time.Now()
	return session, true
}

// handleAuthLoginStart 开始一次登录，返回用户需要打开的授权链接。
//
// 登录协议**由供应商实现**：WorkBuddy 是自有 state/token 三接口，
// Qoder 是标准 PKCE 设备码流，两者形状完全不同。根层只负责从 Metadata
// 解析出目标供应商并按区域开关校验。
func handleAuthLoginStart(request []byte) ([]byte, error) {
	var rpc loginStartRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	vendor, errVendor := vendorForLogin(rpc.Metadata)
	if errVendor != nil {
		return nil, errVendor
	}

	ctx, cancel := context.WithTimeout(httpx.WithCallbackID(context.Background(), rpc.HostCallbackID), loginHTTPTimeout)
	defer cancel()

	response, errStart := vendor.LoginStart(ctx, rpc.Metadata)
	if errStart != nil {
		return nil, errorToPluginError(errStart)
	}
	logger.Info("started login (vendor=%s)", vendor.ID())
	return okEnvelope(response)
}

// handleAuthLoginPoll 轮询一次登录状态。
//
// 会话状态由供应商自己持有（各自的 state 结构不同），根层不参与。
func handleAuthLoginPoll(request []byte) ([]byte, error) {
	var rpc loginPollRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	sessionID := strings.TrimSpace(rpc.State)
	if sessionID == "" {
		return nil, newPluginError("invalid_request", "login state is required", http.StatusBadRequest)
	}
	vendor, errVendor := vendorForLoginSession(sessionID)
	if errVendor != nil {
		return nil, errVendor
	}

	ctx, cancel := context.WithTimeout(httpx.WithCallbackID(context.Background(), rpc.HostCallbackID), loginHTTPTimeout)
	defer cancel()

	response, errPoll := vendor.LoginPoll(ctx, sessionID)
	if errPoll != nil {
		return nil, errorToPluginError(errPoll)
	}
	return okEnvelope(response)
}

// loginSessionOwner 由持有登录会话状态的供应商实现。
//
// 会话是供应商自己建的，id 只存在于它的状态表里；根层要判断「这个会话
// 属于谁」只能问供应商。做成可选接口而不是塞进 core.Vendor：只有需要
// 多步登录的供应商才关心它，其它实现不必被迫实现一个空方法。
type loginSessionOwner interface {
	// OwnsLoginSession 报告该会话是否由本供应商创建且仍有效。
	OwnsLoginSession(sessionID string) bool
}

// vendorForLogin 从登录请求的 Metadata 解析目标供应商。
//
// 控制台页把用户选的供应商标识放在 Metadata 的 vendor 字段里；
// 缺失时回落到配置里启用的第一个区域（兼容旧版页面只传 realm 的情况）。
func vendorForLogin(metadata map[string]any) (core.Vendor, error) {
	if metadata != nil {
		if rawVendor, okVendor := metadata[core.VendorKey].(string); okVendor {
			vendor, okLookup := core.VendorByID(rawVendor)
			if !okLookup {
				return nil, newPluginError("invalid_request",
					fmt.Sprintf("unknown vendor %q", rawVendor), http.StatusBadRequest)
			}
			if !vendorEnabled(vendor) {
				return nil, newPluginError("vendor_disabled",
					fmt.Sprintf("vendor %s is disabled by plugin configuration", vendor.ID()), http.StatusBadRequest)
			}
			return vendor, nil
		}
		// 旧版页面只传 realm：按区域找第一个匹配的供应商。
		if rawRegion, okRegion := metadata["region"].(string); okRegion && strings.TrimSpace(rawRegion) != "" {
			if vendor, okFind := firstVendorForRegion(rawRegion); okFind {
				return vendor, nil
			}
		}
	}
	if vendor, okDefault := firstEnabledVendor(); okDefault {
		return vendor, nil
	}
	return nil, newPluginError("vendor_disabled", "no vendor is enabled by plugin configuration", http.StatusBadRequest)
}

// vendorForLoginSession 按登录会话找供应商。
//
// 会话是供应商自己建的（状态存在各自的包里），因此这里只能遍历询问。
// 会话数很少（同时最多一两个登录在进行），遍历代价可忽略。
func vendorForLoginSession(sessionID string) (core.Vendor, error) {
	// 会话存在哪个供应商里由对方的 LoginPoll 自证（会话 id 只在对方的状态表里）。
	// 因此这里不猜，直接让每个启用的供应商去处理：第一个能认领会话的就是它。
	for _, vendor := range core.Vendors() {
		if !vendorEnabled(vendor) {
			continue
		}
		if provider, okProvider := vendor.(loginSessionOwner); okProvider && provider.OwnsLoginSession(sessionID) {
			return vendor, nil
		}
	}
	// 都不认领（会话已过期）：交给第一个启用的供应商回「会话不存在」的友好错误，
	// 避免把「会话过期」误报成「未知供应商」。
	if vendor, okDefault := firstEnabledVendor(); okDefault {
		return vendor, nil
	}
	return nil, newPluginError("vendor_disabled", "no vendor is enabled by plugin configuration", http.StatusBadRequest)
}

// vendorEnabled 报告供应商的区域是否被配置启用。
func vendorEnabled(vendor core.Vendor) bool {
	return realmEnabled(loadedConfig(), vendor.Region())
}

// firstEnabledVendor 返回配置里第一个启用的供应商。
func firstEnabledVendor() (core.Vendor, bool) {
	for _, vendor := range core.Vendors() {
		if vendorEnabled(vendor) {
			return vendor, true
		}
	}
	return nil, false
}

// firstVendorForRegion 返回某个区域第一个启用的供应商。
func firstVendorForRegion(region string) (core.Vendor, bool) {
	normalized := strings.ToLower(strings.TrimSpace(region))
	for _, vendor := range core.Vendors() {
		if vendor.Region() == normalized && vendorEnabled(vendor) {
			return vendor, true
		}
	}
	return nil, false
}

// loginRegion 从 Metadata 解析目标域。
func loginRegion(metadata map[string]any) workbuddy.Region {
	if metadata != nil {
		if raw, okRaw := metadata["realm"].(string); okRaw && strings.TrimSpace(raw) != "" {
			return workbuddy.NormalizeRegion(raw)
		}
	}
	cfg := loadedConfig()
	if realmEnabled(cfg, string(workbuddy.RegionCN)) {
		return workbuddy.RegionCN
	}
	return workbuddy.RegionGlobal
}

// loginAccount 是登录流程取到的账号信息。
type loginAccount struct {
	uid          string
	nickname     string
	domain       string
	enterpriseID string
}

// requestLoginState 取 state 并构造用户授权链接。
func requestLoginState(ctx context.Context, region workbuddy.Region) (state, authURL string, err error) {
	endpoints := workbuddy.GetEndpoints(region)
	url := endpoints.ChatBase + "/v2/plugin/auth/state?platform=" + loginUAPlatform
	// 上游要求 POST 带 JSON body（空对象即可）；不带 body 时部分节点返回异常。
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if errReq != nil {
		return "", "", fmt.Errorf("build login state request: %w", errReq)
	}
	applyLoginHeaders(req, region)

	raw, errCall := doLoginRequest(req)
	if errCall != nil {
		return "", "", errCall
	}
	var payload struct {
		State string `json:"state"`
		URL   string `json:"url"`
		// 部分形态把链接放在 authUrl / loginUrl 里。
		AuthURL  string `json:"authUrl"`
		LoginURL string `json:"loginUrl"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", "", fmt.Errorf("decode login state: %w", errUnmarshal)
	}
	state = strings.TrimSpace(payload.State)
	authURL = firstNonEmptyString(payload.URL, payload.AuthURL, payload.LoginURL)
	if state == "" || authURL == "" {
		return "", "", fmt.Errorf("login state response is incomplete")
	}
	return state, authURL, nil
}

// exchangeLoginToken 用 state 换 token。
func exchangeLoginToken(ctx context.Context, region workbuddy.Region, state string) (accessToken, refreshToken string, err error) {
	endpoints := workbuddy.GetEndpoints(region)
	url := endpoints.ChatBase + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return "", "", fmt.Errorf("build login token request: %w", errReq)
	}
	applyLoginHeaders(req, region)

	raw, errCall := doLoginRequest(req)
	if errCall != nil {
		return "", "", errCall
	}
	var payload struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", "", fmt.Errorf("decode login token: %w", errUnmarshal)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", "", fmt.Errorf("login token response has no accessToken")
	}
	return strings.TrimSpace(payload.AccessToken), strings.TrimSpace(payload.RefreshToken), nil
}

// fetchLoginAccount 取账号信息。
func fetchLoginAccount(ctx context.Context, region workbuddy.Region, state, accessToken string) (loginAccount, error) {
	endpoints := workbuddy.GetEndpoints(region)
	url := endpoints.ChatBase + "/v2/plugin/login/account?state=" + url.QueryEscape(state)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return loginAccount{}, fmt.Errorf("build login account request: %w", errReq)
	}
	applyLoginHeaders(req, region)
	// 该端点用刚拿到的 token 鉴权（原项目同样带 Bearer）。
	if strings.TrimSpace(accessToken) != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	raw, errCall := doLoginRequest(req)
	if errCall != nil {
		return loginAccount{}, errCall
	}
	var payload struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		Domain       string `json:"domain"`
		EnterpriseID string `json:"enterpriseId"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return loginAccount{}, fmt.Errorf("decode login account: %w", errUnmarshal)
	}
	return loginAccount{
		uid:          strings.TrimSpace(payload.UID),
		nickname:     strings.TrimSpace(payload.Nickname),
		domain:       strings.TrimSpace(payload.Domain),
		enterpriseID: strings.TrimSpace(payload.EnterpriseID),
	}, nil
}

// doLoginRequest 发起一次登录相关请求并解开信封。
func doLoginRequest(req *http.Request) (json.RawMessage, error) {
	client := httpx.Client(req.Context(), loginHTTPTimeout)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("login request: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read login response: %w", errRead)
	}
	if resp.StatusCode >= 400 {
		return nil, workbuddy.Classify(resp.StatusCode, string(body))
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal != nil {
		// 部分登录接口直接返回裸对象（没有信封）。
		return body, nil
	}
	if envelope.Code != 0 {
		// 关键：**不能**把所有非 0 业务码都当成失败。
		// 上游用 11217 + "login ing..." 表示「授权尚未完成」，这是轮询期间的
		// 正常状态；当成失败会让用户看到"授权失败"而实际上只差一步。
		return nil, &workbuddy.Error{
			Kind:   workbuddy.KindClient,
			Status: resp.StatusCode,
			Msg:    fmt.Sprintf("%s %s", strconv.Itoa(envelope.Code), strings.TrimSpace(envelope.Msg)),
		}
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		return envelope.Data, nil
	}
	return body, nil
}

// applyLoginHeaders 写入登录链路的请求头。
// applyLoginHeaders 写入设备授权链路的请求头（与上游客户端一致）。
//
// X-Requested-With 与 Accept 的形态要和官方客户端一致：上游对设备授权
// 端点做 UA/头校验，缺 X-Requested-With 时行为不确定。
func applyLoginHeaders(req *http.Request, region workbuddy.Region) {
	endpoints := workbuddy.GetEndpoints(region)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", endpoints.Origin)
	req.Header.Set("Referer", endpoints.Origin+"/")
	req.Header.Set("User-Agent", loginClientUA)
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// isLoginPending 判断错误是否表示「用户尚未完成授权」。
//
// 上游在待授权期间返回 404 或业务错误，这两种都不该终止轮询。
func isLoginPending(err error) bool {
	if err == nil {
		return false
	}
	var upstreamErr *workbuddy.Error
	if !errors.As(err, &upstreamErr) {
		return false
	}
	if upstreamErr.Status == http.StatusNotFound {
		return true
	}
	message := strings.ToLower(upstreamErr.Msg)
	// 11217 是上游「登录进行中」的业务码；"login ing" 是它的固定文案。
	// 二者都表示用户还没在浏览器里完成授权，应继续轮询而不是报失败。
	if strings.Contains(message, loginPendingCode) {
		return true
	}
	for _, marker := range []string{"login ing", "pending", "not authorized", "waiting", "authorizing"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// loginPendingCode 是上游「授权尚未完成」的业务码。
const loginPendingCode = "11217"

// loginFileName 生成凭证文件名。
//
// 文件名以 workbuddy 开头是必要的：auth.parse 的文件名启发式与
// 后续的凭证归属判定都依赖它。
func loginFileName(credential *workbuddy.Credential) string {
	uid := strings.TrimSpace(credential.UID)
	if uid == "" {
		uid = newLoginSessionID()
	}
	return "workbuddy-" + sanitizeFileComponent(uid) + ".json"
}

// sanitizeFileComponent 把可能含路径分隔符的标识清成安全的文件名片段。
//
// 上游的 uid 理论上受控，但落盘路径绝不能依赖上游数据的"善意"——
// 一个带 ../ 的 uid 就能写到目录之外。
func sanitizeFileComponent(raw string) string {
	var builder strings.Builder
	for _, char := range raw {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '-' || char == '_':
			builder.WriteRune(char)
		}
	}
	out := builder.String()
	if out == "" {
		return newLoginSessionID()
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// defaultDomainFor 返回某域的默认域名。
func defaultDomainFor(region workbuddy.Region) string {
	if region.IsGlobal() {
		return "www.workbuddy.ai"
	}
	return "www.codebuddy.cn"
}

// newLoginSessionID 生成一个随机的登录会话标识。
func newLoginSessionID() string {
	return workbuddy.NewHexID()
}

// gcLoginSessionsLocked 清理过期的登录会话。调用方必须持有 loginStoreMu。
func gcLoginSessionsLocked() {
	// 全量扫描很便宜（会话数是个位数），但没必要每次启动都扫。
	if time.Since(loginStoreLastGC) < time.Minute {
		return
	}
	loginStoreLastGC = time.Now()
	for id, session := range loginStore {
		if time.Since(session.createdAt) > loginSessionTTL {
			delete(loginStore, id)
		}
	}
}
