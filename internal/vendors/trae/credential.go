package trae

// 本文件实现 Trae 凭证的解析、特征识别与存储。
// 参考：/Users/jiandan/Workspaces/trae2api/src/auth.js

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Credential struct {
	Vendor       string `json:"vendor,omitempty"`
	Region       Region `json:"region,omitempty"`
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

// ParseCredential 解析 JSON 凭证。
func ParseCredential(raw []byte, defaultRegion Region) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("trae credential is empty")
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode trae credential json: %w", err)
	}

	authObj := getMap(m, "auth")
	if authObj == nil {
		authObj = m
	}
	accountObj := getMap(m, "account")

	regionStr := strings.ToLower(firstNonEmpty(
		getString(m, "region"),
		getString(m, "realm"),
		getString(m, "_edition"),
		string(defaultRegion),
	))
	r := RegionCN
	if regionStr == "global" || regionStr == "sg" || regionStr == "traeglobal" {
		r = RegionGlobal
	}

	c := &Credential{
		Vendor:       firstNonEmpty(getString(m, "vendor"), string(VendorIDFor(r))),
		Region:       r,
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
		return nil, fmt.Errorf("trae credential missing access_token and refresh_token")
	}

	return c, nil
}

// VendorIDFor 返回某区域对应的 VendorID。
func VendorIDFor(r Region) string {
	if r == RegionGlobal {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// LooksLikeCredential 判断文件或数据是否属于本 Trae 区域实例。
func LooksLikeCredential(raw map[string]any, fileName, provider string, targetRegion Region) bool {
	expectedVendor := VendorIDFor(targetRegion)

	// 1. 显式声明了其它供应商时绝不认领
	if v := strings.ToLower(getString(raw, "vendor")); v != "" && v != expectedVendor {
		// 特例：如果只标了 "trae"，按 region 判定
		if v != "trae" {
			return false
		}
	}

	// 2. 显式命中当前 vendor
	if strings.EqualFold(getString(raw, "vendor"), expectedVendor) {
		return true
	}

	// 3. 文件名前缀
	baseName := strings.ToLower(fileName)
	if strings.HasPrefix(baseName, expectedVendor+"-") || strings.HasPrefix(baseName, expectedVendor+"_") || baseName == expectedVendor+".json" {
		return true
	}

	// 4. 通用 trae 前缀，按内部 region 判断
	if strings.HasPrefix(baseName, "trae-") || strings.HasPrefix(baseName, "trae_") || baseName == "trae.json" {
		rStr := strings.ToLower(firstNonEmpty(getString(raw, "region"), getString(raw, "realm"), getString(raw, "_edition")))
		if targetRegion == RegionGlobal {
			return rStr == "global" || rStr == "sg"
		}
		return rStr == "cn" || rStr == "" // 默认 cn
	}

	return false
}

func (c *Credential) StorageJSON() ([]byte, error) {
	m := map[string]any{
		"type":          "freetier",
		"vendor":        VendorIDFor(c.Region),
		"region":        string(c.Region),
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
