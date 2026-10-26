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
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// PendingLogin 是一次进行中的登录会话数据。
//
// TTL 与轮询节流由 internal/auth 的会话表统一管理，这里只保留协议状态。
type PendingLogin struct {
	Region Region
	State  string
}

// StoreLoginSession 登记一次进行中的登录会话（TTL 与节流交给共享登记表）。
func StoreLoginSession(logins core.LoginSessions, sessionID string, session *PendingLogin) {
	if session == nil {
		return
	}
	logins.Register(VendorIDFor(session.Region), sessionID, LoginSessionTTL, loginPollInterval, session)
}

// RemoveLoginSession 登录成功或终止后清理会话。
func RemoveLoginSession(logins core.LoginSessions, sessionID string) {
	logins.Delete(sessionID)
}

// CheckLoginSession 检查登录会话状态与节流。
// 返回 (session, exists, shouldPoll):
//   - exists: 会话是否存在且在 TTL 内；
//   - shouldPoll: 是否已过节流间隔，可以向上游发起 token 请求。
func CheckLoginSession(logins core.LoginSessions, sessionID string) (session *PendingLogin, exists bool, shouldPoll bool) {
	entry, pollState := logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return nil, false, false
	}
	pending, okPending := entry.Data.(*PendingLogin)
	if !okPending {
		return nil, false, false
	}
	return pending, true, pollState == core.PollReady
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
	authURL = core.FirstNonEmpty(payload.URL, payload.AuthURL, payload.LoginURL)
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
	client := hostbridge.Client(req.Context(), loginHTTPTimeout)
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
		return nil, enrichClassifyError(resp.StatusCode, string(body), resp.Header)
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
		return nil, &core.Failure{
			Kind:   core.FailureKindClient,
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
	var upstreamErr *core.Failure
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

// RefreshToken 刷新凭证的 access token。
//
// 安全红线：X-Refresh-Token 头**只允许出现在刷新端点**，对话请求绝不能携带。
//
// 并发采用两段式：锁内取快照 → 锁外发请求 → 锁内校验快照未被并发改动后再写回。
func (c *Client) RefreshToken(cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("credential is nil")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	refreshToken := cred.RefreshTokenValue()
	if strings.TrimSpace(refreshToken) == "" {
		return fmt.Errorf("credential has no refresh token")
	}
	accessBefore, refreshBefore := cred.Snapshot()

	endpoints := GetEndpoints(region)
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost,
		endpoints.ChatBase+tokenRefreshPath, nil)
	if errReq != nil {
		return fmt.Errorf("build refresh request: %w", errReq)
	}
	applyCommonHeaders(req, c, cred)
	req.Header.Set("X-Refresh-Token", refreshToken)
	req.Header.Set("X-Auth-Refresh-Source", authRefreshSource)

	resp, errDo := c.refreshClient().Do(req)
	if errDo != nil {
		return fmt.Errorf("refresh request: %w", errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
	if errRead != nil {
		return fmt.Errorf("read refresh response: %w", errRead)
	}
	if resp.StatusCode >= 400 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	}

	var envelope struct {
		Code int                  `json:"code"`
		Msg  string               `json:"msg"`
		Data refreshTokenResponse `json:"data"`
	}
	var token refreshTokenResponse
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal == nil && envelope.Code == 0 && strings.TrimSpace(envelope.Data.AccessToken) != "" {
		token = envelope.Data
	} else if errUnmarshal == nil && envelope.Code != 0 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	} else if errDirect := json.Unmarshal(body, &token); errDirect != nil || strings.TrimSpace(token.AccessToken) == "" {
		return &core.Failure{Kind: core.FailureKindSessionDead, Status: http.StatusUnauthorized, Msg: "refresh failed: no accessToken in response — re-login required"}
	}

	// 快照校验：并发刷新已经写过就放弃本次写回，避免用旧响应覆盖新值。
	if !cred.UnchangedSince(accessBefore, refreshBefore) {
		return nil
	}
	var expiresAt int64
	if token.ExpiresIn > 0 && token.ExpiresIn <= maxRefreshTokenExpiresIn {
		expiresAt = time.Now().Unix() + token.ExpiresIn
	}
	cred.ApplyTokenRefresh(token.AccessToken, token.RefreshToken, token.Domain, expiresAt)
	if cred.FilePath != "" {
		if errSave := SaveCredentialFile(cred.FilePath, cred); errSave != nil {
			return fmt.Errorf("save refreshed credential: %w", errSave)
		}
	}
	return nil
}
