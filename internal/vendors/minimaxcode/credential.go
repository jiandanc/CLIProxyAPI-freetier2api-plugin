package minimaxcode

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// Credential 是归一化后的 MiniMax Code 账号凭证。
type Credential struct {
	Vendor       string `json:"vendor,omitempty"`
	Region       Region `json:"region,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	UUID         string `json:"uuid,omitempty"`
	ScreenWidth  int    `json:"screen_width,omitempty"`
	ScreenHeight int    `json:"screen_height,omitempty"`
	Email        string `json:"email,omitempty"`
	Label        string `json:"label,omitempty"`
	AuthMode     string `json:"auth_mode,omitempty"` // "oauth" 或 "token"
	BaseURL      string `json:"base_url,omitempty"`
}

// RandomUUID 生成 RFC 4122 v4 UUID。
func RandomUUID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	buf[6] = (buf[6] & 0x0f) | 0x40 // Version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// RandomDeviceID 生成 8 位十进制数字设备标识符（范围 10000000 ~ 99999999）。
func RandomDeviceID() string {
	n, err := rand.Int(rand.Reader, big.NewInt(90000000))
	if err != nil {
		return "12345678"
	}
	return fmt.Sprintf("%d", 10000000+n.Int64())
}

// EnsureIdentifiers 确保凭证包含必须的指纹标识符。
func (c *Credential) EnsureIdentifiers() {
	if c.UUID == "" {
		c.UUID = RandomUUID()
	}
	if c.DeviceID == "" {
		c.DeviceID = RandomDeviceID()
	}
	if c.ScreenWidth == 0 {
		c.ScreenWidth = 1920
	}
	if c.ScreenHeight == 0 {
		c.ScreenHeight = 1080
	}
	if c.AuthMode == "" {
		if c.RefreshToken != "" {
			c.AuthMode = "oauth"
		} else {
			c.AuthMode = "token"
		}
	}
}

// ParseCredential 解析 MiniMax Code 凭证 JSON。
func ParseCredential(raw []byte, defaultRegion Region) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("minimax credential is empty")
	}

	var m map[string]any
	if errUnmarshal := json.Unmarshal(raw, &m); errUnmarshal != nil {
		return nil, fmt.Errorf("decode minimax credential JSON: %w", errUnmarshal)
	}

	authObj := getSubMap(m, "auth")
	if authObj == nil {
		authObj = m
	}
	accountObj := getSubMap(m, "account")

	regionStr := strings.ToLower(firstNonEmpty(
		getString(m, "region"),
		getString(m, "realm"),
		getString(m, "_edition"),
		string(defaultRegion),
	))
	r := RegionCN
	if regionStr == "global" || regionStr == "io" || regionStr == "en" || regionStr == "minimaxcodeglobal" || regionStr == "minimaxglobal" {
		r = RegionGlobal
	}

	token := firstNonEmpty(
		getString(authObj, "access_token"),
		getString(authObj, "accessToken"),
		getString(authObj, "token"),
		getString(authObj, "bearer"),
		getString(m, "token"),
	)
	refreshToken := firstNonEmpty(
		getString(authObj, "refresh_token"),
		getString(authObj, "refreshToken"),
		getString(m, "refresh_token"),
	)

	if token == "" && refreshToken == "" {
		return nil, fmt.Errorf("minimax credential missing token or refresh_token")
	}

	authMode := strings.ToLower(firstNonEmpty(
		getString(m, "auth_mode"),
		getString(authObj, "auth_mode"),
	))
	if authMode == "" {
		if refreshToken != "" {
			authMode = "oauth"
		} else {
			authMode = "token"
		}
	}

	screenWidth := getInt(m, "screen_width", "screenWidth")
	if screenWidth == 0 {
		screenWidth = 1920
	}
	screenHeight := getInt(m, "screen_height", "screenHeight")
	if screenHeight == 0 {
		screenHeight = 1080
	}

	cred := &Credential{
		Vendor:       firstNonEmpty(getString(m, "vendor"), VendorIDFor(r)),
		Region:       r,
		AccessToken:  token,
		RefreshToken: refreshToken,
		ExpiresAt:    getInt64(authObj, "expires_at", "expiresAt", "TokenExpireAt"),
		UserID: firstNonEmpty(
			getString(authObj, "user_id"),
			getString(accountObj, "user_id"),
			getString(m, "user_id"),
			getString(m, "realUserID"),
			getString(m, "real_user_id"),
			getString(m, "uid"),
		),
		AgentID: firstNonEmpty(
			getString(authObj, "agent_id"),
			getString(m, "agent_id"),
			getString(m, "agentId"),
		),
		DeviceID: firstNonEmpty(
			getString(authObj, "device_id"),
			getString(m, "device_id"),
			getString(m, "deviceId"),
		),
		UUID: firstNonEmpty(
			getString(authObj, "uuid"),
			getString(m, "uuid"),
		),
		ScreenWidth:  screenWidth,
		ScreenHeight: screenHeight,
		Email: firstNonEmpty(
			getString(accountObj, "email"),
			getString(m, "email"),
			getString(m, "identifier"),
		),
		Label:    firstNonEmpty(getString(m, "label"), getString(m, "name")),
		AuthMode: authMode,
		BaseURL:  getString(m, "base_url"),
	}

	cred.EnsureIdentifiers()
	return cred, nil
}

// LooksLikeCredential 判断文件或数据是否属于本 MiniMax Code 区域实例。
func LooksLikeCredential(raw map[string]any, fileName, provider string, targetRegion Region) bool {
	expectedVendor := VendorIDFor(targetRegion)

	// 1. 显式声明了其它供应商时绝不认领
	if v := strings.ToLower(getString(raw, "vendor")); v != "" && v != expectedVendor {
		// 特例：若只标了 "minimax" 或 "minimaxcode"，按 region 判定
		if v != "minimax" && v != "minimaxcode" {
			return false
		}
	}

	// 2. 显式命中当前 vendor 字段
	if strings.EqualFold(getString(raw, "vendor"), expectedVendor) {
		return true
	}

	// 3. 文件名前缀匹配
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base != "" {
		if targetRegion == RegionGlobal {
			if strings.HasPrefix(base, "minimaxcodeglobal") || strings.HasPrefix(base, "minimaxglobal") {
				return true
			}
		} else {
			if strings.HasPrefix(base, "minimaxcodecn") || strings.HasPrefix(base, "minimaxcn") {
				return true
			}
		}
	}

	// 4. 特征字段识别
	hasAgentID := getString(raw, "agent_id") != "" || getString(raw, "agentId") != ""
	hasRealUserID := getString(raw, "realUserID") != "" || getString(raw, "real_user_id") != ""
	hasMinimaxSignature := strings.Contains(strings.ToLower(getString(raw, "app_id")), "3001") ||
		strings.Contains(strings.ToLower(getString(raw, "client_id")), "mcode-public")

	if hasAgentID || hasRealUserID || hasMinimaxSignature {
		r := strings.ToLower(firstNonEmpty(getString(raw, "region"), getString(raw, "realm")))
		if targetRegion == RegionGlobal && (r == "global" || r == "en" || r == "io") {
			return true
		}
		if targetRegion == RegionCN && (r == "cn" || r == "") {
			return true
		}
	}

	return false
}

// StorageJSON 序列化为规范的凭证存储 JSON。
func (c *Credential) StorageJSON() ([]byte, error) {
	c.EnsureIdentifiers()
	m := map[string]any{
		"vendor":        c.Vendor,
		"region":        c.Region,
		"access_token":  c.AccessToken,
		"refresh_token": c.RefreshToken,
		"expires_at":    c.ExpiresAt,
		"user_id":       c.UserID,
		"agent_id":      c.AgentID,
		"device_id":     c.DeviceID,
		"uuid":          c.UUID,
		"screen_width":  c.ScreenWidth,
		"screen_height": c.ScreenHeight,
		"auth_mode":     c.AuthMode,
	}
	if c.Email != "" {
		m["email"] = c.Email
	}
	if c.Label != "" {
		m["label"] = c.Label
	}
	if c.BaseURL != "" {
		m["base_url"] = c.BaseURL
	}
	return json.MarshalIndent(m, "", "  ")
}

// MergeStorageJSON 合并刷新后的令牌，保留用户自定义字段。
func MergeStorageJSON(original []byte, cred *Credential) ([]byte, error) {
	if len(original) == 0 {
		return cred.StorageJSON()
	}
	var m map[string]any
	if err := json.Unmarshal(original, &m); err != nil {
		return cred.StorageJSON()
	}

	m["access_token"] = cred.AccessToken
	m["refresh_token"] = cred.RefreshToken
	m["expires_at"] = cred.ExpiresAt
	if cred.UserID != "" {
		m["user_id"] = cred.UserID
	}
	if cred.AgentID != "" {
		m["agent_id"] = cred.AgentID
	}
	if cred.DeviceID != "" {
		m["device_id"] = cred.DeviceID
	}
	if cred.UUID != "" {
		m["uuid"] = cred.UUID
	}
	if cred.Email != "" && getString(m, "email") == "" {
		m["email"] = cred.Email
	}
	if cred.Label != "" && getString(m, "label") == "" {
		m["label"] = cred.Label
	}

	return json.MarshalIndent(m, "", "  ")
}

// 辅助字段提取函数
func firstNonEmpty(strs ...string) string {
	for _, s := range strs {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func getString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s, ok := v.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func getInt(m map[string]any, keys ...string) int {
	return int(getInt64(m, keys...))
}

func getInt64(m map[string]any, keys ...string) int64 {
	if m == nil {
		return 0
	}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch val := v.(type) {
			case float64:
				return int64(val)
			case int64:
				return val
			case int:
				return int64(val)
			case json.Number:
				if n, err := val.Int64(); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

func getSubMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key]; ok && v != nil {
		if sub, ok := v.(map[string]any); ok {
			return sub
		}
	}
	return nil
}
