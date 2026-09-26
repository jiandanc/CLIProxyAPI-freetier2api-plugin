package cb

// 本文件是 CodeBuddy 协议的客户端核心：请求发送、统一信封处理、凭证刷新、
// 设备指纹派生与出站 HTTP 客户端构造。
//
// 所有出站请求都经宿主 HTTP 桥（internal/httpx），因此宿主的代理、
// 账号级代理与请求日志对这些请求同样生效。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"workbuddy2api-plugin/internal/httpx"
)

const (
	// shortTimeout 是短 RPC（刷新、签到、余额、模型清单、任务上报）的总时长上限。
	shortTimeout = 120 * time.Second
	// chatHeaderTimeout 是对话首字节前的等待上限。
	chatHeaderTimeout = 120 * time.Second
	// refreshTimeout 是凭证刷新请求的上限。
	refreshTimeout = 30 * time.Second
	// apiBodyLimit 是短 RPC 响应体的读取上限。
	apiBodyLimit = 1 << 20
	// configBodyLimit 是 /v3/config 的读取上限（响应体明显更大）。
	configBodyLimit = 2 << 20

	// deriveSaltPrefix 是设备指纹派生的固定盐前缀。
	//
	// 固定前缀保证指纹跨重启稳定（换了就是新设备）；真正的部署隔离
	// 由 installSalt（机器盐）提供：不同部署的指纹空间互不相同。
	deriveSaltPrefix = "wb2a:"

	// authRefreshSource 是刷新请求的 X-Auth-Refresh-Source 取值。
	authRefreshSource = "plugin"

	// maxRefreshTokenExpiresIn 是刷新响应里 expiresIn 的合理上限。
	// 超过它视为脏数据，保留原有的 expiresAt 而不是写成一个离谱的未来时刻。
	maxRefreshTokenExpiresIn = int64(10 * 365 * 24 * 60 * 60)

	// deviceTokenFileTTL 是 device token 文件的读取缓存时长。
	deviceTokenFileTTL = 5 * time.Minute
	// deviceTokenFileMaxLen 是 device token 文件的长度上限（超过视为异常）。
	deviceTokenFileMaxLen = 1024
)

// Options 是构造 Client 所需的全部输入。
//
// 刻意用一个结构体而不是长参数列表：字段会随上游变化持续增加，
// 位置参数很快就会不可读。
type Options struct {
	// Context 绑定宿主回调 ID，使出站请求进宿主的请求日志。
	Context context.Context
	// EnabledRealms 是启用的域（cn / global 子集）。
	EnabledRealms []string
	// PromptMode / PromptText 控制系统提示词处理。
	PromptMode string
	PromptText string
	// SanitizeFingerprints 开启出站请求体黑名单脱敏。
	SanitizeFingerprints bool
	// PassthroughIP 决定是否把客户端 IP 透传给上游。
	PassthroughIP bool
	// UserAgent 显式覆盖出站 UA。
	UserAgent string
	// ClientVersion / CLIVersion 覆盖 UA 的两段版本号。
	ClientVersion string
	CLIVersion    string
	// ClientName 覆盖归属头取值。
	ClientName string
	// DeviceToken / DeviceTokenFile 是设备令牌的两级兜底。
	DeviceToken     string
	DeviceTokenFile string
	// StateDir 用于存放模型清单缓存。
	StateDir string
}

// Client 是 CodeBuddy 上游客户端。
//
// 每个请求构造一个实例：它很轻（只是配置快照 + 少许缓存），
// 但必须绑定请求级 context 才能让出站流量进宿主的请求日志。
type Client struct {
	opts Options
}

// SetInstallSalt 注入部署级设备指纹盐。
//
// 由 package main 在 applyConfig 时调用：盐变更会让全部账号的派生机器码漂移
// （等价于换设备），因此生成后必须保持不变。
var (
	installSaltMu sync.RWMutex
	installSalt   string
)

// SetInstallSalt 设置机器盐。
func SetInstallSalt(salt string) {
	installSaltMu.Lock()
	installSalt = strings.TrimSpace(salt)
	installSaltMu.Unlock()
}

func currentInstallSalt() string {
	installSaltMu.RLock()
	defer installSaltMu.RUnlock()
	return installSalt
}

// NewClient 构造一个上游客户端。
func NewClient(opts Options) *Client {
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	return &Client{opts: opts}
}

// enabledRealm 报告某个域是否被配置启用。
//
// 关闭的域上的账号一律不发起上游请求：判断放在客户端层而不是散落各处，
// 保证「关掉 global」是真的一个请求都不发。
func (c *Client) enabledRealm(region Region) bool {
	if len(c.opts.EnabledRealms) == 0 {
		return true
	}
	for _, item := range c.opts.EnabledRealms {
		if item == string(region) {
			return true
		}
	}
	return false
}

