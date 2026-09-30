package minimaxcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
)

// RefreshCredential 使用 refresh_token 刷新 MiniMax OAuth 访问令牌。
func RefreshCredential(ctx context.Context, cred *Credential, baseURLOverride string) (*Credential, bool, error) {
	if cred == nil {
		return nil, false, core.NewPluginError("credential_missing", "credential is nil", http.StatusUnauthorized)
	}

	refreshToken := strings.TrimSpace(cred.RefreshToken)
	if refreshToken == "" || cred.AuthMode == "token" {
		// 没有 refresh_token 的 Web JWT 令牌无法自动续期
		return cred, false, nil
	}

	accountHost := AccountHostFor(cred.Region)
	tokenURL := accountHost + EpToken

	data := url.Values{}
	data.Set("client_id", ClientID)
	data.Set("grant_type", RefreshGrantType)
	data.Set("refresh_token", refreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if errReq != nil {
		return nil, false, fmt.Errorf("build refresh request: %w", errReq)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, errDo := httpx.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		return nil, false, fmt.Errorf("refresh request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&payload); errDec != nil {
		return nil, false, fmt.Errorf("decode refresh response (HTTP %d): %w", resp.StatusCode, errDec)
	}

	if resp.StatusCode >= 400 || getString(payload, "error") != "" {
		errDesc := firstNonEmpty(
			getString(payload, "error_description"),
			getString(payload, "error"),
			fmt.Sprintf("HTTP %d", resp.StatusCode),
		)
		return nil, false, fmt.Errorf("minimax refresh token rejected: %s", errDesc)
	}

	accessToken := getString(payload, "access_token", "accessToken")
	if accessToken == "" {
		return nil, false, fmt.Errorf("minimax refresh response missing access_token")
	}

	newRefreshToken := getString(payload, "refresh_token", "refreshToken")
	expiresIn := getInt64(payload, "expires_in", "expiresIn")
	if expiresIn <= 0 {
		expiresIn = 3600
	}

	cred.AccessToken = accessToken
	if newRefreshToken != "" {
		cred.RefreshToken = newRefreshToken
	}
	cred.ExpiresAt = time.Now().Unix() + expiresIn

	return cred, true, nil
}
