package cb

// 本文件实现凭证的解析与写回。
//
// 凭证有两种形态（历史原因，两种都要认）：
//   - 嵌套形：{"auth": {...}, "account": {...}, "device_token": ...}（OAuth 登录产物）
//   - 扁平形：顶层直接放 accessToken / refreshToken / uid / ...（手写或旧版）
//
// **并发约定**：所有出站请求头构造必须经取值方法（AccessTokenValue 等）加锁快照，
// 不能直读字段——任务调度器与请求转发会真并发地刷新/读取同一份凭证。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Credential 是归一化后的账号凭证。
type Credential struct {
	mu sync.Mutex

	// AccessToken 是出站 Authorization 的 Bearer 值。
	AccessToken string
	// RefreshToken 用于刷新 access token。
	RefreshToken string
	// ExpiresAt 是 access token 过期时刻（Unix 秒）。
	ExpiresAt int64
	// Domain 是账号所属域名（用于推断 realm 与出站 X-Domain 头）。
	Domain string
	// realm 未导出：显式声明优先，为空时按 Domain 推断。
	realm Region
	// UID 是账号唯一标识（也是设备指纹的派生种子）。
	UID string
	// EnterpriseID 是企业租户标识（CN 的 X-Enterprise-Id）。
	EnterpriseID string
	// Nickname 是展示用昵称。
	Nickname string
	// DeviceToken 是 X-Device-Token；空表示不注入。
	DeviceToken string
	// FilePath 是凭证来源文件路径（刷新后原子写回此处）；空表示非文件来源。
	FilePath string
}

// Credential 的顶层 JSON 形态。
type credentialJSON struct {
	Auth *struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
	} `json:"auth"`
	Account *struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
	DeviceToken string `json:"device_token"`

	// 扁平形态的字段。
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// RegisterPathHint 是凭证文件名的识别提示（auth.parse 用它判断文件归属）。
const RegisterPathHint = "workbuddy"

// ParseCredential 解析凭证 JSON。
//
// fallbackRegion 是账号未声明 realm 时的兜底域（来自插件配置）。
func ParseCredential(raw []byte, fallbackRegion Region) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var parsed credentialJSON
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}

	cred := &Credential{}
	if parsed.Auth != nil {
		cred.AccessToken = strings.TrimSpace(parsed.Auth.AccessToken)
		cred.RefreshToken = strings.TrimSpace(parsed.Auth.RefreshToken)
		cred.ExpiresAt = parsed.Auth.ExpiresAt
		cred.Domain = strings.TrimSpace(parsed.Auth.Domain)
		cred.realm = normalizeRealmStrict(parsed.Auth.Realm)
	} else {
		cred.AccessToken = strings.TrimSpace(parsed.AccessToken)
		cred.RefreshToken = strings.TrimSpace(parsed.RefreshToken)
		cred.ExpiresAt = parsed.ExpiresAt
		cred.Domain = strings.TrimSpace(parsed.Domain)
		cred.realm = normalizeRealmStrict(parsed.Realm)
	}
	if parsed.Account != nil {
		cred.UID = strings.TrimSpace(parsed.Account.UID)
		cred.EnterpriseID = strings.TrimSpace(parsed.Account.EnterpriseID)
		cred.Nickname = strings.TrimSpace(parsed.Account.Nickname)
	} else {
		cred.UID = strings.TrimSpace(parsed.UID)
		cred.EnterpriseID = strings.TrimSpace(parsed.EnterpriseID)
		cred.Nickname = strings.TrimSpace(parsed.Nickname)
	}
	cred.DeviceToken = strings.TrimSpace(parsed.DeviceToken)

	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return nil, fmt.Errorf("credential has neither accessToken nor refreshToken")
	}
	if cred.realm == "" {
		if inferred := realmFromDomain(cred.Domain); inferred != "" {
			cred.realm = inferred
		} else {
			cred.realm = fallbackRegion
		}
	}
	return cred, nil
}

// normalizeRealmStrict 只接受明确的 cn / global 值，其余返回空串。
func normalizeRealmStrict(raw string) Region {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(RegionCN):
		return RegionCN
	case string(RegionGlobal):
		return RegionGlobal
	}
	return ""
}

// realmFromDomain 按域名推断域。
//
// 用域名后缀而非完全匹配：国际版有多个子域（www.workbuddy.ai、api...），
// 而 cn 域全部落在 codebuddy.cn / tencent.com 之下。
func realmFromDomain(domain string) Region {
	trimmed := strings.ToLower(strings.TrimSpace(domain))
	trimmed = strings.TrimPrefix(trimmed, "https://")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	if trimmed == "workbuddy.ai" || strings.HasSuffix(trimmed, ".workbuddy.ai") {
		return RegionGlobal
	}
	return RegionCN
}

