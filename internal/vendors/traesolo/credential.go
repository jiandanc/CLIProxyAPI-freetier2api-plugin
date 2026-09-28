package traesolo

// 本文件实现 TRAE SOLO 凭证的解析、特征识别与归一化。
// 参考：/Users/jiandan/Workspaces/trae2api-more/internal/auth/auth.go

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Credential struct {
	Vendor       string `json:"vendor,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	UID          string `json:"uid,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	Label        string `json:"label,omitempty"`
}

// ParseCredential 解析 JSON 凭证。支持顶层或嵌套（auth/account）结构。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("traesolo credential is empty")
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode traesolo credential json: %w", err)
	}

	// 嵌套解包：兼容 trae2api-more 的 {auth: {...}, account: {...}} 结构
	authObj := getMap(m, "auth")
	if authObj == nil {
		authObj = m
	}
	accountObj := getMap(m, "account")

	c := &Credential{
		Vendor:       firstNonEmpty(getString(m, "vendor"), VendorID),
		AccessToken:  firstNonEmpty(getString(authObj, "access_token"), getString(authObj, "accessToken"), getString(authObj, "token")),
		RefreshToken: firstNonEmpty(getString(authObj, "refresh_token"), getString(authObj, "refreshToken")),
		ExpiresAt:    getInt64(authObj, "expires_at", "expiresAt", "TokenExpireAt"),
		UID:          firstNonEmpty(getString(authObj, "uid"), getString(accountObj, "uid"), getString(m, "uid")),
		Nickname:     firstNonEmpty(getString(authObj, "nickname"), getString(accountObj, "nickname"), getString(m, "nickname")),
		EnterpriseID: firstNonEmpty(getString(authObj, "enterprise_id"), getString(accountObj, "enterprise_id"), getString(m, "enterprise_id")),
		MachineID:    firstNonEmpty(getString(authObj, "machine_id"), getString(authObj, "machineId"), getString(m, "machine_id")),
		DeviceID:     firstNonEmpty(getString(authObj, "device_id"), getString(authObj, "deviceId"), getString(m, "device_id")),
		Label:        getString(m, "label"),
	}

	if c.AccessToken == "" && c.RefreshToken == "" {
		return nil, fmt.Errorf("traesolo credential missing access_token and refresh_token")
	}

	return c, nil
}

// LooksLikeCredential 判断文件或数据是否属于 TRAE SOLO。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if v := strings.ToLower(getString(raw, "vendor")); v != "" && v != VendorID {
		return false
	}

	// 显式声明
	if strings.EqualFold(getString(raw, "vendor"), VendorID) {
		return true
	}

	// 文件名前缀匹配
	baseName := strings.ToLower(fileName)
	if strings.HasPrefix(baseName, "traesolo-") || strings.HasPrefix(baseName, "traesolo_") || baseName == "traesolo.json" {
		return true
	}

	// 内容特征：SOLO 特有端点或特征
	if getString(raw, "function") == "solo_work_lite" || strings.Contains(getString(raw, "api_host"), "mchost.guru") {
		return true
	}

	return false
}

// StorageJSON 序列化为标准的 JSON 存储结构。
func (c *Credential) StorageJSON() ([]byte, error) {
	m := map[string]any{
		"type":          "freetier",
		"vendor":        VendorID,
		"access_token":  c.AccessToken,
		"refresh_token": c.RefreshToken,
		"expires_at":    c.ExpiresAt,
		"uid":           c.UID,
		"nickname":      c.Nickname,
		"enterprise_id": c.EnterpriseID,
		"machine_id":    c.MachineID,
		"device_id":     c.DeviceID,
	}
	if c.Label != "" {
		m["label"] = c.Label
	}
	return json.MarshalIndent(m, "", "  ")
}

func getMap(m map[string]any, key string) map[string]any {
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

func getInt64(m map[string]any, keys ...string) int64 {
	if m == nil {
		return 0
	}
	for _, key := range keys {
		if v, ok := m[key]; ok && v != nil {
			switch n := v.(type) {
			case float64:
				val := int64(n)
				if val > 1e12 {
					return val / 1000
				}
				return val
			case int64:
				if n > 1e12 {
					return n / 1000
				}
				return n
			case json.Number:
				if parsed, err := n.Int64(); err == nil {
					if parsed > 1e12 {
						return parsed / 1000
					}
					return parsed
				}
			}
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
