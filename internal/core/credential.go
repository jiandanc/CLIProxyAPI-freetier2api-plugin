package core

import (
	"strings"
	"sync"
)

// Credential 是归一化后的账号凭证，**字段取各供应商的并集**。
//
// 设计取舍：各供应商的凭证结构差异较大（WorkBuddy 用嵌套的 auth/account，
// Qoder 用扁平的 device_token/refresh_token），强行统一成一套字段会丢失信息。
// 因此这里只放**确实通用**的字段，供应商特有的部分留在 Raw 里由各自的 Parse 解释。
//
// **并发约定**：所有出站请求头构造必须经取值方法（TokenValue 等）加锁快照，
// 不能直读字段——任务调度器与请求转发会真并发地刷新/读取同一份凭证。
type Credential struct {
	mu sync.Mutex

	// VendorID 是供应商实例标识（workbuddycn 等），决定请求走哪个上游。
	VendorID string
	// Region 是区域标识（cn / global）。
	Region string
	// Label 是展示名（昵称/邮箱/uid，取第一个非空）。
	Label string
	// FileID 是**账号身份**：凭证文件名去掉 .json 后缀，全局唯一且稳定。
	//
	// 专门用于「按账号落状态」的场景——签到记录、任务历史、额度缓存。
	// 与 UID 的分工是刻意的：UID 是**上游身份**（WorkBuddy 的 X-Uid 与设备
	// 指纹种子、Trae 的 X-Uid），换了会让上游视为新设备；而 FileID 只在本
	// 插件内部使用，各家算法一致、没有分支。
	//
	// 早期版本拿 UID 兼做账号身份，导致「UID 取自凭证内容」的供应商
	// （WorkBuddy）正常，而「UID 取自文件名」的供应商（Qoder/ZCode）在
	// 签到与展示两条路径上拿到不同的文件名、算出两个身份，页面于是永远
	// 显示「未签到」。
	FileID string
	// UID 是上游身份标识，仅用于出站请求（X-Uid、设备指纹派生、userId 上报）。
	// 为空的供应商表示上游不需要它。
	UID string
	// Token 是出站凭证：OAuth 账号是 access token，设备令牌账号是 device token。
	Token string
	// RefreshToken 用于刷新 Token（可能为空，表示只能靠上游 401 触发重登）。
	RefreshToken string
	// ExpiresAt 是 Token 过期时刻（Unix 秒）；0 表示未知。
	ExpiresAt int64
	// AuthMode 标识凭证形态：oauth / device / pat 等。
	AuthMode string
	// FilePath 是凭证来源文件路径（刷新后写回此处）；空表示非文件来源。
	FilePath string
	// Raw 是原始凭证 JSON，供应商特有的字段都在这里。
	//
	// 刷新写回时以它为基础做增量合并（而非重建），这样用户在 auth 文件里
	// 手工维护的 disabled / prefix / proxy_url / note / weight 不会丢失。
	Raw map[string]any
	// Native 存放供应商自己的凭证对象（如 workbuddy.Credential）。
	//
	// core 不理解它的内容，也绝不访问它的字段——它只是让供应商在
	// 「解析一次、多处使用」时不必重复解析（用类型断言取回）。
	Native any
}

// TokenValue 返回加锁快照的 Token。
func (c *Credential) TokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Token
}

// RefreshTokenValue 返回加锁快照的 RefreshToken。
func (c *Credential) RefreshTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RefreshToken
}

// ExpiresAtValue 返回加锁快照的过期时刻。
func (c *Credential) ExpiresAtValue() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ExpiresAt
}

// LabelValue 返回加锁快照的展示名。
func (c *Credential) LabelValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Label
}

// FileIDValue 返回加锁快照的账号身份（文件名去掉 .json）。
func (c *Credential) FileIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.FileID
}

// SetFileID 设置账号身份。
//
// 解析凭证时由各供应商的 Parse 填入，或由根层按文件名兜底（见
// ensureFileID）；后者保证任何供应商都不会因漏填而失去账号身份。
func (c *Credential) SetFileID(fileID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.FileID = fileID
}

// UIDValue 返回加锁快照的上游身份标识。
func (c *Credential) UIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UID
}

// VendorIDValue 返回加锁快照的供应商标识。
func (c *Credential) VendorIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.VendorID
}

// RegionValue 返回加锁快照的区域。
func (c *Credential) RegionValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Region
}

// SetVendorID 设置供应商标识（解析凭证后回填）。
func (c *Credential) SetVendorID(vendorID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.VendorID = vendorID
}

// SetRegion 设置区域（宿主 attributes 可覆盖凭证自带的区域声明）。
func (c *Credential) SetRegion(region string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Region = region
}

// SetFilePath 记录凭证来源文件路径（刷新后写回用）。
func (c *Credential) SetFilePath(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.FilePath = path
}

// ApplyTokenRefresh 在并发安全的前提下写入新令牌。
//
// 返回是否真的发生了变化——上游有时会回一个与旧值相同的 token，
// 此时不应触发写盘（否则 mtime 会无意义变动，用户无法判断是否真的续期了）。
func (c *Credential) ApplyTokenRefresh(token, refreshToken string, expiresAt int64) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	if strings.TrimSpace(token) != "" && token != c.Token {
		c.Token = token
		changed = true
	}
	if strings.TrimSpace(refreshToken) != "" && refreshToken != c.RefreshToken {
		c.RefreshToken = refreshToken
		changed = true
	}
	if expiresAt > 0 && expiresAt != c.ExpiresAt {
		c.ExpiresAt = expiresAt
		changed = true
	}
	return changed
}
