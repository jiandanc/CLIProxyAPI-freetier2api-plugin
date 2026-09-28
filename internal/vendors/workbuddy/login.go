package workbuddy

// 本文件实现 WorkBuddy 的设备授权登录流程。
//
// 流程（与原项目的 cmd/login 一致）：
//  1. 向 /v2/plugin/auth/state 取 state，拿到用户需要打开的授权链接；
//  2. 用户在浏览器里完成授权；
//  3. 轮询 /v2/plugin/auth/token 换 token，再用 /v2/plugin/login/account 取账号信息。
//
// 宿主只列它自己硬编码的 OAuth provider，插件不出现在那份列表里，
// 因此登录入口由插件的控制台页提供（见根层的 auth_login.go）。

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

	"freetier2api-plugin/internal/httpx"
)

// PendingLogin 是一次进行中的登录会话。
type PendingLogin struct {
	Region    Region
	State     string
	CreatedAt time.Time
	// LastPoll 记录上次向上游轮询的时刻（节流）。
	LastPoll time.Time
}

var (
	loginStoreMu     sync.Mutex
	loginStore       = map[string]*PendingLogin{}
	loginPollMinGap  = loginPollInterval
	loginStoreLastGC time.Time
)

// storeLoginSession 记录一次进行中的登录会话（含过期 GC）。
func StoreLoginSession(sessionID string, session *PendingLogin) {
	loginStoreMu.Lock()
	gcLoginSessionsLocked()
	loginStore[sessionID] = session
	loginStoreMu.Unlock()
}

// OwnsLoginSession 报告该会话是否由 WorkBuddy 持有且仍有效。
func OwnsLoginSession(sessionID string) bool {
	loginStoreMu.Lock()
	defer loginStoreMu.Unlock()
	session, okSession := loginStore[sessionID]
	if okSession && time.Since(session.CreatedAt) > LoginSessionTTL {
		delete(loginStore, sessionID)
		return false
	}
	return okSession
}

// RemoveLoginSession 登录成功或终止后清理会话。
func RemoveLoginSession(sessionID string) {
	loginStoreMu.Lock()
	delete(loginStore, sessionID)
	loginStoreMu.Unlock()
}

// CheckLoginSession 检查登录会话状态与节流。
// 返回 (session, exists, shouldPoll):
//   - exists: 会话是否存在且在 TTL 内；
//   - shouldPoll: 是否已过节流间隔，可以向上游发起 token 请求。
func CheckLoginSession(sessionID string) (session *PendingLogin, exists bool, shouldPoll bool) {
	loginStoreMu.Lock()
	defer loginStoreMu.Unlock()
	s, okSession := loginStore[sessionID]
	if !okSession {
		return nil, false, false
	}
	if time.Since(s.CreatedAt) > LoginSessionTTL {
		delete(loginStore, sessionID)
		return nil, false, false
	}
	if time.Since(s.LastPoll) < loginPollMinGap {
		return s, true, false
	}
	s.LastPoll = time.Now()
	return s, true, true
}

// TakeLoginSession 取出登录会话，同时应用节流（兼容旧调用）。
func TakeLoginSession(sessionID string) (*PendingLogin, bool) {
	s, exists, shouldPoll := CheckLoginSession(sessionID)
	if !exists {
		return nil, false
	}
	return s, shouldPoll
}

// loginAccount 是登录流程取到的账号信息。
type LoginAccount struct {
	UID          string
	Nickname     string
	Domain       string
	EnterpriseID string
}

// requestLoginState 取 state 并构造用户授权链接。
func RequestLoginState(ctx context.Context, region Region) (state, authURL string, err error) {
	endpoints := GetEndpoints(region)
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
func ExchangeLoginToken(ctx context.Context, region Region, state string) (accessToken, refreshToken string, err error) {
	endpoints := GetEndpoints(region)
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
func FetchLoginAccount(ctx context.Context, region Region, state, accessToken string) (LoginAccount, error) {
	endpoints := GetEndpoints(region)
	url := endpoints.ChatBase + "/v2/plugin/login/account?state=" + url.QueryEscape(state)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return LoginAccount{}, fmt.Errorf("build login account request: %w", errReq)
	}
	applyLoginHeaders(req, region)
	// 该端点用刚拿到的 token 鉴权（原项目同样带 Bearer）。
	if strings.TrimSpace(accessToken) != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	raw, errCall := doLoginRequest(req)
	if errCall != nil {
		return LoginAccount{}, errCall
	}
	var payload struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		Domain       string `json:"domain"`
		EnterpriseID string `json:"enterpriseId"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return LoginAccount{}, fmt.Errorf("decode login account: %w", errUnmarshal)
	}
	return LoginAccount{
		UID:          strings.TrimSpace(payload.UID),
		Nickname:     strings.TrimSpace(payload.Nickname),
		Domain:       strings.TrimSpace(payload.Domain),
		EnterpriseID: strings.TrimSpace(payload.EnterpriseID),
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
		return nil, Classify(resp.StatusCode, string(body))
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
		return nil, &Error{
			Kind:   KindClient,
			Status: resp.StatusCode,
			Msg:    fmt.Sprintf("%s %s", strconv.Itoa(envelope.Code), strings.TrimSpace(envelope.Msg)),
		}
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		return envelope.Data, nil
	}
	return body, nil
}

// applyLoginHeaders 写入设备授权链路的请求头（与上游客户端一致）。
//
// X-Requested-With 与 Accept 的形态要和官方客户端一致：上游对设备授权
// 端点做 UA/头校验，缺 X-Requested-With 时行为不确定。
func applyLoginHeaders(req *http.Request, region Region) {
	endpoints := GetEndpoints(region)
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
func IsLoginPending(err error) bool {
	if err == nil {
		return false
	}
	var upstreamErr *Error
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

// 登录链路的常量（原先是根层的私有常量，随登录逻辑一起迁入本包）。
const (
	// LoginSessionTTL 是登录会话的有效期。
	LoginSessionTTL = 15 * time.Minute
	// loginPollInterval 是插件侧轮询上游的最小间隔（防止过于频繁）。
	loginPollInterval = 2 * time.Second
	// loginHTTPTimeout 是单次登录相关请求的上限。
	loginHTTPTimeout = 30 * time.Second
	// loginUAPlatform 是登录链路的平台标识。
	loginUAPlatform = "CLI"
	// loginClientUA 是登录链路的 UA。
	loginClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// DefaultDomainFor 返回某域的默认域名。
func DefaultDomainFor(region Region) string {
	if region.IsGlobal() {
		return "www.workbuddy.ai"
	}
	return "www.codebuddy.cn"
}

// gcLoginSessionsLocked 清理过期的登录会话。调用方必须持有 loginStoreMu。
func gcLoginSessionsLocked() {
	// 全量扫描很便宜（会话数是个位数），但没必要每次启动都扫。
	if time.Since(loginStoreLastGC) < time.Minute {
		return
	}
	loginStoreLastGC = time.Now()
	for id, session := range loginStore {
		if time.Since(session.CreatedAt) > LoginSessionTTL {
			delete(loginStore, id)
		}
	}
}

// ResetLoginStoreForTest 清空登录会话（测试用）。
//
// 登录会话是包级的：测试之间不清空会让上一个用例的会话泄漏到下一个，
// 表现为「随便一个 state 都能命中旧会话」。
func ResetLoginStoreForTest() {
	loginStoreMu.Lock()
	defer loginStoreMu.Unlock()
	loginStore = map[string]*PendingLogin{}
	loginStoreLastGC = time.Time{}
}

// firstNonEmptyString 返回第一个非空字符串。
func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
