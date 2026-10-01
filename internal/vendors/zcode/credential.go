package zcode

// ZCode 凭证的解析、特征识别与脱敏。
//
// 一个 ZCode 账号可能同时持有两种凭证，语义完全不同：
//
//   - APIKey（api.z.ai，x-api-key）—— **对话通道**，免人机验证码；
//   - JWTToken（zcode.z.ai，Bearer）—— Coding Plan 通道，上游强制校验
//     X-Aliyun-Captcha-Verify-Param，没有真实浏览器求解器时不可用于对话。
//
// 因此对话一律优先 APIKey；JWT 仅用于 billing 族（额度查询 / 套餐领取）。
// OAuth 登录会一次拿到两者（JWT 直接下发，API Key 由 access_token 兑换）。

import (
	"encoding/json"
	"fmt"
	"strings"

	"freetier2api-plugin/internal/core"
)

// Credential 是归一化后的 ZCode 账号凭证。
type Credential struct {
	Vendor   string        `json:"vendor,omitempty"`
	APIKey   string        `json:"api_key,omitempty"`
	JWTToken string        `json:"jwt_token,omitempty"`
	Email    string        `json:"email,omitempty"`
	UserID   string        `json:"user_id,omitempty"`
	Profile  DeviceProfile `json:"profile,omitempty"`
	Label    string        `json:"label,omitempty"`
	AuthMode string        `json:"auth_mode,omitempty"` // oauth / apikey
}

// UsesPlanChannel 报告本凭证是否具备 Coding Plan 通道能力。
//
// 只判断凭证形状，不代表当下可用：上游还要求验证码头，见 CanChatOverPlan。
func (c *Credential) UsesPlanChannel() bool {
	return c != nil && strings.TrimSpace(c.JWTToken) != ""
}

// OutboundToken 返回对话出站凭证（优先 API Key，回退 JWT）。
func (c *Credential) OutboundToken() string {
	if c == nil {
		return ""
	}
	if strings.TrimSpace(c.APIKey) != "" {
		return c.APIKey
	}
	return strings.TrimSpace(c.JWTToken)
}

// ParseCredential 解析凭证 JSON。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("zcode credential is empty")
	}

	var m map[string]any
	if errUnmarshal := json.Unmarshal(raw, &m); errUnmarshal != nil {
		return nil, fmt.Errorf("decode zcode credential json: %w", errUnmarshal)
	}

	c := &Credential{
		Vendor:   firstNonEmpty(getString(m, "vendor"), VendorID),
		APIKey:   firstNonEmpty(getString(m, "api_key"), getString(m, "apiKey")),
		JWTToken: firstNonEmpty(getString(m, "jwt_token"), getString(m, "jwtToken")),
		Email:    strings.ToLower(getString(m, "email")),
		UserID:   getString(m, "user_id"),
		Label:    getString(m, "label"),
		AuthMode: getString(m, "auth_mode"),
	}

	c.Profile = parseProfile(m)
	c.Profile.ensure()

	if c.APIKey == "" && c.JWTToken == "" {
		return nil, fmt.Errorf("zcode credential missing api_key or jwt_token")
	}
	if c.AuthMode == "" {
		c.AuthMode = "apikey"
	}
	return c, nil
}

// parseProfile 读取设备档案（兼容指纹对象嵌在顶层或 profile/fingerprint 键下）。
func parseProfile(m map[string]any) DeviceProfile {
	nested := m
	for _, key := range []string{"profile", "fingerprint", "device"} {
		if sub, ok := m[key].(map[string]any); ok && sub != nil {
			nested = sub
			break
		}
	}
	return DeviceProfile{
		Platform:  getString(nested, "platform"),
		Arch:      getString(nested, "arch"),
		OSVersion: firstNonEmpty(getString(nested, "os_version"), getString(nested, "osVersion")),
		Language:  getString(nested, "language"),
		Timezone:  getString(nested, "timezone"),
		Screen:    getString(nested, "screen"),
		DeviceMID: firstNonEmpty(
			getString(nested, "device_mid"),
			getString(nested, "deviceMid"),
			getString(m, "device_mid"),
		),
	}
}

// StorageJSON 导出规范落盘 JSON。
func (c *Credential) StorageJSON() ([]byte, error) {
	m := map[string]any{
		"type":      "freetier",
		"vendor":    VendorID,
		"auth_mode": firstNonEmpty(c.AuthMode, "apikey"),
	}
	if c.APIKey != "" {
		m["api_key"] = c.APIKey
	}
	if c.JWTToken != "" {
		m["jwt_token"] = c.JWTToken
	}
	if c.Email != "" {
		m["email"] = c.Email
	}
	if c.UserID != "" {
		m["user_id"] = c.UserID
	}
	if c.Label != "" {
		m["label"] = c.Label
	}
	// 档案恒定落盘：device_mid 变了上游即视为新设备，必须持久化。
	m["profile"] = c.Profile
	return json.MarshalIndent(m, "", "  ")
}

