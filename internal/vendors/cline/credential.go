package cline

// 本文件实现 Cline 凭证的解析与归属判定。
//
// Cline 的凭证来自 WorkOS 设备码登录：真正的长期凭据是 refreshToken，
// accessToken 有有效期、由续期链路轮换。
//
// **出站 token 必须带 workos: 前缀**：上游按这个前缀识别凭证来源，
// 漏掉会被当成无效令牌。前缀在 credential.go 里统一补，调用方不必记。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// VendorID 是供应商实例标识。同时是凭证文件名前缀。
//
// Cline 只有一个部署（api.cline.bot），没有国内/国际之分。
const VendorID = "cline"

// VendorName 是管理端展示名。
const VendorName = "Cline"

// workosTokenPrefix 是出站 Bearer 令牌必须携带的前缀。
//
// 这是上游的硬要求（不是装饰）：Cline 用它区分 WorkOS 签发的令牌与
// 其它来源的令牌。登录、续期、导入三条路径都要补。
const workosTokenPrefix = "workos:"

// Credential 是归一化后的 Cline 凭证。
type Credential struct {
	// AccessToken 是出站令牌（**不含** workos: 前缀，发送时由 BearerToken 补）。
	AccessToken string
	// RefreshToken 是长期凭据（登录产物，续期用它换新的 access token）。
	RefreshToken string
	// ExpiresAt 是 AccessToken 的过期时刻（Unix 秒）；0 表示未知。
	ExpiresAt int64
	// Email 是账号邮箱（登录流程会填）。
	Email string
	// Label 是展示名。
	Label string
	// AccountID 是账号唯一标识（登录产物，缺失时用邮箱）。
	AccountID string
}

// BearerToken 返回可直接放进 Authorization 头的令牌。
//
// 补 workos: 前缀是**幂等**的：从文件导入的凭证可能已经带了前缀
// （cline2api 的导出格式就带），重复补会变成 workos:workos:…。
func (c *Credential) BearerToken() string {
	if c == nil {
		return ""
	}
	return WithWorkOSPrefix(c.AccessToken)
}

// WithWorkOSPrefix 给令牌补上 workos: 前缀（已有则原样返回）。
func WithWorkOSPrefix(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, workosTokenPrefix) {
		return trimmed
	}
	return workosTokenPrefix + trimmed
}

// StripWorkOSPrefix 去掉 workos: 前缀（导入时用）。
func StripWorkOSPrefix(token string) string {
	return strings.TrimPrefix(strings.TrimSpace(token), workosTokenPrefix)
}

// ParseCredential 解析 Cline 凭证 JSON。
//
// 字段名候选是刻意的：Cline 的凭证在不同来源里写法不同——
//   - 本插件登录落盘：accessToken / refreshToken（驼峰，与上游一致）；
//   - cline2api 导出：access_token / refresh_token（下划线）；
//   - 手写：可能只给一个 refreshToken。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{
		AccessToken:  firstString(payload, "accessToken", "access_token"),
		RefreshToken: firstString(payload, "refreshToken", "refresh_token"),
		Email:        firstString(payload, "email", "account", "userEmail"),
		Label:        firstString(payload, "label", "name"),
		AccountID:    firstString(payload, "accountId", "account_id", "uid"),
	}
	cred.ExpiresAt = parseExpiry(payload["expiresAt"])
	if cred.ExpiresAt == 0 {
		cred.ExpiresAt = parseExpiry(payload["expires_at"])
	}
	// 令牌里的 workos: 前缀在归一化时剥掉，发送时由 BearerToken 统一补。
	// 这样文件里存的是裸令牌，不会出现 workos:workos: 的累积。
	cred.AccessToken = StripWorkOSPrefix(cred.AccessToken)

	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return nil, fmt.Errorf("credential has neither accessToken nor refreshToken")
	}
	return cred, nil
}

// firstString 返回第一个非空的字符串字段。
func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// LooksLikeCredential 判断一份 JSON 是否属于 Cline。
//
// **判定必须偏严**：宿主会把所有非内建格式的凭证依次喂给每个插件，
// 误吞别家的会让宿主用本插件的结构覆盖对方账号（且是静默的）。
//
// 只接受 Cline 特有的证据：
//  1. 令牌带 workos: 前缀（Cline 独有）；
//  2. 文件名含 cline 段；
//  3. 同时有 refreshToken 与 Cline 特有的字段组合。
//
// 刻意不用 "token" / "access_token" 这类通用字段名做判据：Qoder 与
// WorkBuddy 的凭证也有这些字段，拿它们当判据必然跨供应商误吞。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if fileNameMatchesConvention(fileName) {
		return true
	}
	if raw == nil {
		return false
	}
	if hasWorkOSPrefix(raw) {
		return true
	}
	// 驼峰 refreshToken 是 Cline 与 cline2api 导出格式的写法：
	// 别家供应商（Qoder / WorkBuddy）用的是下划线 refresh_token。
	if _, okRefresh := raw["refreshToken"]; okRefresh {
		return true
	}
	_ = provider
	return false
}

// hasWorkOSPrefix 判断凭证里是否有带 workos: 前缀的令牌。
func hasWorkOSPrefix(raw map[string]any) bool {
	for _, key := range []string{"accessToken", "access_token", "token", "refreshToken", "refresh_token"} {
		if value, okValue := raw[key].(string); okValue {
			if strings.HasPrefix(strings.TrimSpace(value), workosTokenPrefix) {
				return true
			}
		}
	}
	return false
}

// fileNameMatchesConvention 判断文件名是否符合本协议的命名约定。
//
// 约定：cline.json / cline-<id>.json / cline-login-<id>.json。
func fileNameMatchesConvention(fileName string) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base == "" {
		return false
	}
	if base == VendorID {
		return true
	}
	for _, separator := range []string{"-", "_", "."} {
		if strings.HasPrefix(base, VendorID+separator) ||
			strings.Contains(base, separator+VendorID) {
			return true
		}
	}
	return false
}