// Realm 返回凭证所属域。
func (c *Credential) Realm() Region {
	if c == nil {
		return RegionCN
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.realm
}

// SetRealm 覆盖凭证域（账号级覆盖优先于插件配置兜底）。
func (c *Credential) SetRealm(region Region) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.realm = region
}

// AccessTokenValue 加锁读取 access token。
func (c *Credential) AccessTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken
}

// RefreshTokenValue 加锁读取 refresh token。
func (c *Credential) RefreshTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RefreshToken
}

// DomainValue 加锁读取域名。
func (c *Credential) DomainValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Domain
}

// UIDValue 加锁读取 UID。
func (c *Credential) UIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UID
}

// NicknameValue 加锁读取昵称。
func (c *Credential) NicknameValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Nickname
}

// EnterpriseIDValue 加锁读取企业标识。
func (c *Credential) EnterpriseIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.EnterpriseID
}

// DeviceTokenValue 加锁读取设备令牌。
func (c *Credential) DeviceTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.DeviceToken
}

// NeedsRefresh 报告 access token 是否即将过期（或已过期）。
//
// 没有过期时间（0）时保守地认为需要刷新：这种情况通常来自手写凭证，
// 刷一次就能拿到真实的 expiresAt。
func (c *Credential) NeedsRefresh(withinSeconds int64, nowUnix int64) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(c.RefreshToken) == "" {
		// 没有 refresh token 就刷不了，交给上游返回 401。
		return false
	}
	if c.ExpiresAt <= 0 {
		return true
	}
	return nowUnix+withinSeconds >= c.ExpiresAt
}

// ApplyTokenRefresh 写回刷新结果。
//
// 采用「两段式快照校验」：调用方在发起网络请求前取快照，拿到响应后在本方法里
// 比对当前值是否仍是快照——若已被并发刷新过，则放弃写回，避免用旧响应覆盖新值。
func (c *Credential) ApplyTokenRefresh(accessToken, refreshToken, domain string, expiresAt int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if trimmed := strings.TrimSpace(accessToken); trimmed != "" {
		c.AccessToken = trimmed
	}
	if trimmed := strings.TrimSpace(refreshToken); trimmed != "" {
		c.RefreshToken = trimmed
	}
	if trimmed := strings.TrimSpace(domain); trimmed != "" {
		c.Domain = trimmed
	}
	if expiresAt > 0 {
		c.ExpiresAt = expiresAt
	}
}

// Snapshot 返回 access/refresh token 的当前值，供两段式刷新校验使用。
func (c *Credential) Snapshot() (accessToken, refreshToken string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken, c.RefreshToken
}

// UnchangedSince 报告凭证是否仍与快照一致。
func (c *Credential) UnchangedSince(accessToken, refreshToken string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken == accessToken && c.RefreshToken == refreshToken
}

// StorageJSON 把凭证序列化为嵌套形态，用于写回 CPA 的凭证文件。
//
// 写成嵌套形是刻意的：CPA 的 auth 文件里还可能有用户自己维护的字段
// （disabled / prefix / proxy_url / note / weight），调用方应先做**合并更新**
// 而不是用本方法的输出整体替换（见 MergeStorageJSON）。
func (c *Credential) StorageJSON() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	auth := map[string]any{
		"accessToken":  c.AccessToken,
		"refreshToken": c.RefreshToken,
		"expiresAt":    c.ExpiresAt,
	}
	if c.Domain != "" {
		auth["domain"] = c.Domain
	}
	if c.realm != "" {
		auth["realm"] = string(c.realm)
	}
	account := map[string]any{}
	if c.UID != "" {
		account["uid"] = c.UID
	}
	if c.EnterpriseID != "" {
		account["enterpriseId"] = c.EnterpriseID
	}
	if c.Nickname != "" {
		account["nickname"] = c.Nickname
	}
	out := map[string]any{
		// type 必须写入：宿主用它**直接判定归属**，有它就按 provider 找到
		// 本插件，不再遍历插件。缺它时宿主会把「当前询问的插件的 identifier」
		// 兜底填进 provider，归属判定过宽的前置插件会抢走本插件的凭证，
		// 导致账号在宿主侧被归到别的 provider（页面表现为账号凭空消失）。
		"type": ProviderKey,
		"auth": auth,
	}
	if len(account) > 0 {
		out["account"] = account
	}
	if c.DeviceToken != "" {
		out["device_token"] = c.DeviceToken
	}
	return json.MarshalIndent(out, "", "  ")
}

