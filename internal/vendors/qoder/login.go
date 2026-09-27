package qoder

// 本文件实现 Qoder 的 OAuth 设备授权登录（PKCE）。
//
// 流程（与上游 Qoder 桌面端一致）：
//  1. 生成 PKCE verifier/challenge 与 nonce，拼出授权链接（DeviceLoginBase）；
//  2. 用户在浏览器里完成授权；
//  3. 轮询 poll 端点：404 = 还没授权，200 = 拿到 device_token；
//  4. 取账号信息与套餐名，组装凭证 JSON。
//
// 与 WorkBuddy 的差异：WorkBuddy 是自有 state/token 三接口，Qoder 是标准
// PKCE 设备码流。两者形状完全不同，因此登录协议属于供应商层。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/vendors/qoder/bridge"
	"freetier2api-plugin/internal/vendors/qoder/qoderapi"
)

const (
	// oauthClientID 是 Qoder 桌面端使用的公开 client id（与上游实现一致）。
	oauthClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	// loginFlowTTL 是单次登录会话的有效期（与上游 10 分钟一致）。
	loginFlowTTL = 10 * time.Minute
	// loginHTTPTimeout 是登录相关请求的上限。
	loginHTTPTimeout = 30 * time.Second
	// loginMaxResponseBytes 是登录响应的读取上限。
	loginMaxResponseBytes = 1 << 20
	// loginPollInterval 是同一会话两次访问上游 poll 端点之间的最小间隔。
	//
	// 控制台页的轮询频率远高于上游建议（上游客户端是 1 秒一次），
	// 不节流会让面板一开就把上游打满。
	loginPollInterval = 1200 * time.Millisecond
)

// pendingLogin 是一次进行中的登录会话。
type pendingLogin struct {
	state    string
	nonce    string
	verifier string
	region   qoderapi.Region
	deadline time.Time
	lastPoll time.Time
}

var loginStore = struct {
	mu      sync.Mutex
	pending map[string]*pendingLogin
}{pending: map[string]*pendingLogin{}}

// LoginProfile 是账号信息（取不到时保持零值）。
type LoginProfile struct {
	UserID string
	Email  string
	Name   string
	Type   string
}

// LoginStart 生成 PKCE 参数与登录 URL，并登记一个待轮询的登录会话。
func LoginStart(ctx context.Context, region qoderapi.Region) (loginURL, state string, expiresAt time.Time, err error) {
	verifier, challenge, errPKCE := newPKCEPair()
	if errPKCE != nil {
		return "", "", time.Time{}, errPKCE
	}
	nonce, errNonce := randomHex(16)
	if errNonce != nil {
		return "", "", time.Time{}, errNonce
	}
	state, errState := randomHex(16)
	if errState != nil {
		return "", "", time.Time{}, errState
	}

	endpoints := qoderapi.GetEndpoints(region)
	params := url.Values{}
	params.Set("nonce", nonce)
	params.Set("challenge", challenge)
	params.Set("challenge_method", "S256")
	params.Set("client_id", oauthClientID)
	loginURL = endpoints.DeviceLoginBase + "?" + params.Encode()

	deadline := time.Now().Add(loginFlowTTL)
	storePending(&pendingLogin{
		state:    state,
		nonce:    nonce,
		verifier: verifier,
		region:   region,
		deadline: deadline,
	})
	return loginURL, state, deadline, nil
}

// LoginPollResult 是一次轮询的结果。
type LoginPollResult struct {
	// Pending 为 true 表示还在等用户授权，调用方应继续轮询。
	Pending bool
	// Token / RefreshToken / Profile 在拿到凭证时有效。
	Token        string
	RefreshToken string
	Profile      LoginProfile
	// Region 是本次登录的区域（取凭证时要用它拼落盘内容）。
	Region qoderapi.Region
}

