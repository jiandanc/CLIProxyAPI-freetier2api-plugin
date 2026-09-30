package minimaxcode

import (
	"bytes"
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

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
)

var (
	pendingMu       sync.Mutex
	pendingSessions = make(map[string]*PendingLogin)
)

// PendingLogin 记录进行中的 OAuth 设备码授权会话。
type PendingLogin struct {
	SessionID  string
	Region     Region
	DeviceCode string
	Verifier   string
	UserCode   string
	VerifyURL  string
	Interval   time.Duration
	LastPoll   time.Time
	ExpiresAt  time.Time
}

func pkcePair() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LoginStart 向 MiniMax 账号服务申请 OAuth2 设备码。
func LoginStart(ctx context.Context, r Region) (*pluginapi.AuthLoginStartResponse, error) {
	verifier, challenge, errPKCE := pkcePair()
	if errPKCE != nil {
		return nil, fmt.Errorf("generate PKCE: %w", errPKCE)
	}

	accountHost := AccountHostFor(r)
	codeURL := accountHost + EpDeviceCode

	reqBody, _ := json.Marshal(map[string]any{
		"client_id":             ClientID,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"scope":                 OAuthScope,
		"audience":              OAuthAudience,
	})

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, codeURL, bytes.NewReader(reqBody))
	if errReq != nil {
		return nil, fmt.Errorf("create device code request: %w", errReq)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := httpx.Client(ctx, 30*time.Second).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request device code: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&payload); errDec != nil {
		return nil, fmt.Errorf("decode device code response: %w", errDec)
	}

	deviceCode := getString(payload, "device_code")
	userCode := getString(payload, "user_code")
	if deviceCode == "" {
		return nil, fmt.Errorf("device code response missing device_code: %v", payload)
	}

	verifyURL := firstNonEmpty(
		getString(payload, "verification_uri_complete"),
		getString(payload, "verification_uri"),
		fmt.Sprintf("%s/oauth-authorize?user_code=%s", accountHost, userCode),
	)

	intervalSec := getInt(payload, "interval")
	if intervalSec < 3 {
		intervalSec = 3
	}
	expiresInSec := getInt(payload, "expires_in")
	if expiresInSec <= 0 {
		expiresInSec = 300
	}

	sessionID := "minimaxcode_" + string(r) + "_" + randomHex(8)
	expiresAt := time.Now().Add(time.Duration(expiresInSec) * time.Second)

	pending := &PendingLogin{
		SessionID:  sessionID,
		Region:     r,
		DeviceCode: deviceCode,
		Verifier:   verifier,
		UserCode:   userCode,
		VerifyURL:  verifyURL,
		Interval:   time.Duration(intervalSec) * time.Second,
		LastPoll:   time.Now(),
		ExpiresAt:  expiresAt,
	}

	pendingMu.Lock()
	pendingSessions[sessionID] = pending
	pendingMu.Unlock()

	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       verifyURL,
		State:     sessionID,
		ExpiresAt: expiresAt,
		Metadata: map[string]any{
			"user_code":    userCode,
			core.VendorKey: VendorIDFor(r),
			"region":       string(r),
		},
	}, nil
}

// LoginPoll 轮询设备码授权状态。
func LoginPoll(ctx context.Context, state string, baseURLOverride string) (*pluginapi.AuthLoginPollResponse, error) {
	cleanState := strings.TrimSpace(state)
	pendingMu.Lock()
	session, exists := pendingSessions[cleanState]
	if !exists {
		pendingMu.Unlock()
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	if time.Now().After(session.ExpiresAt) {
		delete(pendingSessions, cleanState)
		pendingMu.Unlock()
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话已超时，请重新发起登录",
		}, nil
	}

	// 轮询节流防踩限流
	if time.Since(session.LastPoll) < session.Interval {
		pendingMu.Unlock()
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}
	session.LastPoll = time.Now()
	pendingMu.Unlock()

	accountHost := AccountHostFor(session.Region)
	tokenURL := accountHost + EpToken

	data := url.Values{}
	data.Set("client_id", ClientID)
	data.Set("grant_type", DeviceGrantType)
	data.Set("device_code", session.DeviceCode)
	data.Set("code_verifier", session.Verifier)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if errReq != nil {
		return nil, fmt.Errorf("create poll request: %w", errReq)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, errDo := httpx.Client(ctx, 30*time.Second).Do(req)
	if errDo != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "网络请求重试中: " + errDo.Error(),
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	var payload map[string]any
	rawBytes, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(rawBytes, &payload)

	errCode := getString(payload, "error")
	switch errCode {
	case "authorization_pending":
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	case "slow_down":
		pendingMu.Lock()
		session.Interval += 2 * time.Second
		pendingMu.Unlock()
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	case "expired_token":
		pendingMu.Lock()
		delete(pendingSessions, cleanState)
		pendingMu.Unlock()
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "授权码已超时失效，请重新发起登录",
		}, nil
	case "access_denied":
		pendingMu.Lock()
		delete(pendingSessions, cleanState)
		pendingMu.Unlock()
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "用户在浏览器中拒绝了授权",
		}, nil
	}

	if resp.StatusCode >= 400 || errCode != "" {
		pendingMu.Lock()
		delete(pendingSessions, cleanState)
		pendingMu.Unlock()
		errDesc := firstNonEmpty(getString(payload, "error_description"), errCode, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "授权失败：" + errDesc,
		}, nil
	}

	accessToken := getString(payload, "access_token", "accessToken")
	if accessToken == "" {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}

	// 授权成功，清理会话并构建凭据
	pendingMu.Lock()
	delete(pendingSessions, cleanState)
	pendingMu.Unlock()

	refreshToken := getString(payload, "refresh_token", "refreshToken")
	expiresIn := getInt64(payload, "expires_in", "expiresIn")
	if expiresIn <= 0 {
		expiresIn = 3600
	}

	cred := &Credential{
		Vendor:       VendorIDFor(session.Region),
		Region:       session.Region,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().Unix() + expiresIn,
		AuthMode:     "oauth",
	}
	cred.EnsureIdentifiers()

	// 探测用户真实 ID 与 Agent ID
	_, _, _, _ = fetchUserInfo(ctx, cred, baseURLOverride)
	_ = prepare(ctx, cred, baseURLOverride)

	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "生成凭证失败：" + errStorage.Error(),
		}, nil
	}

	accountIdentifier := firstNonEmpty(cred.UserID, cred.Email, session.UserCode, randomHex(4))
	label := cred.Label
	if label == "" {
		label = VendorNameFor(session.Region) + "-" + accountIdentifier
	}

	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(VendorIDFor(session.Region), accountIdentifier),
			Label:       label,
			StorageJSON: storageJSON,
		},
	}, nil
}

// OwnsLoginSession 检查 sessionID 是否属于本 MiniMax Code 供应商实例。
func OwnsLoginSession(sessionID string, r Region) bool {
	prefix := "minimaxcode_" + string(r) + "_"
	if strings.HasPrefix(strings.ToLower(sessionID), prefix) {
		return true
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if s, ok := pendingSessions[sessionID]; ok && s.Region == r {
		return true
	}
	return false
}
