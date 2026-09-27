package workbuddy

// 本文件实现 WorkBuddy 的凭证续期。
//
// 续期链路：POST <ChatBase>/v2/plugin/auth/token，用 X-Refresh-Token 头
// 带旧 refresh token 换一组新令牌。
//
// 安全红线：X-Refresh-Token 头**只允许出现在这个刷新端点**，对话请求
// 绝不能携带——把 refresh token 发给对话上游等于把长期凭据暴露给一个
// 只需要 access token 的服务。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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
		return &Error{Kind: KindSessionDead, Status: http.StatusUnauthorized, Msg: "refresh failed: no accessToken in response — re-login required"}
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