// LoginPoll 轮询一次登录会话。
//
// 返回错误表示会话已失效（不存在或超时），调用方应终止轮询；
// 返回 Pending=true 表示用户还没完成授权，继续轮询即可。
func LoginPoll(ctx context.Context, state string) (LoginPollResult, error) {
	entry, okEntry := peekPending(state)
	if !okEntry {
		return LoginPollResult{}, fmt.Errorf("登录会话不存在或已过期，请重新发起登录")
	}
	if time.Now().After(entry.deadline) {
		dropPending(state)
		return LoginPollResult{}, fmt.Errorf("登录超时：10 分钟内未完成授权，请重新发起登录")
	}
	// 节流：面板轮询频率高于上游建议（上游客户端是 1 秒一次）。
	if time.Since(entry.lastPoll) < loginPollInterval {
		return LoginPollResult{Pending: true}, nil
	}
	markPolled(entry)

	token, refreshToken, waiting, errPoll := pollDeviceToken(ctx, entry)
	if errPoll != nil {
		// 网络抖动不该直接判死：保持会话，下次轮询再试。
		return LoginPollResult{Pending: true}, nil
	}
	if waiting {
		return LoginPollResult{Pending: true}, nil
	}

	profile := fetchLoginProfile(ctx, token, entry.region)
	dropPending(state)
	return LoginPollResult{
		Token:        token,
		RefreshToken: refreshToken,
		Profile:      profile,
		Region:       entry.region,
	}, nil
}

// OwnsLoginSession 报告该会话是否由本供应商创建且仍有效。
func OwnsLoginSession(sessionID string) bool {
	_, okEntry := peekPending(sessionID)
	return okEntry
}

// pollDeviceToken 访问上游 poll 端点。
//
// waiting=true 表示用户尚未授权（上游 404），调用方应继续等待。
func pollDeviceToken(ctx context.Context, entry *pendingLogin) (token, refreshToken string, waiting bool, err error) {
	endpoints := qoderapi.GetEndpoints(entry.region)
	pollURL := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		endpoints.PollEndpoint, url.QueryEscape(entry.nonce), url.QueryEscape(entry.verifier))
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if errRequest != nil {
		return "", "", false, errRequest
	}
	resp, errDo := httpx.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", false, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, loginMaxResponseBytes))

	switch resp.StatusCode {
	case http.StatusNotFound:
		// 上游语义：用户还没授权。
		return "", "", true, nil
	case http.StatusOK:
	default:
		return "", "", false, fmt.Errorf("上游 poll 返回 HTTP %d：%s", resp.StatusCode, summarizeBody(body))
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return "", "", false, fmt.Errorf("上游 poll 响应不是合法 JSON：%w", errUnmarshal)
	}
	deviceToken, _ := payload["token"].(string)
	deviceToken = strings.TrimSpace(deviceToken)
	if deviceToken == "" {
		return "", "", false, fmt.Errorf("上游 poll 响应里没有 token")
	}
	refreshToken, _ = payload["refresh_token"].(string)
	return deviceToken, strings.TrimSpace(refreshToken), false, nil
}

// fetchLoginProfile 取账号信息（失败只降级：没有 email/name 也能用）。
func fetchLoginProfile(ctx context.Context, token string, region qoderapi.Region) LoginProfile {
	endpoints := qoderapi.GetEndpoints(region)
	result, errProfile := httpGetBearerJSON(ctx, endpoints.UserinfoBase, token)
	if errProfile != nil {
		return LoginProfile{}
	}
	return LoginProfile{
		UserID: stringField(result, "userId"),
		Email:  strings.ToLower(stringField(result, "email")),
		Name:   stringField(result, "name"),
		Type:   stringField(result, "userType"),
	}
}

// FetchPlanName 取套餐名（失败留空）。
func FetchPlanName(ctx context.Context, token string, region qoderapi.Region) string {
	endpoints := qoderapi.GetEndpoints(region)
	result, errPlan := httpGetBearerJSON(ctx, endpoints.PlanEndpoint, token)
	if errPlan != nil {
		return ""
	}
	return stringField(result, "plan_tier_name")
}

