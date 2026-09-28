package traesolo

// Token 续期：调用 ExchangeToken 轮换 accessToken 与 refreshToken。
// 参考：/Users/jiandan/Workspaces/trae2api-more/internal/upstream/

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"freetier2api-plugin/internal/httpx"
)

// RefreshCredential 刷新 TRAE SOLO 令牌。
func RefreshCredential(ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if cred == nil || cred.RefreshToken == "" {
		return cred, false, nil
	}

	payload := map[string]string{
		"refreshToken": cred.RefreshToken,
		"client_id":    ClientID,
	}
	bodyBytes, _ := json.Marshal(payload)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, OAuthHost+EpExchange, bytes.NewReader(bodyBytes))
	if errReq != nil {
		return nil, false, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ClientUA)

	client := httpx.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, false, fmt.Errorf("traesolo refresh token request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, false, Classify(resp.StatusCode, string(body))
	}

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken         string `json:"accessToken"`
			RefreshToken        string `json:"refreshToken"`
			TokenExpireAt       int64  `json:"tokenExpireAt"`
			TokenExpireDuration int64  `json:"tokenExpireDuration"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, false, fmt.Errorf("decode exchange token response: %w", err)
	}

	if result.Code != 0 || result.Data.AccessToken == "" {
		return nil, false, fmt.Errorf("exchange token failed: code=%d msg=%s", result.Code, result.Msg)
	}

	expiresAt := result.Data.TokenExpireAt
	if expiresAt > 1e12 {
		expiresAt /= 1000
	}
	if expiresAt <= 0 && result.Data.TokenExpireDuration > 0 {
		expiresAt = time.Now().Unix() + result.Data.TokenExpireDuration
	}

	updated := *cred
	updated.AccessToken = result.Data.AccessToken
	if result.Data.RefreshToken != "" {
		updated.RefreshToken = result.Data.RefreshToken
	}
	updated.ExpiresAt = expiresAt

	return &updated, true, nil
}

// MergeStorageJSON 把刷新后的令牌合并进原凭证 JSON。
func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if len(original) == 0 {
		return updated.StorageJSON()
	}

	var m map[string]any
	if err := json.Unmarshal(original, &m); err != nil {
		return updated.StorageJSON()
	}

	m["access_token"] = updated.AccessToken
	if updated.RefreshToken != "" {
		m["refresh_token"] = updated.RefreshToken
	}
	if updated.ExpiresAt > 0 {
		m["expires_at"] = updated.ExpiresAt
	}

	// 嵌套兼容
	if authObj, ok := m["auth"].(map[string]any); ok {
		authObj["accessToken"] = updated.AccessToken
		if updated.RefreshToken != "" {
			authObj["refreshToken"] = updated.RefreshToken
		}
		if updated.ExpiresAt > 0 {
			authObj["expiresAt"] = updated.ExpiresAt
		}
	}

	return json.MarshalIndent(m, "", "  ")
}
