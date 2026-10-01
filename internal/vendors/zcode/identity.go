package zcode

// 每账号客户端设备档案（一号一台设备）与上游身份头构造。
//
// 为什么需要它：billing 族（current/balance/preview/claim）**必需** X-Device-Mid，
// 缺失时上游返回 code=3001；同时上游按设备聚类做风控，多个账号共用同一套
// 平台/内核/分辨率会聚成「一台机器开了 N 个账号」的关联信号。
//
// 因此档案从**成套 SKU** 抽样（platform × arch × os_version × screen 绑定），
// 禁止字段笛卡尔积——「darwin-arm64 + 1366x768」这类组合不是真实电脑。
// 池内不含 linux：官方桌面端主形态是 Mac / Windows，服务器内核特征反而扎眼。
//
// 参考 zcode2api/app/fingerprint.py 与 app/identity.py。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// DeviceProfile 是单个账号的设备档案，字段直接映射上游身份头与事件字段。
type DeviceProfile struct {
	Platform  string `json:"platform"`   // darwin / win32
	Arch      string `json:"arch"`       // arm64 / x64
	OSVersion string `json:"os_version"` // X-Os-Version（os.release() 语义）
	Language  string `json:"language"`   // X-Client-Language
	Timezone  string `json:"timezone"`   // X-Client-Timezone（IANA）
	Screen    string `json:"screen"`     // 激活事件 screen_resolution
	DeviceMID string `json:"device_mid"` // X-Device-Mid，一号一台
}

// PlatformFull 返回 X-Platform 取值形态（darwin-arm64）。
func (p DeviceProfile) PlatformFull() string {
	if p.Platform == "" {
		return DefaultPlatform
	}
	return p.Platform + "-" + p.Arch
}

// OSCategory 返回 X-Os-Category（darwin→macos / win32→windows）。
func (p DeviceProfile) OSCategory() string {
	switch p.Platform {
	case "darwin", "macos":
		return "macos"
	case "win32", "windows":
		return "windows"
	default:
		return "linux"
	}
}

// sku 是一套完整可用的桌面设备组合。
type sku struct {
	weight    int
	platform  string
	arch      string
	osVersion string
	screen    string
}

// desktopSKUs 是从官方桌面端常见形态归纳的成套组合。
//
// 权重即抽样概率：Apple silicon 与 Win11 主流机型占多数，老旧机型少量。
var desktopSKUs = []sku{
	// Apple silicon MacBook Air/Pro 13–14"（darwin 24 = Sequoia，25 = Tahoe）
	{10, "darwin", "arm64", "24.5.0", "1512x982"},
	{10, "darwin", "arm64", "24.6.0", "1512x982"},
	{8, "darwin", "arm64", "24.5.0", "1728x1117"},
	{8, "darwin", "arm64", "24.6.0", "1728x1117"},
	{8, "darwin", "arm64", "25.5.0", "1512x982"},
	{6, "darwin", "arm64", "25.5.0", "1728x1117"},
	{5, "darwin", "arm64", "23.6.0", "1512x982"},
	{4, "darwin", "arm64", "24.5.0", "2560x1440"},
	{3, "darwin", "arm64", "24.6.0", "2560x1600"},
	// Intel Mac 存量（darwin 24+ 不再配 x64）
	{2, "darwin", "x64", "23.6.0", "1920x1080"},
	{2, "darwin", "x64", "22.6.0", "1440x900"},
	// Windows 11 主流 + 少量 Win10
	{8, "win32", "x64", "10.0.22631", "1920x1080"},
	{7, "win32", "x64", "10.0.26100", "1920x1080"},
	{5, "win32", "x64", "10.0.22631", "2560x1440"},
	{4, "win32", "x64", "10.0.26200", "1920x1080"},
	{3, "win32", "x64", "10.0.26100", "2560x1440"},
	{3, "win32", "x64", "10.0.22621", "1920x1080"},
	{2, "win32", "x64", "10.0.22631", "3840x2160"},
	{2, "win32", "x64", "10.0.19045", "1920x1080"},
	{1, "win32", "x64", "10.0.22000", "1920x1080"},
}

// locales 是真实地区对（语言 ↔ 时区同源，激活事件共用）。
var locales = []struct{ language, timezone string }{
	{"zh-CN", "Asia/Shanghai"},
	{"en-US", "America/New_York"},
	{"en-US", "America/Los_Angeles"},
	{"en-GB", "Europe/London"},
	{"de-DE", "Europe/Berlin"},
	{"ja-JP", "Asia/Tokyo"},
	{"ko-KR", "Asia/Seoul"},
	{"en-SG", "Asia/Singapore"},
}

