package opencodezen

// 本文件实现 OpenCode ZEN 凭证的解析与归属判定。
//
// 与其它供应商最大的不同：ZEN **没有登录流程**，凭证就是一把静态 API key
// （用户自行到 OpenCode 网站申请后填进来）。没有 access/refresh token 之分，
// 也没有续期——key 要么有效要么失效。
//
// 因此本包没有 login.go / refresh.go：不是漏了，是上游压根没有这两个能力。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// VendorID 是供应商实例标识。同时是凭证文件名前缀。
//
// ZEN 只有一个部署（opencode.ai），没有国内/国际之分，因此不像 WorkBuddy
// 与 Qoder 那样有 cn/global 两个实例。
const VendorID = "opencodezen"

// VendorName 是管理端展示名。
const VendorName = "OpenCode ZEN"

// ErrMissingAPIKey 表示凭证里没有可用的 API key。
var ErrMissingAPIKey = errors.New("credential has no api key")

// 凭证里可能承载 API key 的字段名。
//
// 多个候选是刻意的：用户从不同渠道拿到 key 后手写凭证时写法各异，
// 全部试一遍比要求用户改文件友好。
var credentialKeyKeys = []string{
	"api_key", "apikey", "apiKey", "key", "zen_key", "token", "access_token",
}

// Credential 是归一化后的 ZEN 凭证。
type Credential struct {
	// APIKey 是出站凭证（Bearer）。
	APIKey string
	// Label 是展示名。未提供时由调用方用脱敏后的 key 兜底。
	Label string
}

// ParseCredential 解析 ZEN 凭证 JSON。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{APIKey: firstKeyIn(payload)}
	if cred.APIKey == "" {
		return nil, fmt.Errorf("credential has no api key field (tried: %s)", strings.Join(credentialKeyKeys, ", "))
	}
	if label, okLabel := payload["label"].(string); okLabel {
		cred.Label = strings.TrimSpace(label)
	}
	if cred.Label == "" {
		if email, okEmail := payload["email"].(string); okEmail {
			cred.Label = strings.TrimSpace(email)
		}
	}
	return cred, nil
}

// firstKeyIn 返回一层 JSON 里第一个非空的 key 字段。
func firstKeyIn(payload map[string]any) string {
	for _, key := range credentialKeyKeys {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// MaskedKey 返回脱敏后的 key：保留前四位与后四位，中间用 *** 代替。
//
// 用于展示名兜底——**绝不**把完整 key 放进 Label，因为 Label 会进日志、
// 进管理接口响应、进页面 HTML。凭证材料只应存在于凭证文件里。
func MaskedKey(key string) string {
	trimmed := strings.TrimSpace(key)
	// 太短的 key 全遮：露出前后四位等于露出全部。
	if len(trimmed) <= 8 {
		if trimmed == "" {
			return ""
		}
		return "***"
	}
	return trimmed[:4] + "***" + trimmed[len(trimmed)-4:]
}

// LooksLikeCredential 判断一份 JSON 是否属于 OpenCode ZEN。
//
// **判定必须偏严**：宿主会把所有非内建格式的凭证依次喂给每个插件，
// 误吞别家的会让宿主用本插件的结构覆盖对方账号（且是静默的）。
//
// 只接受 ZEN 特有的证据：
//  1. 文件名含 opencode / zen 段（最可靠——用户添加时就是这个名字）；
//  2. 凭证里有 zen 专有的字段名（zen_key / api_key），且**没有**别家供应商
//     的特征字段。
//
// 刻意不用 "key" / "token" 这类通用字段名做判据：Qoder 的凭证也有 token
// 字段，拿它当判据必然跨供应商误吞。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if fileNameMatchesConvention(fileName) {
		return true
	}
	if raw == nil {
		return false
	}
	// 有 zen 专有字段名才认。api_key 单独出现也算——但必须排除别家的特征。
	if hasZenSpecificField(raw) && !hasForeignMarker(raw) {
		return true
	}
	_ = provider
	return false
}

// hasZenSpecificField 报告凭证里是否有 ZEN 专有的字段名。
func hasZenSpecificField(raw map[string]any) bool {
	for _, key := range []string{"zen_key", "api_key", "apikey"} {
		if value, okValue := raw[key].(string); okValue && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// hasForeignMarker 报告凭证里是否带有别家供应商的特征字段。
//
// 这是「api_key 字段太通用」的兜底：WorkBuddy 有 device_token/account，
// Qoder 有 device_token/refresh_token/secret，Cline 有 refreshToken。
// 命中任何一个就说明它不是 ZEN 的凭证。
func hasForeignMarker(raw map[string]any) bool {
	for _, key := range []string{
		"device_token", "refresh_token", "refreshToken", "secret",
		"account", "auth", "region", "workos",
	} {
		if _, okKey := raw[key]; okKey {
			return true
		}
	}
	return false
}

// fileNameMatchesConvention 判断文件名是否符合本协议的命名约定。
//
// 约定：opencodezen.json / opencodezen-<id>.json / opencode-zen-<id>.json。
func fileNameMatchesConvention(fileName string) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base == "" {
		return false
	}
	if base == VendorID || base == "opencode" || base == "zen" {
		return true
	}
	for _, separator := range []string{"-", "_", "."} {
		for _, name := range []string{VendorID, "opencode", "zen"} {
			if strings.HasPrefix(base, name+separator) ||
				strings.Contains(base, separator+name) {
				return true
			}
		}
	}
	return false
}
