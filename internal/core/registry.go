package core

import (
	"sort"
	"strings"
	"sync"
)

var (
	vendorMu       sync.RWMutex
	vendorRegistry = map[string]Vendor{}
	// vendorOrder 保持注册顺序，让 UI 与状态输出稳定可复现。
	vendorOrder []string
)

// RegisterVendor 注册一个供应商实例（由根层在 init 时调用）。
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

// ResolveVendor 按凭证判定归属哪个供应商。
//
// 判定顺序（证据由强到弱）：
//  1. 凭证内的 vendor 字段 —— 最可靠，是插件自己写进去的；
//  2. 文件名前缀 —— 用户可读，且登录落盘时就按供应商命名；
//  3. 各供应商自己的 Match —— 内容嗅探兜底（兼容手写/改名凭证）。
//
// 刻意不用「第一个认领的就收下」：供应商之间的结构有重叠（都可能有
// accessToken 字段），靠遍历顺序决定归属会让结果随注册顺序漂移。
func ResolveVendor(fileName, provider string, raw map[string]any) (Vendor, bool) {
	// 1) vendor 字段。
	if raw != nil {
		if rawVendor, okVendor := raw[VendorKey].(string); okVendor {
			if v, ok := VendorByID(rawVendor); ok {
				return v, true
			}
		}
	}

	// 2) 文件名前缀。按 ID 长度倒序匹配，避免前缀互为前缀时短的抢先命中。
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base != "" {
		vendorMu.RLock()
		ids := make([]string, len(vendorOrder))
		copy(ids, vendorOrder)
		vendorMu.RUnlock()
		sort.Slice(ids, func(i, j int) bool {
			if len(ids[i]) != len(ids[j]) {
				return len(ids[i]) > len(ids[j])
			}
			return ids[i] < ids[j]
		})
		for _, id := range ids {
			if fileNameHasVendorPrefix(base, id) {
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

// fileNameHasVendorPrefix 判断文件名是否符合某个供应商的前缀约定。
//
// 约定：<vendor>.json / <vendor>-<id>.json / <vendor>_<id>.json，
// 或名字里含 -<vendor> / _<vendor> 段（如 my-workbuddycn.json）。
func fileNameHasVendorPrefix(base, vendorID string) bool {
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

// FileNameFor 生成某个供应商的凭证文件名：<vendor>-<标识>.json。
//
// 标识做文件名安全化，防止上游 uid 里的路径分隔符造成目录穿越。
func FileNameFor(vendorID, identifier string) string {
	vendorID = strings.ToLower(strings.TrimSpace(vendorID))
	identifier = SanitizeFileComponent(identifier)
	if identifier == "" {
		identifier = "account"
	}
	return vendorID + "-" + identifier + ".json"
}

// SanitizeFileComponent 只保留文件名安全字符，并截断到合理长度。
func SanitizeFileComponent(value string) string {
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
