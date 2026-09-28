package zcode

// 本文件实现 ZCode 的 OAuth CLI 登录流程。
// 参考：/Users/jiandan/Workspaces/zcode2api/app/oauth.py

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/httpx"
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

	sessionID := "zcode_" + randomHex(16)
	loginMu.Lock()
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

	loginMu.Lock()
	session, ok := loginStore[sessionID]
	loginMu.Unlock()

	if !ok || time.Since(session.CreatedAt) > 15*time.Minute {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期",
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
		return nil, fmt.Errorf("oauth poll request: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户授权...",
		}, nil
	}

	var res struct {
		Code int `json:"code"`
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode poll response: %w", err)
	}

	token := res.Data.AccessToken
	if token == "" {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待用户在浏览器中完成登录...",
		}, nil
	}

	// 授权成功，清理会话
	loginMu.Lock()
	delete(loginStore, sessionID)
	loginMu.Unlock()

	storagePayload := map[string]any{
		"type":      "freetier",
		"vendor":    VendorID,
		"jwt_token": token,
		"label":     "ZCode-" + MaskedKey(token),
	}
	rawStorage, _ := json.Marshal(storagePayload)

	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth: pluginapi.AuthData{
			Provider:    "freetier",
			FileName:    "zcode-" + MaskedKey(token) + ".json",
			Label:       "ZCode-" + MaskedKey(token),
			StorageJSON: rawStorage,
		},
	}, nil
}

func randomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
