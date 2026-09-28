package zcode

// 本文件实现 ZCode 的 OAuth CLI 登录流程与 API Key 自动兑换。
// 参考：D:\Workspace\zcode2api\app\oauth.py

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/logger"
)

type pendingLogin struct {
	FlowID    string
	PollToken string
	CreatedAt time.Time
}

var (
	loginMu    sync.Mutex
	loginStore = map[string]*pendingLogin{}
)

// LoginStart 发起 ZCode OAuth 授权，返回用户需在浏览器中打开的链接。
func LoginStart(ctx context.Context, apiBase string) (*pluginapi.AuthLoginStartResponse, error) {
	if apiBase == "" {
		apiBase = DefaultZCodeOrigin + "/api/v1"
	}
	apiBase = strings.TrimRight(apiBase, "/")

	pollToken := randomHex(32)
	payload := map[string]string{"provider": "zai"}
	bodyBytes, _ := json.Marshal(payload)

	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+PathOAuthInit, bytes.NewReader(bodyBytes))
	if errNew != nil {
		return nil, fmt.Errorf("create oauth init request: %w", errNew)
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Content-Type", "application/json")

	client := httpx.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("oauth init request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}

	var res struct {
		Code int `json:"code"`
		Data struct {
			FlowID       string `json:"flow_id"`
			AuthorizeURL string `json:"authorize_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode oauth init response: %w", err)
	}

	if res.Data.FlowID == "" || res.Data.AuthorizeURL == "" {
		return nil, fmt.Errorf("incomplete oauth init response")
	}

	sessionID := "zcode_" + randomHex(8)
	loginMu.Lock()
	gcLoginSessionsLocked()
	loginStore[sessionID] = &pendingLogin{
		FlowID:    res.Data.FlowID,
		PollToken: pollToken,
		CreatedAt: time.Now(),
	}
	loginMu.Unlock()

	return &pluginapi.AuthLoginStartResponse{
		Provider:  "freetier",
		URL:       res.Data.AuthorizeURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

// LoginPoll 轮询 OAuth 授权状态。
func LoginPoll(ctx context.Context, apiBase, sessionID string) (*pluginapi.AuthLoginPollResponse, error) {
	if apiBase == "" {
		apiBase = DefaultZCodeOrigin + "/api/v1"
	}
	apiBase = strings.TrimRight(apiBase, "/")

	loginMu.Lock()
	session, ok := loginStore[sessionID]
	loginMu.Unlock()

	if !ok || time.Since(session.CreatedAt) > 15*time.Minute {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}

	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+PathOAuthPoll+"/"+session.FlowID, nil)
	if errNew != nil {
		return nil, fmt.Errorf("create poll request: %w", errNew)
	}
	req.Header.Set("Authorization", "Bearer "+session.PollToken)

	client := httpx.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}

	var res struct {
		Code int `json:"code"`
		Data struct {
			Status      string `json:"status"`
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}

	token := res.Data.AccessToken
	if token == "" || res.Data.Status == "pending" {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中完成登录...",
		}, nil
	}

	// 授权成功：清理进行中会话
	loginMu.Lock()
	delete(loginStore, sessionID)
	loginMu.Unlock()

	// 尝试兑换业务 API Key（免验证码通道）
	apiKey, errEx := ExchangeAPIKey(ctx, token)
	if errEx != nil {
		logger.Info("zcode: exchange api key skipped (%v), using jwt token", errEx)
	}

	deviceMID := randomUUID()
	label := "ZCode-" + MaskedKey(firstNonEmpty(apiKey, token))

	cred := &Credential{
		Vendor:    VendorID,
		APIKey:    apiKey,
		JWTToken:  token,
		DeviceMID: deviceMID,
		Label:     label,
	}

	rawStorage, errJSON := cred.StorageJSON()
	if errJSON != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "生成凭证失败: " + errJSON.Error(),
		}, nil
	}

	fileName := "zcode-" + MaskedKey(firstNonEmpty(apiKey, token)) + ".json"

	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth: pluginapi.AuthData{
			Provider:    "freetier",
			ID:          strings.TrimSuffix(fileName, ".json"),
			FileName:    fileName,
			Label:       label,
			StorageJSON: rawStorage,
		},
	}, nil
}

// ExchangeAPIKey 把 OAuth access_token 兑换为业务 API Key。
func ExchangeAPIKey(ctx context.Context, accessToken string) (string, error) {
	client := httpx.Client(ctx, 30*time.Second)

	// 1. POST /api/auth/z/login 用 access_token 换业务 token
	loginBody, _ := json.Marshal(map[string]string{"token": accessToken})
	reqLogin, errReqLogin := http.NewRequestWithContext(ctx, http.MethodPost, DefaultAPIBase+PathZLogin, bytes.NewReader(loginBody))
	if errReqLogin != nil {
		return "", errReqLogin
	}
	reqLogin.Header.Set("Content-Type", "application/json")

	respLogin, errLogin := client.Do(reqLogin)
	if errLogin != nil {
		return "", errLogin
	}
	defer func() { _ = respLogin.Body.Close() }()

	bodyLogin, _ := io.ReadAll(io.LimitReader(respLogin.Body, 1<<20))
	if respLogin.StatusCode >= 400 {
		return "", fmt.Errorf("z/login error %d: %s", respLogin.StatusCode, string(bodyLogin))
	}

	var resLogin struct {
		Code int `json:"code"`
		Data struct {
			AccessToken    string `json:"access_token"`
			AccessTokenAlt string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bodyLogin, &resLogin); err != nil {
		return "", err
	}
	bizToken := firstNonEmpty(resLogin.Data.AccessToken, resLogin.Data.AccessTokenAlt)
	if bizToken == "" {
		return "", fmt.Errorf("no biz access token in response")
	}

	// 2. GET /api/biz/customer/getCustomerInfo 查找机构与项目 ID
	reqInfo, errReqInfo := http.NewRequestWithContext(ctx, http.MethodGet, DefaultAPIBase+PathCustomerInfo, nil)
	if errReqInfo != nil {
		return "", errReqInfo
	}
	reqInfo.Header.Set("Authorization", "Bearer "+bizToken)

	respInfo, errInfo := client.Do(reqInfo)
	if errInfo != nil {
		return "", errInfo
	}
	defer func() { _ = respInfo.Body.Close() }()

	bodyInfo, _ := io.ReadAll(io.LimitReader(respInfo.Body, 1<<20))
	if respInfo.StatusCode >= 400 {
		return "", fmt.Errorf("customer info error %d", respInfo.StatusCode)
	}

	var resInfo struct {
		Data struct {
			Organizations []struct {
				OrganizationID   string `json:"organizationId"`
				OrganizationName string `json:"organizationName"`
				Projects         []struct {
					ProjectID   string `json:"projectId"`
					ProjectName string `json:"projectName"`
				} `json:"projects"`
			} `json:"organizations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bodyInfo, &resInfo); err != nil || len(resInfo.Data.Organizations) == 0 {
		return "", fmt.Errorf("no organization found")
	}

	orgID := resInfo.Data.Organizations[0].OrganizationID
	if len(resInfo.Data.Organizations[0].Projects) == 0 {
		return "", fmt.Errorf("no project found")
	}
	projID := resInfo.Data.Organizations[0].Projects[0].ProjectID

	// 3. 查找或创建 API Key
	keyURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys", DefaultAPIBase, orgID, projID)

	reqKeys, _ := http.NewRequestWithContext(ctx, http.MethodGet, keyURL, nil)
	reqKeys.Header.Set("Authorization", "Bearer "+bizToken)
	respKeys, errKeys := client.Do(reqKeys)
	if errKeys == nil {
		defer func() { _ = respKeys.Body.Close() }()
		bodyKeys, _ := io.ReadAll(io.LimitReader(respKeys.Body, 1<<20))
		var resKeys struct {
			Data []struct {
				Name string `json:"name"`
				Key  string `json:"key"`
			} `json:"data"`
		}
		if json.Unmarshal(bodyKeys, &resKeys) == nil {
			for _, k := range resKeys.Data {
				if k.Key != "" {
					return k.Key, nil
				}
			}
		}
	}

	// 尝试新建一个 key
	createBody, _ := json.Marshal(map[string]string{"name": "zcode-api-key"})
	reqCreate, _ := http.NewRequestWithContext(ctx, http.MethodPost, keyURL, bytes.NewReader(createBody))
	reqCreate.Header.Set("Authorization", "Bearer "+bizToken)
	reqCreate.Header.Set("Content-Type", "application/json")
	respCreate, errCreate := client.Do(reqCreate)
	if errCreate != nil {
		return "", errCreate
	}
	defer func() { _ = respCreate.Body.Close() }()

	bodyCreate, _ := io.ReadAll(io.LimitReader(respCreate.Body, 1<<20))
	var resCreate struct {
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bodyCreate, &resCreate); err == nil && resCreate.Data.Key != "" {
		return resCreate.Data.Key, nil
	}

	return "", fmt.Errorf("create api key failed: %s", string(bodyCreate))
}

// OwnsLoginSession 报告会话是否归属于 ZCode。
func OwnsLoginSession(sessionID string) bool {
	norm := strings.ToLower(strings.TrimSpace(sessionID))
	if strings.HasPrefix(norm, "zcode_") {
		return true
	}
	loginMu.Lock()
	defer loginMu.Unlock()
	s, ok := loginStore[sessionID]
	if !ok {
		return false
	}
	return time.Since(s.CreatedAt) <= 15*time.Minute
}

func gcLoginSessionsLocked() {
	now := time.Now()
	for id, s := range loginStore {
		if now.Sub(s.CreatedAt) > 15*time.Minute {
			delete(loginStore, id)
		}
	}
}

func randomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
