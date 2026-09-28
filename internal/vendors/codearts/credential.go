package codearts

// 本文件实现 CodeArts 凭证的解析、特征识别与序列化。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/auth/auth.go

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Credential struct {
	Vendor          string            `json:"vendor,omitempty"`
	UserID          string            `json:"user_id,omitempty"`
	UserName        string            `json:"user_name,omitempty"`
	DomainID        string            `json:"domain_id,omitempty"`
	AccessKeyID     string            `json:"access_key_id,omitempty"`
	SecretAccessKey string            `json:"secret_access_key,omitempty"`
	SecurityToken   string            `json:"security_token,omitempty"`
	RefreshToken    string            `json:"refresh_token,omitempty"`
	CodeVerifier    string            `json:"code_verifier,omitempty"`
	ClientID        string            `json:"client_id,omitempty"`
	Expiration      string            `json:"expiration,omitempty"`
	ExpiresAt       int64             `json:"expires_at,omitempty"`
	DPoPPrivateKey  map[string]string `json:"dpop_private_key,omitempty"`
	Label           string            `json:"label,omitempty"`
}

// ParseCredential 解析 JSON 凭证。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("codearts credential is empty")
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode codearts credential json: %w", err)
	}

	c := &Credential{
		Vendor:          firstNonEmpty(getString(m, "vendor"), VendorID),
		UserID:          firstNonEmpty(getString(m, "user_id"), getString(m, "userId")),
		UserName:        firstNonEmpty(getString(m, "user_name"), getString(m, "userName")),
		DomainID:        firstNonEmpty(getString(m, "domain_id"), getString(m, "domainId")),
		AccessKeyID:     firstNonEmpty(getString(m, "access_key_id"), getString(m, "accessKeyId"), getString(m, "ak")),
		SecretAccessKey: firstNonEmpty(getString(m, "secret_access_key"), getString(m, "secretAccessKey"), getString(m, "sk")),
		SecurityToken:   firstNonEmpty(getString(m, "security_token"), getString(m, "securityToken"), getString(m, "cloud_dragon_token")),
		RefreshToken:    firstNonEmpty(getString(m, "refresh_token"), getString(m, "refreshToken")),
		CodeVerifier:    firstNonEmpty(getString(m, "code_verifier"), getString(m, "codeVerifier")),
		ClientID:        firstNonEmpty(getString(m, "client_id"), getString(m, "clientId"), ClientID),
		Expiration:      getString(m, "expiration"),
		ExpiresAt:       getInt64(m, "expires_at", "expiresAt"),
		Label:           getString(m, "label"),
	}

	if jwkObj := getMap(m, "dpop_private_key"); jwkObj != nil {
		c.DPoPPrivateKey = make(map[string]string)
		for k, v := range jwkObj {
			if s, ok := v.(string); ok {
				c.DPoPPrivateKey[k] = s
			}
		}
	}

	if c.AccessKeyID == "" && c.RefreshToken == "" {
		return nil, fmt.Errorf("codearts credential missing access_key_id and refresh_token")
	}

	return c, nil
}

// LooksLikeCredential 判断文件或数据是否属于 CodeArts。
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
	if strings.HasPrefix(baseName, "codearts-") || strings.HasPrefix(baseName, "codearts_") || baseName == "codearts.json" {
		return true
	}

	// 特征字段匹配：华为云 AK/SK + SecurityToken 或 DPoP
	if (getString(raw, "access_key_id") != "" && getString(raw, "secret_access_key") != "") || getMap(raw, "dpop_private_key") != nil {
		return true
	}

	return false
}

// StorageJSON 序列化为规范的凭证存储格式。
func (c *Credential) StorageJSON() ([]byte, error) {
	m := map[string]any{
		"type":              "freetier",
		"vendor":            VendorID,
		"user_id":           c.UserID,
		"user_name":         c.UserName,
		"domain_id":         c.DomainID,
		"access_key_id":     c.AccessKeyID,
		"secret_access_key": c.SecretAccessKey,
		"security_token":    c.SecurityToken,
		"refresh_token":     c.RefreshToken,
		"code_verifier":     c.CodeVerifier,
		"client_id":         c.ClientID,
		"expiration":        c.Expiration,
		"expires_at":        c.ExpiresAt,
	}
	if c.DPoPPrivateKey != nil {
		m["dpop_private_key"] = c.DPoPPrivateKey
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
				return int64(n)
			case int64:
				return n
			case json.Number:
				if parsed, err := n.Int64(); err == nil {
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
