package cline

// 本文件实现 Cline 的凭证续期。
//
// 续期链路（移植自 cline2api 的 refreshClineToken）：
//   POST {base}/auth/refresh
//   {"refreshToken":"<rt>","grantType":"refresh_token"}
//
// 注意两个细节，都不是笔误：
//   - 字段是**驼峰** refreshToken，不是标准 OAuth 的 refresh_token；
//   - 授权类型字段叫 **grantType**，不是 grant_type。
//     上游自己定义了这套形状，照标准 OAuth 发会被拒。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/httpx"
)

// refreshHTTPTimeout 是续期请求的上限。
const refreshHTTPTimeout = 30 * time.Second

// RefreshCredential 用 refresh token 换一组新的令牌。
//
// 返回 updated=false 表示上游没下发新令牌（理论上不该发生，但上游偶尔
// 会回同样的值）；调用方据此决定是否写盘。
func RefreshCredential(ctx context.Context, baseURL string, cred *Credential) (*Credential, bool, error) {
	if cred == nil {
		return nil, false, fmt.Errorf("credential is nil")
	}
	refreshToken := StripWorkOSPrefix(cred.RefreshToken)
	if refreshToken == "" {
		return nil, false, NewCredentialError("cline_credential_invalid", "凭证没有 refresh token，无法续期")
	}

	endpoint := BaseURL(baseURL) + RefreshPath
	body, errMarshal := json.Marshal(map[string]string{
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})
	if errMarshal != nil {
		return nil, false, fmt.Errorf("encode refresh request: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, false, fmt.Errorf("build refresh request: %w", errReq)
	}
	ApplyJSONHeaders(req)

	resp, errDo := httpx.Client(ctx, refreshHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, false, &Error{Msg: "refresh: " + errDo.Error(), Kind: KindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, false, &Error{Status: resp.StatusCode, Msg: "read refresh response: " + errRead.Error(), Kind: KindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, Classify(resp.StatusCode, string(raw))
	}

	var payload struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, false, fmt.Errorf("decode refresh response: %w", errUnmarshal)
	}
	newAccess := StripWorkOSPrefix(payload.Data.AccessToken)
	if newAccess == "" {
		return nil, false, NewCredentialError("cline_credential_invalid", "续期响应里没有 accessToken")
	}
	nextRefresh := StripWorkOSPrefix(payload.Data.RefreshToken)
	if nextRefresh == "" {
		// 上游没轮换 refresh token：保留旧的（否则下次续期会失败）。
		nextRefresh = refreshToken
	}
	if newAccess == cred.AccessToken && nextRefresh == cred.RefreshToken {
		// 上游回了同样的值：算「有效」但不算「已刷新」，避免无意义写盘。
		return cred, false, nil
	}

	updated := &Credential{
		AccessToken:  newAccess,
		RefreshToken: nextRefresh,
		ExpiresAt:    parseExpiry(payload.Data.ExpiresAt),
		Email:        cred.Email,
		Label:        cred.Label,
		AccountID:    cred.AccountID,
	}
	return updated, true, nil
}

// NewCredentialError 构造一个凭证失效错误。
func NewCredentialError(code, message string) *Error {
	return &Error{Code: code, Msg: message, Kind: KindCredential, Status: http.StatusUnauthorized}
}

// parseExpiry 解析过期时刻，兼容上游的多种形态。
//
// 上游的 expiresAt 是 any：可能是 Unix 毫秒（float64 / int64）、
// 也可能是 RFC3339 字符串。解析失败返回 0——调用方会当成「未知」，
// 立即续期一次，代价只是一次多余的请求。
func parseExpiry(raw any) int64 {
	switch value := raw.(type) {
	case float64:
		return millisToSeconds(int64(value))
	case int64:
		return millisToSeconds(value)
	case int:
		return millisToSeconds(int64(value))
	case json.Number:
		parsed, errParse := value.Int64()
		if errParse != nil {
			return 0
		}
		return millisToSeconds(parsed)
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return 0
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, errParse := time.Parse(layout, trimmed); errParse == nil {
				return parsed.Unix()
			}
		}
		return 0
	}
	return 0
}

// millisToSeconds 把 Unix 毫秒转成 Unix 秒。
//
// 上游给的是毫秒。值小于 1e12 时说明它其实是秒（上游部分接口混用），
// 这时原样返回——硬按毫秒除会让过期时间落到 1970 年，账号被判定为
// 永久过期而反复续期。
func millisToSeconds(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value < 1_000_000_000_000 {
		return value
	}
	return value / 1000
}

// NeedsRefresh 报告凭证是否该续期了（提前 60 秒）。
func NeedsRefresh(cred *Credential, now time.Time) bool {
	if cred == nil {
		return true
	}
	if cred.AccessToken == "" {
		return true
	}
	if cred.ExpiresAt == 0 {
		// 过期时间未知：不主动续期，交给上游 401 触发。
		return false
	}
	return now.Add(60*time.Second).Unix() >= cred.ExpiresAt
}