// 三档出站 HTTP 客户端。

// shortClient 返回短 RPC 客户端。
func (c *Client) shortClient() *http.Client {
	return httpx.Client(c.opts.Context, shortTimeout)
}

// refreshClient 返回凭证刷新客户端。
func (c *Client) refreshClient() *http.Client {
	return httpx.Client(c.opts.Context, refreshTimeout)
}

// chatClient 返回对话客户端。
//
// 总时长设为 0：流式对话的时长不可预期，首字节与空闲分别由
// headerTimeout 与流内空闲监控负责。用总超时会误杀长回答。
func (c *Client) chatClient(headerTimeout time.Duration) *http.Client {
	if headerTimeout <= 0 {
		headerTimeout = chatHeaderTimeout
	}
	// 宿主桥不支持 ResponseHeaderTimeout（由宿主侧决定），
	// 这里用 context 超时表达首字节上限：httpx 的 transport 会把
	// 请求 context 传给 host.http.do_stream。
	return httpx.StreamClient(c.opts.Context, 0)
}

// apiEnvelope 是上游的统一响应信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发起一次短 RPC 并解开统一信封。
//
// 错误处理：HTTP ≥400 走 Classify；HTTP 成功但业务 code 非 0 也走 Classify
// （把 msg 当正文）。返回的是信封里的 data 部分。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request %s: %w", req.URL.Path, errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
	if errRead != nil {
		// 半截 body 不进 Classify：网络问题不该被当成账号问题罚号。
		return nil, fmt.Errorf("read %s response: %w", req.URL.Path, errRead)
	}
	return unwrapEnvelope(resp.StatusCode, string(body), resp.Header)
}

// doJSONWithLimit 与 doJSON 相同，但使用更大的读取上限（/v3/config 需要）。
func (c *Client) doJSONWithLimit(req *http.Request, limit int64) (json.RawMessage, error) {
	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request %s: %w", req.URL.Path, errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, limit))
	if errRead != nil {
		return nil, fmt.Errorf("read %s response: %w", req.URL.Path, errRead)
	}
	return unwrapEnvelope(resp.StatusCode, string(body), resp.Header)
}

// unwrapEnvelope 把「状态码 + 正文」解成 data 或分类错误。
func unwrapEnvelope(status int, body string, header http.Header) (json.RawMessage, error) {
	if status >= 400 {
		return nil, enrichClassifyError(status, body, header)
	}
	var envelope apiEnvelope
	if errUnmarshal := json.Unmarshal([]byte(body), &envelope); errUnmarshal != nil {
		// 响应不是 JSON：说明上游返回了非预期内容（HTML 错误页等）。
		return nil, &Error{Kind: KindClient, Status: status, Msg: truncate(body, 512)}
	}
	if envelope.Code != 0 {
		return nil, enrichClassifyError(status, body, header)
	}
	return envelope.Data, nil
}

// enrichClassifyError 在分类结果上补齐上游给出的等待信息。
//
// 上游有两种表达等待的方式：响应头（Retry-After 系列）与正文文案（"将在 X 重置"）。
// 两者都解析出来交给调用方，由它决定冷却到哪个时刻。
func enrichClassifyError(status int, body string, header http.Header) *Error {
	classified := Classify(status, body)
	classified.RetryAfter = ParseRetryAfter(header, time.Now())
	classified.ResetAt = ParseRateReset(body)
	return classified
}