// NewDeviceProfile 抽样生成一份成套桌面档案（登录入池时调用一次并持久化）。
func NewDeviceProfile() DeviceProfile {
	total := 0
	for _, item := range desktopSKUs {
		total += item.weight
	}
	picked := desktopSKUs[0]
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(total))); err == nil {
		remaining := n.Int64()
		for _, item := range desktopSKUs {
			remaining -= int64(item.weight)
			if remaining < 0 {
				picked = item
				break
			}
		}
	}
	locale := locales[0]
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(len(locales)))); err == nil {
		locale = locales[n.Int64()]
	}
	return DeviceProfile{
		Platform:  picked.platform,
		Arch:      picked.arch,
		OSVersion: picked.osVersion,
		Language:  locale.language,
		Timezone:  locale.timezone,
		Screen:    picked.screen,
		DeviceMID: NewUUID(),
	}
}

// ensureProfile 就地补齐档案中缺失的字段（旧凭证升级用）。
//
// 整台设备都缺失时补一套完整 SKU；否则只在**同一平台内**补空字段——
// 混搭平台会产生自相矛盾的档案（如 win32 + macOS 内核号），比字段缺失更糟。
func (p *DeviceProfile) ensure() {
	if p.Platform == "" && p.Arch == "" && p.OSVersion == "" && p.Screen == "" {
		fresh := NewDeviceProfile()
		fresh.DeviceMID = firstNonEmpty(p.DeviceMID, fresh.DeviceMID)
		*p = fresh
		return
	}
	if p.Platform == "" {
		p.Platform = "darwin"
	}
	if p.Arch == "" {
		if p.Platform == "win32" {
			p.Arch = "x64"
		} else {
			p.Arch = "arm64"
		}
	}
	if p.OSVersion == "" || !osVersionMatchesPlatform(p.Platform, p.OSVersion) {
		p.OSVersion = defaultOSVersionFor(p.Platform)
	}
	if p.Screen == "" {
		p.Screen = "1920x1080"
	}
	if p.Language == "" {
		p.Language = DefaultLanguage
	}
	if p.Timezone == "" {
		p.Timezone = DefaultTimezone
	}
	if p.DeviceMID == "" {
		p.DeviceMID = NewUUID()
	}
}

// osVersionMatchesPlatform 报告内核版本串是否与平台相符。
//
// 上游按平台解析该字段的形态，混搭（win32 + 25.5.0）是明显的伪造信号。
func osVersionMatchesPlatform(platform, osVersion string) bool {
	switch platform {
	case "win32", "windows":
		return strings.HasPrefix(osVersion, "10.")
	case "darwin", "macos":
		// darwin 内核版本为两位主版本号（22–25）。
		return !strings.HasPrefix(osVersion, "10.")
	default:
		return true
	}
}

// defaultOSVersionFor 返回某平台的默认内核版本。
func defaultOSVersionFor(platform string) string {
	switch platform {
	case "win32", "windows":
		return "10.0.22631"
	default:
		return DefaultOSVersion
	}
}

// IdentityHeaders 是上游要求的 X- 身份头集合（不含鉴权头）。
type IdentityHeaders struct {
	values map[string]string
}

// Set 写入一个身份头。
func (h *IdentityHeaders) Set(key, value string) {
	if h.values == nil {
		h.values = map[string]string{}
	}
	h.values[key] = value
}

// Apply 把身份头写入目标 map（键名保持上游要求的原样大小写）。
func (h *IdentityHeaders) Apply(target map[string]string) {
	for key, value := range h.values {
		target[key] = value
	}
}

// Has 报告某个身份头是否已设置。
func (h *IdentityHeaders) Has(key string) bool {
	_, ok := h.values[key]
	return ok
}

// BuildIdentityHeaders 构造完整身份头（对齐官方桌面端 companion 头集合）。
//
// 有账号时取该账号的设备档案；profile 为零值时回退全局伪装常量。
func BuildIdentityHeaders(profile DeviceProfile) *IdentityHeaders {
	profile.ensure()
	headers := &IdentityHeaders{}
	headers.Set("HTTP-Referer", HTTPReferer)
	headers.Set("User-Agent", UserAgent)
	headers.Set("X-ZCode-App-Version", ClientAppVersion)
	headers.Set("X-Title", ClientTitle)
	headers.Set("X-ZCode-Agent", ClientAgent)
	headers.Set("X-Platform", profile.PlatformFull())
	headers.Set("X-Release-Channel", ReleaseChannel)
	headers.Set("X-Client-Language", profile.Language)
	headers.Set("X-Client-Timezone", profile.Timezone)
	headers.Set("X-Os-Category", profile.OSCategory())
	headers.Set("X-Os-Version", profile.OSVersion)
	headers.Set("X-Device-Mid", profile.DeviceMID)
	return headers
}

// BuildTraceHeaders 构造追踪头。
//
// JWT/Plan 通道（= 上游的 start-plan）**只允许**这三个头：官方客户端不带
// x-query-id / x-session-id，误发会触发上游 3012「unusual activity」风控。
// 每请求重新生成。
func BuildTraceHeaders() map[string]string {
	return map[string]string{
		"x-request-id":         NewUUID(),
		"x-zcode-session-type": "main",
		"x-zcode-trace-id":     NewUUID(),
	}
}

// NewUUID 生成 RFC 4122 v4 UUID。
func NewUUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// NewHexID 生成 n 字节的随机十六进制串。
func NewHexID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}
