package core

import (
	"strings"
	"sync"
)

// Credential 是归一化后的账号凭证，**字段取各供应商的并集**。
//
// 设计取舍：各供应商的凭证结构差异较大（WorkBuddy 用嵌套的 auth/account，
// Qoder 用扁平的 device_token/refresh_token），把它们强行统一成一套字段会
// 丢失信息。因此这里只放**确实通用**的字段，供应商特有的部分留在 Raw 里，
// 由各自的 Parse 负责解释。
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
	// UID 是账号唯一标识，也是设备指纹的派生种子。
	UID string
	// Token 是出站凭证：OAuth 账号是 access token，设备令牌账号是 device token。
	Token string
	// RefreshToken 用于刷新 Token（可能为空，表示只能靠上游 401 触发重登）。
	RefreshToken string
	// ExpiresAt 是 Token 过期时刻（Unix 秒）；0 表示未知。
	ExpiresAt int64
	// AuthMode 标识凭证形态：oauth / device / pat 等，供 UI 展示与分支判断。
	AuthMode string
	// FilePath 是凭证来源文件路径（刷新后原子写回此处）；空表示非文件来源。
	FilePath string
	// Raw 是原始凭证 JSON，供应商特有的字段都在这里。
	//
	// 刷新写回时以它为基础做增量合并（而非重建），这样用户在 auth 文件里
	// 手工维护的 disabled / prefix / proxy_url / note / weight 不会丢失。
	Raw map[string]any
	// Native 存放供应商自己的凭证对象（如 workbuddy.Credential）。
	//
	// core 不理解它的内容，也绝不访问它的字段——它只是让供应商在
	// 「解析一次、多处使用」时不必重复解析。供应商实现里用类型断言取回：
	//
	//	if native, ok := cred.Native.(*workbuddy.Credential); ok { ... }
	//
	// 为什么需要它：各供应商的凭证带大量特有字段与派生逻辑（区域、域、
	// 设备令牌、指纹种子），把它们全部拉平进 core.Credential 会让 core
	// 反过来依赖供应商细节，违背分层的初衷。
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

// UIDValue 返回加锁快照的 UID。
func (c *Credential) UIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UID
}

// AuthModeValue 返回加锁快照的凭证形态。
func (c *Credential) AuthModeValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AuthMode
}

// Snapshot 返回字段的加锁快照（供应商实现刷新逻辑时用它做并发校验）。
func (c *Credential) Snapshot() (token, refreshToken string, expiresAt int64) {
	if c == nil {
		return "", "", 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Token, c.RefreshToken, c.ExpiresAt
}

// ApplyTokenRefresh 在并发安全的前提下写入新令牌。
//
// 返回是否真的发生了变化——上游有时会回一个与旧值相同的 token，
// 此时不应触发写盘（否则 mtime 会无意义地变动，用户无法判断是否真的续期了）。
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

// SetVendorID 设置供应商标识（解析凭证后由 core 回填）。
func (c *Credential) SetVendorID(vendorID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.VendorID = vendorID
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

// NeedsRefresh 报告凭证是否需要在刷新窗口内续期。
//
// 语义刻意保守：
//   - 没有 refresh token 时返回 false（刷不了，交给上游 401 触发重登）；
//   - ExpiresAt 未知（<= 0）时返回 true —— 手写凭证刷一次就能拿到真实过期时间。
func (c *Credential) NeedsRefresh(windowSeconds int64) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(c.RefreshToken) == "" {
		return false
	}
	if c.ExpiresAt <= 0 {
		return true
	}
	return c.ExpiresAt-nowUnix() <= windowSeconds
}