// MergeStorageJSON 把刷新后的凭证合并回原文件。
//
// 只更新会变化的字段，保留用户在 auth 文件里手工维护的其它键。
func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if updated == nil {
		return original, nil
	}
	var existing map[string]any
	if len(original) > 0 {
		if errUnmarshal := json.Unmarshal(original, &existing); errUnmarshal != nil || existing == nil {
			existing = map[string]any{}
		}
	} else {
		existing = map[string]any{}
	}
	existing["type"] = "freetier"
	existing["vendor"] = VendorID
	if updated.APIKey != "" {
		existing["api_key"] = updated.APIKey
	}
	if updated.JWTToken != "" {
		existing["jwt_token"] = updated.JWTToken
	}
	if updated.Email != "" {
		existing["email"] = updated.Email
	}
	if updated.UserID != "" {
		existing["user_id"] = updated.UserID
	}
	if updated.AuthMode != "" {
		existing["auth_mode"] = updated.AuthMode
	}
	if updated.Label != "" {
		existing["label"] = updated.Label
	}
	if updated.Profile.DeviceMID != "" {
		existing["profile"] = updated.Profile
	}
	return json.MarshalIndent(existing, "", "  ")
}

// LooksLikeCredential 判断文件或数据是否属于 ZCode。
//
// **必须偏严**：宿主会把所有非内建格式的凭证依次喂给插件，误吞别家的会让
// 宿主用本插件的结构覆盖对方账号。因此只在有明确归属证据时才认领。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	// 显式声明了其它供应商时绝不认领。
	if v := strings.ToLower(getString(raw, "vendor")); v != "" && v != VendorID {
		return false
	}
	// 1) 显式 vendor == "zcode"
	if strings.EqualFold(getString(raw, "vendor"), VendorID) {
		return true
	}

	baseName := strings.ToLower(strings.TrimSpace(fileName))

	// 2) 文件名前缀包含 zcode
	if strings.HasPrefix(baseName, "zcode-") || strings.HasPrefix(baseName, "zcode_") ||
		strings.EqualFold(baseName, "zcode.json") {
		return true
	}

	// 3) 带 ZCode 专有痕迹：档案键或 auth_mode 标记。
	//    这两个键都不会出现在其它供应商的凭证里，是强特征。
	if _, ok := raw["profile"]; ok {
		if getString(raw, "device_mid") != "" || getString(raw, "jwt_token") != "" {
			return true
		}
	}

	// 4) 同时带 jwt_token 与 api_key（本插件 ZCode 的双凭证形态）。
	if getString(raw, "jwt_token") != "" && getString(raw, "api_key") != "" {
		return true
	}

	// 5) 智谱/Z.AI API Key 形态（xxx.xxx）+ 文件名含 zcode/zai
	key := firstNonEmpty(getString(raw, "api_key"), getString(raw, "apiKey"))
	if key != "" && strings.Count(key, ".") == 1 &&
		(strings.Contains(baseName, "zcode") || strings.Contains(baseName, "zai")) {
		return true
	}

	return false
}

// fileNameFor 生成 ZCode 凭证文件名（沿用插件的 <vendor>-<标识>.json 约定）。
func fileNameFor(identifier string) string {
	return core.FileNameFor(VendorID, identifier)
}

// MaskedKey 对凭证脱敏：保留前 4 位与后 4 位。
func MaskedKey(val string) string {
	s := strings.TrimSpace(val)
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "***" + s[len(s)-4:]
}

// UserIDFromJWT 从 JWT payload 解 user_id（user_id 优先，sub 兜底）。
//
// user_id 是官方客户端激活上报的用户标识，每次从 token 实时解出而不是缓存：
// token 刷新后 user_id 会自动跟随。
func UserIDFromJWT(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	claims := decodeJWTPayload(parts[1])
	for _, key := range []string{"user_id", "sub"} {
		if v, ok := claims[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// EmailFromJWT 从 JWT payload 解邮箱（OAuth 用户资料缺失时的备援）。
func EmailFromJWT(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	claims := decodeJWTPayload(parts[1])
	for _, key := range []string{"email", "user_email", "mail"} {
		if v, ok := claims[key].(string); ok && strings.Contains(v, "@") {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	return ""
}

// decodeJWTPayload 解 base64url 编码的 payload；失败返回空 map。
func decodeJWTPayload(segment string) map[string]any {
	decoded, errDecode := base64URLDecode(segment)
	if errDecode != nil {
		return nil
	}
	var claims map[string]any
	if errUnmarshal := json.Unmarshal(decoded, &claims); errUnmarshal != nil {
		return nil
	}
	return claims
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
