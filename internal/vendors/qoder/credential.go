package qoder

// 本文件实现 Qoder 凭证的解析与归属判定。
//
// Qoder 的凭证形态比 WorkBuddy 松散得多（历史原因）：token 字段有七个候选名，
// 还可能是 qoder2api 导出格式把整对凭证塞在 secret 字段的 JSON 字符串里。
// 因此解析要尽可能宽，而**归属判定要尽可能严**——宽进严出。

import (
	"encoding/json"
	"fmt"
	"strings"

	"freetier2api-plugin/internal/vendors/qoder/qoderapi"
)

// 供应商实例标识。同时是凭证文件名前缀。
const (
	// VendorIDCN 是国内版（qoder.com.cn）。
	VendorIDCN = "qodercn"
	// VendorIDGlobal 是国际版（qoder.com）。
	VendorIDGlobal = "qoderglobal"

	// protocolName 是本协议在文件名里的公共标识段。
	protocolName = "qoder"
)

// qoderapi.Region / qoderapi.RegionCN / qoderapi.RegionGlobal / qoderapi.NormalizeRegion 定义在 endpoints.go
// ——它们同时服务端点表，属于协议层的基础设施。

// VendorIDFor 返回该区域对应的供应商标识。
func VendorIDFor(region qoderapi.Region) string {
	if region == qoderapi.RegionCN {
		return VendorIDCN
	}
	return VendorIDGlobal
}

// VendorNameFor 返回该区域的展示名。
func VendorNameFor(region qoderapi.Region) string {
	if region == qoderapi.RegionCN {
		return "Qoder 国内版"
	}
	return "Qoder 国际版"
}

// credentialTokenKeys 是凭证里可能承载 token 的字段名。
//
// 七个候选来自历史积累：不同版本的 qoder2api 导出、桌面端抓包、
// 用户手写各有各的写法。全部试一遍比要求用户改文件友好得多。
var credentialTokenKeys = []string{
	"token", "device_token", "access_token", "api_key",
	"personal_token", "pat", "qoder_token",
}

// Credential 是归一化后的 Qoder 凭证。
type Credential struct {
	// Token 是出站凭证：PAT（pt-…）或设备令牌（dt-…）。
	Token string
	// RefreshToken 用于续期（drt-… 前缀）。
	RefreshToken string
	// Region 是账号所属区域。
	Region qoderapi.Region
	// Label 是展示名。
	Label string
	// Email 是账号邮箱（登录流程会填）。
	Email string
}

// ParseCredential 解析 Qoder 凭证 JSON。
func ParseCredential(raw []byte, fallbackRegion qoderapi.Region) (*Credential, error) {
	cred := &Credential{Region: fallbackRegion}
	if len(raw) == 0 {
		return cred, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return cred, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred.Token = firstTokenIn(payload)
	if refresh, okRefresh := payload["refresh_token"].(string); okRefresh {
		cred.RefreshToken = strings.TrimSpace(refresh)
	}

	// qoder2api 的导出格式把整对凭证放在 secret 字段的 JSON 字符串里。
	// 直接导入这类文件（或原样拷贝 export 条目）时必须能解出 dt- 与 drt-。
	if nested, okNested := payload["secret"].(string); okNested && strings.TrimSpace(nested) != "" {
		trimmed := strings.TrimSpace(nested)
		inner := map[string]any{}
		if errInner := json.Unmarshal([]byte(trimmed), &inner); errInner == nil {
			if cred.Token == "" {
				cred.Token = firstTokenIn(inner)
			}
			if cred.RefreshToken == "" {
				if refresh, okRefresh := inner["refresh_token"].(string); okRefresh {
					cred.RefreshToken = strings.TrimSpace(refresh)
				}
			}
		} else if cred.Token == "" {
			// 非 JSON 的 secret 视作直接是凭证（与上游 ParseOAuthSecret 一致）。
			cred.Token = trimmed
		}
	}
	if cred.Token == "" {
		return cred, fmt.Errorf("credential has no token field (tried: %s)", strings.Join(credentialTokenKeys, ", "))
	}
	if region, okRegion := payload["region"].(string); okRegion && strings.TrimSpace(region) != "" {
		cred.Region = qoderapi.NormalizeRegion(region)
	}
	if label, okLabel := payload["label"].(string); okLabel {
		cred.Label = strings.TrimSpace(label)
	}
	if email, okEmail := payload["email"].(string); okEmail {
		cred.Email = strings.TrimSpace(email)
	}
	return cred, nil
}

// firstTokenIn 返回一层 JSON 里第一个非空的 token 字段。
func firstTokenIn(payload map[string]any) string {
	for _, key := range credentialTokenKeys {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// LooksLikeCredential 判断一份 JSON 是否属于 Qoder（任意区域）。
//
// **判定必须偏严**：宿主会把所有非内建格式的凭证依次喂给每个插件，
// 误吞别家的会让宿主用本插件的结构覆盖对方账号（且是静默的）。
//
// 只接受 Qoder 特有的证据：
//  1. token 值带 Qoder 独有的前缀（pt- / drt-）；
//  2. 文件名含 qoder 段；
//  3. 有 secret 字段（qoder2api 导出格式的特征）。
//
// **刻意不用 dt- 前缀**：WorkBuddy 的凭证也有 device_token 字段且同样用
// dt- 前缀（实测两家的设备令牌前缀相同），拿它当判据必然跨供应商误吞。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if hasQoderTokenPrefix(raw) {
		return true
	}
	if _, okSecret := raw["secret"].(string); okSecret {
		return true
	}
	if fileNameMatchesConvention(fileName) {
		return true
	}
	_ = provider
	return false
}

// hasQoderTokenPrefix 判断凭证里的 token 是否带 Qoder 独有前缀。
//
// pt- 个人访问令牌 / drt- 刷新令牌 —— 这两个前缀是 Qoder 独有的。
// 设备令牌的 dt- 前缀**不在此列**：WorkBuddy 也用同样的前缀。
func hasQoderTokenPrefix(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	for _, key := range append(append([]string{}, credentialTokenKeys...), "refresh_token") {
		value, okValue := raw[key].(string)
		if !okValue {
			continue
		}
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "pt-") || strings.HasPrefix(trimmed, "drt-") {
			return true
		}
	}
	return false
}

// fileNameMatchesConvention 判断文件名是否符合本协议的命名约定。
//
// 约定：qoder.json / qodercn-<id>.json / qoderglobal-<id>.json / qoder-login-<id>.json。
func fileNameMatchesConvention(fileName string) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base == "" {
		return false
	}
	if base == protocolName || base == VendorIDCN || base == VendorIDGlobal {
		return true
	}
	for _, separator := range []string{"-", "_", "."} {
		if strings.HasPrefix(base, protocolName+separator) ||
			strings.Contains(base, separator+protocolName) {
			return true
		}
	}
	return false
}

// RegionForCredential 从凭证推断所属区域，推断不出返回空。
//
// 与区域相关的字段只有一个 region（Qoder 的端点表不像 WorkBuddy 那样
// 能从 domain 反推——它压根不写 domain 字段）。
func RegionForCredential(raw map[string]any) qoderapi.Region {
	if raw == nil {
		return ""
	}
	if region, okRegion := raw["region"].(string); okRegion && strings.TrimSpace(region) != "" {
		return qoderapi.NormalizeRegion(region)
	}
	return ""
}
