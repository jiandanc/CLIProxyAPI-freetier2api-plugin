package zcode

// 本文件实现 ZCode 凭证的解析、特征识别与脱敏。
// 参考：/Users/jiandan/Workspaces/zcode2api

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Credential 是 ZCode 账号的归一化凭证。
type Credential struct {
	Vendor    string `json:"vendor,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
	JWTToken  string `json:"jwt_token,omitempty"`
	DeviceMID string `json:"device_mid,omitempty"`
	Label     string `json:"label,omitempty"`
}

// ParseCredential 解析 JSON 凭证。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("zcode credential is empty")
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode zcode credential json: %w", err)
	}

	c := &Credential{
		Vendor:    firstNonEmpty(getString(m, "vendor"), VendorID),
		APIKey:    firstNonEmpty(getString(m, "api_key"), getString(m, "apiKey"), getString(m, "key")),
		JWTToken:  firstNonEmpty(getString(m, "jwt_token"), getString(m, "jwtToken"), getString(m, "token"), getString(m, "access_token")),
		DeviceMID: firstNonEmpty(getString(m, "device_mid"), getString(m, "deviceMid")),
		Label:     getString(m, "label"),
	}

	if c.APIKey == "" && c.JWTToken == "" {
		return nil, fmt.Errorf("zcode credential missing api_key or jwt_token")
	}

	return c, nil
}

// LooksLikeCredential 判断文件或数据是否属于 ZCode。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	// 显式声明了其它供应商时绝不认领
	if v := strings.ToLower(getString(raw, "vendor")); v != "" && v != VendorID {
		return false
	}

	// 1. 显式 vendor == "zcode"
	if strings.EqualFold(getString(raw, "vendor"), VendorID) {
		return true
	}

	// 2. 文件名前缀包含 zcode
	baseName := strings.ToLower(fileName)
	if strings.HasPrefix(baseName, "zcode-") || strings.HasPrefix(baseName, "zcode_") || baseName == "zcode.json" {
		return true
	}

	// 3. 内容特征：包含 device_mid 且有 zcode/zai 痕迹
	if getString(raw, "device_mid") != "" || getString(raw, "deviceMid") != "" {
		if strings.Contains(strings.ToLower(fileName), "zcode") || strings.EqualFold(provider, "zai") {
			return true
		}
	}

	// 4. API Key 形如 xxx.xxx（智谱/Z.AI 的标准 API key 格式）且文件名含 zcode/zai
	key := firstNonEmpty(getString(raw, "api_key"), getString(raw, "apiKey"))
	if key != "" && (strings.Contains(baseName, "zcode") || strings.Contains(baseName, "zai")) {
		return true
	}

	return false
}

// MaskedKey 对 API Key 或 Token 进行脱敏：保留前 4 位和后 4 位，中间用 *** 替换。
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