// BuildLoginStorage 生成落盘用的凭证 JSON（扁平格式）。
//
// 与导入路径同构：刷新时代码（auth.refresh）能原样读回这些字段。
func BuildLoginStorage(region qoderapi.Region, token, refreshToken string, profile LoginProfile, plan string) ([]byte, *Credential) {
	payload := map[string]any{
		"device_token": token,
		"region":       string(region),
		"auth_mode":    "oauth",
	}
	if refreshToken != "" {
		payload["refresh_token"] = refreshToken
	}
	label := profile.Name
	if label == "" {
		label = profile.Email
	}
	if label != "" {
		payload["label"] = label
	}
	if profile.Email != "" {
		payload["email"] = profile.Email
	}
	if plan != "" {
		payload["plan"] = plan
	}
	if profile.UserID != "" {
		payload["qoder_account_id"] = profile.UserID
	}
	if profile.Type != "" {
		payload["qoder_user_type"] = profile.Type
	}
	storage, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		// map 里只有字符串，理论上不会失败；退化成紧凑编码保证登录不中断。
		storage, _ = json.Marshal(payload)
	}
	return storage, &Credential{
		Token:        token,
		RefreshToken: refreshToken,
		Region:       region,
		Label:        label,
		Email:        profile.Email,
	}
}

// LoginAuthID 生成宿主侧 auth 标识（会用于落盘文件名，必须是文件名安全字符）。
func LoginAuthID(profile LoginProfile, state string) string {
	base := profile.Email
	if base == "" {
		base = profile.UserID
	}
	if base == "" {
		base = "login-" + shortState(state)
	}
	return "qoder-" + sanitizeAuthID(base)
}

// ---- 会话存储 ----

func storePending(entry *pendingLogin) {
	loginStore.mu.Lock()
	defer loginStore.mu.Unlock()
	gcPendingLocked()
	loginStore.pending[entry.state] = entry
}

// peekPending 取出会话（不改动轮询时间）。
func peekPending(state string) (*pendingLogin, bool) {
	loginStore.mu.Lock()
	defer loginStore.mu.Unlock()
	entry, okEntry := loginStore.pending[strings.TrimSpace(state)]
	return entry, okEntry
}

func markPolled(entry *pendingLogin) {
	loginStore.mu.Lock()
	defer loginStore.mu.Unlock()
	entry.lastPoll = time.Now()
}

func dropPending(state string) {
	loginStore.mu.Lock()
	defer loginStore.mu.Unlock()
	delete(loginStore.pending, strings.TrimSpace(state))
}

// gcPendingLocked 清理过期会话。调用方必须持有锁。
func gcPendingLocked() {
	now := time.Now()
	for state, entry := range loginStore.pending {
		if now.After(entry.deadline) {
			delete(loginStore.pending, state)
		}
	}
}

// ---- 工具 ----

// newPKCEPair 生成 PKCE 的 verifier 与 challenge（S256）。
func newPKCEPair() (verifier, challenge string, err error) {
	buffer := make([]byte, 32)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return "", "", errRead
	}
	verifier = base64.RawURLEncoding.EncodeToString(buffer)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// randomHex 生成 n 字节的随机十六进制串。
func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return "", errRead
	}
	return hex.EncodeToString(buffer), nil
}

// shortState 截断 state 用于日志（完整 state 是凭证材料的一部分）。
func shortState(state string) string {
	if len(state) <= 8 {
		return state
	}
	return state[:8]
}

// sanitizeAuthID 只保留文件名安全字符。
func sanitizeAuthID(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.', r == '_', r == '-', r == '@':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	out := strings.Trim(builder.String(), "-.@")
	if out == "" {
		out = "account"
	}
	return out
}

// stringField 读一个字符串字段并去空白。
func stringField(payload map[string]any, key string) string {
	value, okValue := payload[key].(string)
	if !okValue {
		return ""
	}
	return strings.TrimSpace(value)
}

// summarizeBody 截断响应正文用于错误信息。
func summarizeBody(body []byte) string {
	return truncateForMessage(string(body), 200)
}

// 编译期确认 bridge 的凭证解析在本包可用（登录落盘要用它解回 token）。
var _ = bridge.ParseOAuthSecret