// MergeStorageJSON 在原凭证 JSON 上做合并更新，返回新的字节。
//
// 基于原文件合并而不是从头构造：CPA 的 auth 文件里可能有用户自己维护的
// disabled / prefix / proxy_url / note / weight 等字段，重建会静默丢掉它们。
func MergeStorageJSON(original []byte, cred *Credential) ([]byte, error) {
	updated, errCred := cred.StorageJSON()
	if errCred != nil {
		return original, errCred
	}
	if len(original) == 0 {
		return updated, nil
	}
	var existing map[string]any
	if errUnmarshal := json.Unmarshal(original, &existing); errUnmarshal != nil {
		// 原文件不是对象（或已损坏）：退回整体替换。
		return updated, nil
	}
	var fresh map[string]any
	if errUnmarshal := json.Unmarshal(updated, &fresh); errUnmarshal != nil {
		return original, errUnmarshal
	}
	mergeMaps(existing, fresh)
	// 兜底：无论原文件有没有 type，合并结果都要带它。
	// 用户从别处复制进来的旧凭证常常缺这个字段，补上后归属就确定了。
	if _, okType := existing["type"]; !okType {
		existing["type"] = ProviderKey
	}
	return json.MarshalIndent(existing, "", "  ")
}

// mergeMaps 把 src 的键值递归合并进 dst（src 优先）。
func mergeMaps(dst, src map[string]any) {
	for key, value := range src {
		if srcMap, okSrc := value.(map[string]any); okSrc {
			if dstMap, okDst := dst[key].(map[string]any); okDst {
				mergeMaps(dstMap, srcMap)
				continue
			}
		}
		dst[key] = value
	}
}

// SaveCredentialFile 原子写回凭证文件。
func SaveCredentialFile(path string, cred *Credential) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil
	}
	original, errRead := os.ReadFile(trimmed)
	if errRead != nil {
		original = nil
	}
	merged, errMerge := MergeStorageJSON(original, cred)
	if errMerge != nil {
		return fmt.Errorf("merge credential file %s: %w", trimmed, errMerge)
	}
	tmp := trimmed + ".tmp"
	if errWrite := os.WriteFile(tmp, merged, 0o600); errWrite != nil {
		return fmt.Errorf("write credential file %s: %w", tmp, errWrite)
	}
	if errRename := os.Rename(tmp, trimmed); errRename != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace credential file %s: %w", trimmed, errRename)
	}
	return nil
}

// LooksLikeCredential 判断一份 JSON 是否属于本插件。
//
// **归属判定必须偏严**：宿主会把所有非内建格式的凭证文件依次喂给每个插件，
// 一旦误吞别家的凭证，宿主会用它写出本插件的凭证结构，等于污染了对方的账号
// （而且这个错误是静默的）。反过来，漏认只是加载失败，用户立刻会发现。
//
// 因此只接受**本插件特有的证据**，从强到弱：
//  1. 文件里的 type 字段或宿主传入的 provider 提示是 workbuddy；
//  2. 文件名符合本插件的命名约定（workbuddy 前缀/中缀）；
//  3. 具备本插件独有的嵌套结构（auth.accessToken + auth.refreshToken）。
//
// 刻意**不**接受顶层的 accessToken / refreshToken / device_token：
// 这些是通用字段，别的 provider 也用（例如某些插件同样写 device_token），
// 拿它们做判据必然跨插件误吞。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	// 1) 显式声明（最可靠，也是推荐的写法）。
	if strings.EqualFold(strings.TrimSpace(provider), ProviderKey) {
		return true
	}
	if typeValue, okType := raw["type"].(string); okType && strings.EqualFold(strings.TrimSpace(typeValue), ProviderKey) {
		return true
	}

	// 2) 文件名约定。
	if fileNameMatchesConvention(fileName) {
		return true
	}

	// 3) 本插件独有的嵌套结构：auth 里同时有 accessToken 与 refreshToken。
	// 单有 accessToken 不足以判定（其它 provider 也可能这样嵌套）。
	if nested, okNested := raw["auth"].(map[string]any); okNested {
		_, hasAccess := nested["accessToken"]
		_, hasRefresh := nested["refreshToken"]
		if hasAccess && hasRefresh {
			return true
		}
	}
	return false
}

// fileNameMatchesConvention 判断文件名是否符合本插件的命名约定。
//
// 约定：workbuddy.json / workbuddy-<id>.json / workbuddy_<id>.json，
// 或名字里含 workbuddy 段（如 my-workbuddy.json）。
func fileNameMatchesConvention(fileName string) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base == "" {
		return false
	}
	if base == ProviderKey {
		return true
	}
	for _, separator := range []string{"-", "_", "."} {
		if strings.HasPrefix(base, ProviderKey+separator) ||
			strings.Contains(base, separator+ProviderKey) {
			return true
		}
	}
	return false
}

// ProviderKey 是本插件在 CPA 里占用的 provider 键。
//
// 定义在 cb 包而非 main：凭证归属判定需要它，而判定逻辑属于协议层。
// main 里有一份同名常量用于注册，两者必须一致（见 .go 文件的编译期断言）。
const ProviderKey = "workbuddy"
