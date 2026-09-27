package core

import (
	"sort"
	"strings"
	"sync"
)

// VendorKey 是凭证内用于标识供应商的字段名。
//
// 宿主 ABI 限制一个插件只能有一个 provider key（见 vendor.go 的 ProviderKey 注释），
// 因此供应商的归属必须落在别处：
//   - 凭证文件名前缀（workbuddycn-<uid>.json）；
//   - 凭证内的 vendor 字段（本常量）。
//
// 两者冗余是刻意的：文件名让用户一眼能看出归属，vendor 字段让判定不依赖
// 文件命名（用户手工改名后仍能正确归属）。
const VendorKey = "vendor"

var (
	vendorMu       sync.RWMutex
	vendorRegistry = map[string]Vendor{}
	// vendorOrder 保持注册顺序，让 UI 与状态输出稳定可复现。
	vendorOrder []string
)

// RegisterVendor 注册一个供应商实例。由 internal/vendors 的 init 调用。
//
// 重复注册同一个 ID 会 panic：这是编程错误（两个实例抢同一个文件名前缀），
// 静默覆盖会让其中一个供应商的凭证永远无法归属。
func RegisterVendor(v Vendor) {
	if v == nil {
		panic("core: RegisterVendor called with nil vendor")
	}
	id := strings.ToLower(strings.TrimSpace(v.ID()))
	if id == "" {
		panic("core: RegisterVendor called with empty vendor ID")
	}
	vendorMu.Lock()
	defer vendorMu.Unlock()
	if _, exists := vendorRegistry[id]; exists {
		panic("core: duplicate vendor ID: " + id)
	}
	vendorRegistry[id] = v
	vendorOrder = append(vendorOrder, id)
}

// Vendors 返回全部已注册的供应商实例（按注册顺序）。
func Vendors() []Vendor {
	vendorMu.RLock()
	defer vendorMu.RUnlock()
	out := make([]Vendor, 0, len(vendorOrder))
	for _, id := range vendorOrder {
		out = append(out, vendorRegistry[id])
	}
	return out
}

// VendorByID 按 ID 取供应商实例。
func VendorByID(id string) (Vendor, bool) {
	vendorMu.RLock()
	defer vendorMu.RUnlock()
	v, ok := vendorRegistry[strings.ToLower(strings.TrimSpace(id))]
	return v, ok
}

// VendorIDs 返回全部供应商 ID（按注册顺序）。
func VendorIDs() []string {
	vendorMu.RLock()
	defer vendorMu.RUnlock()
	out := make([]string, len(vendorOrder))
	copy(out, vendorOrder)
	return out
}

// ResolveVendor 按凭证归属判定该用哪个供应商。
//
// 判定顺序（证据由强到弱）：
//  1. 凭证内的 vendor 字段 —— 最可靠，是插件自己写进去的；
//  2. 文件名前缀 —— 用户可读，且登录落盘时就按供应商命名；
//  3. 各供应商自己的 Match —— 内容嗅探兜底（兼容手写/改名/旧格式凭证）。
//
// 刻意**不**用「第一个认领的就收下」：供应商之间的结构有重叠（都可能有
// accessToken 字段），靠顺序决定归属会让结果随注册顺序漂移。因此先查
// 强证据，只有在前两者都无果时才让供应商各自嗅探。
func ResolveVendor(fileName, provider string, raw map[string]any) (Vendor, bool) {
	// 1) vendor 字段。
	if raw != nil {
		if rawVendor, okVendor := raw[VendorKey].(string); okVendor {
			if v, ok := VendorByID(rawVendor); ok {
				return v, true
			}
		}
	}

	// 2) 文件名前缀。按 ID 长度倒序匹配，避免 workbuddycn 的凭证被
	//    更短的前缀（若有）抢先命中。
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base != "" {
		vendorMu.RLock()
		ids := make([]string, len(vendorOrder))
		copy(ids, vendorOrder)
		vendorMu.RUnlock()
		sortByLengthDesc(ids)
		for _, id := range ids {
			if fileNameHasPrefix(base, id) {
				return vendorRegistry[id], true
			}
		}
	}

	// 3) 内容嗅探兜底。
	for _, v := range Vendors() {
		if v.Match(fileName, provider, raw) {
			return v, true
		}
	}
	return nil, false
}

// fileNameHasPrefix 判断文件名是否符合某个供应商的前缀约定。
//
// 约定：<vendor>.json / <vendor>-<id>.json / <vendor>_<id>.json，
// 或名字里含 -<vendor> / _<vendor> 段（如 my-workbuddycn.json）。
func fileNameHasPrefix(base, vendorID string) bool {
	if base == vendorID {
		return true
	}
	for _, separator := range []string{"-", "_", "."} {
		if strings.HasPrefix(base, vendorID+separator) ||
			strings.Contains(base, separator+vendorID) {
			return true
		}
	}
	return false
}

// FileNameFor 生成某个供应商的凭证文件名。
//
// 格式 <vendor>-<标识>.json。标识做文件名安全化，防止上游 uid 里的
// 路径分隔符造成目录穿越。
func FileNameFor(vendorID, identifier string) string {
	vendorID = strings.ToLower(strings.TrimSpace(vendorID))
	identifier = sanitizeFileComponent(identifier)
	if identifier == "" {
		identifier = "account"
	}
	return vendorID + "-" + identifier + ".json"
}

// sanitizeFileComponent 只保留文件名安全字符，并截断到合理长度。
func sanitizeFileComponent(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.', r == '_', r == '-', r == '@':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
		if builder.Len() >= 64 {
			break
		}
	}
	return strings.Trim(builder.String(), "-.@")
}

// sortByLengthDesc 按字符串长度倒序排列（长度相同则字典序，保证确定性）。
//
// 长度倒序是必要的：若存在前缀互为前缀的供应商 ID，短的那个会抢先命中。
func sortByLengthDesc(values []string) {
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
}
