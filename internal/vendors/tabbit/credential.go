package tabbit

// 本文件实现 Tabbit 凭证的解析、特征识别与脱敏。
// 参考：/Users/jiandan/Workspaces/tabbit2api

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Credential struct {
	Vendor       string `json:"vendor,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	SessionToken string `json:"session_token,omitempty"`
	Label        string `json:"label,omitempty"`
}

// ParseCredential 解析 JSON 凭证。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("tabbit credential is empty")
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode tabbit credential json: %w", err)
	}

	c := &Credential{
		Vendor:       firstNonEmpty(getString(m, "vendor"), VendorID),
		APIKey:       firstNonEmpty(getString(m, "api_key"), getString(m, "apiKey"), getString(m, "key")),
		BaseURL:      firstNonEmpty(getString(m, "base_url"), getString(m, "baseUrl")),
		SessionToken: firstNonEmpty(getString(m, "session_token"), getString(m, "sessionToken"), getString(m, "token")),
		Label:        getString(m, "label"),
	}

	if c.APIKey == "" && c.SessionToken == "" {
		return nil, fmt.Errorf("tabbit credential missing api_key or session_token")
	}

	return c, nil
}

// LooksLikeCredential 判断文件或数据是否属于 Tabbit。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if v := strings.ToLower(getString(raw, "vendor")); v != "" && v != VendorID {
		return false
	}

	// 显式 vendor == "tabbit"
	if strings.EqualFold(getString(raw, "vendor"), VendorID) {
		return true
	}

	// 文件名前缀匹配
	baseName := strings.ToLower(fileName)
	if strings.HasPrefix(baseName, "tabbit-") || strings.HasPrefix(baseName, "tabbit_") || baseName == "tabbit.json" {
		return true
	}

	// 凭证带有 tabbit 标识
	if strings.Contains(strings.ToLower(getString(raw, "base_url")), "tabbit") {
		return true
	}

	return false
}

// MaskedKey 对凭证进行脱敏处理。
func MaskedKey(val string) string {
	s := strings.TrimSpace(val)
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "***" + s[len(s)-4:]
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