// refreshTokenResponse 是刷新端点的响应。
type refreshTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// RefreshToken 刷新凭证的 access token。
//
// 安全红线：X-Refresh-Token 头**只允许出现在刷新端点**，对话请求绝不能携带。
//
// 并发采用两段式：锁内取快照 → 锁外发请求 → 锁内校验快照未被并发改动后再写回。
func (c *Client) RefreshToken(cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("credential is nil")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	refreshToken := cred.RefreshTokenValue()
	if strings.TrimSpace(refreshToken) == "" {
		return fmt.Errorf("credential has no refresh token")
	}
	accessBefore, refreshBefore := cred.Snapshot()

	endpoints := GetEndpoints(region)
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost,
		endpoints.ChatBase+tokenRefreshPath, nil)
	if errReq != nil {
		return fmt.Errorf("build refresh request: %w", errReq)
	}
	applyCommonHeaders(req, c, cred)
	req.Header.Set("X-Refresh-Token", refreshToken)
	req.Header.Set("X-Auth-Refresh-Source", authRefreshSource)

	resp, errDo := c.refreshClient().Do(req)
	if errDo != nil {
		return fmt.Errorf("refresh request: %w", errDo)
	}
	defer closeBody(resp.Body)

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
	if errRead != nil {
		return fmt.Errorf("read refresh response: %w", errRead)
	}
	if resp.StatusCode >= 400 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	}

	var envelope struct {
		Code int                  `json:"code"`
		Msg  string               `json:"msg"`
		Data refreshTokenResponse `json:"data"`
	}
	var token refreshTokenResponse
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal == nil && envelope.Code == 0 && strings.TrimSpace(envelope.Data.AccessToken) != "" {
		token = envelope.Data
	} else if errUnmarshal == nil && envelope.Code != 0 {
		return fmt.Errorf("refresh failed: code=%d msg=%s", envelope.Code, envelope.Msg)
	} else if errDirect := json.Unmarshal(body, &token); errDirect != nil || strings.TrimSpace(token.AccessToken) == "" {
		return fmt.Errorf("refresh failed: no accessToken in response — re-login required")
	}

	// 快照校验：并发刷新已经写过就放弃本次写回，避免用旧响应覆盖新值。
	if !cred.UnchangedSince(accessBefore, refreshBefore) {
		return nil
	}
	var expiresAt int64
	if token.ExpiresIn > 0 && token.ExpiresIn <= maxRefreshTokenExpiresIn {
		expiresAt = time.Now().Unix() + token.ExpiresIn
	}
	cred.ApplyTokenRefresh(token.AccessToken, token.RefreshToken, token.Domain, expiresAt)
	if cred.FilePath != "" {
		if errSave := SaveCredentialFile(cred.FilePath, cred); errSave != nil {
			return fmt.Errorf("save refreshed credential: %w", errSave)
		}
	}
	return nil
}

// DeviceToken 按「账号凭证 > 插件配置 > 文件」的优先级解析设备令牌。
func (c *Client) DeviceToken(cred *Credential) string {
	if token := cred.DeviceTokenValue(); token != "" {
		return token
	}
	if token := strings.TrimSpace(c.opts.DeviceToken); token != "" {
		return token
	}
	return readDeviceTokenFile(c.opts.DeviceTokenFile)
}

// deviceTokenFileCache 是 device token 文件的读取缓存。
//
// 带缓存是因为它会被每个请求读取，而文件内容变化很慢（桌面端登录时写入）。
var deviceTokenFileCache struct {
	mu      sync.Mutex
	path    string
	token   string
	fetched time.Time
}

// readDeviceTokenFile 读取 device token 文件（带 5 分钟缓存）。
func readDeviceTokenFile(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	deviceTokenFileCache.mu.Lock()
	defer deviceTokenFileCache.mu.Unlock()

	if deviceTokenFileCache.path == trimmed && time.Since(deviceTokenFileCache.fetched) < deviceTokenFileTTL {
		return deviceTokenFileCache.token
	}
	deviceTokenFileCache.path = trimmed
	deviceTokenFileCache.fetched = time.Now()
	deviceTokenFileCache.token = ""

	info, errStat := os.Stat(trimmed)
	if errStat != nil || info.Size() > deviceTokenFileMaxLen {
		// 读不到或明显异常：缓存空串，避免把过期值继续注入出站请求。
		return ""
	}
	raw, errRead := os.ReadFile(trimmed)
	if errRead != nil {
		return ""
	}
	deviceTokenFileCache.token = strings.TrimSpace(string(raw))
	return deviceTokenFileCache.token
}

// deriveID 派生一个跨重启稳定、账号间互异的标识。
//
// 用途：设备指纹（X-Machine-ID / X-Session-ID）与事件里的 machineId。
// 派生种子混入机器盐，使不同部署拥有独立的指纹空间——
// 否则上游一旦识别出派生模式即可全局拉黑所有同源部署。
func deriveID(uid, purpose string) string {
	seed := deriveSaltPrefix + purpose + ":" + uid + ":" + currentInstallSalt()
	sum := sha256.Sum256([]byte(seed))
	// 取 18 字节 → 36 位 hex，与官方客户端的设备 id 长度一致。
	return hex.EncodeToString(sum[:18])
}

// buildCacheKey 构造 prompt_cache_key。
//
// 这是单项最大的费用优化：同一段长前缀命中缓存后费用可降一个数量级。
// uid 前 8 位是**硬隔离因子**——跨账号复用同一 cache key 会让上游
// 命中错账号的前缀缓存，泄露对方对话内容。
func buildCacheKey(uid, conversation string) string {
	uidPrefix := uid
	if len(uidPrefix) > 8 {
		uidPrefix = uidPrefix[:8]
	}
	if uidPrefix == "" {
		uidPrefix = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	return "wb2a-" + uidPrefix + "-" + hex.EncodeToString(sum[:16])
}

// closeBody 关闭响应体并忽略错误（响应体关闭失败不影响业务结果）。
func closeBody(body io.Closer) {
	if body != nil {
		_ = body.Close()
	}
}

// truncate 截断字符串用于错误信息。
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
