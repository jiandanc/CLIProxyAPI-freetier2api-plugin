package main

// 本文件实现 auth provider 能力：识别凭证文件、解析、刷新。
//
// 与宿主的契约要点：宿主判断「这个凭证能不能刷新」看的是 Metadata 里的
// refresh_token（sdk/cliproxy/auth 的 authHasOAuthMetadata）。没有它，
// /auth-files/refresh 与自动刷新循环都会跳过该账号——OAuth 账号的
// access token 过期后就永远不会被续期。因此 refresh_token 必须放进 Metadata。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

const (
	// authRefreshInterval 是凭证校验成功后的下次刷新间隔。
	authRefreshInterval = 30 * time.Minute
	// authRefreshRetryAfter 是遇到抖动错误时的重试间隔。
	authRefreshRetryAfter = 5 * time.Minute
	// authRefreshHTTPTimeout 是刷新校验的请求上限。
	authRefreshHTTPTimeout = 60 * time.Second
	// credentialCacheTTL 是凭证回源查找的缓存时长。
	//
	// 凭证轮换主要由宿主驱动（它会把新 StorageJSON 交给各能力），
	// 这里的缓存主要服务管理页轮询：30 秒既够用又能及时看到换号。
	credentialCacheTTL = 30 * time.Second
	// authParseFileNameHint 是凭证文件名识别提示。
	authParseFileNameHint = workbuddy.RegisterPathHint
)

// authParseRPCRequest 与宿主的 auth.parse 请求对齐。
type authParseRPCRequest struct {
	pluginapi.AuthParseRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// authRefreshRPCRequest 与宿主的 auth.refresh 请求对齐。
type authRefreshRPCRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthParse 识别并解析本插件的凭证文件。
//
// 归属判定交给 core.ResolveVendor（三级：vendor 字段 → 文件名前缀 → 内容嗅探），
// 解析本身交给命中的供应商实例。这样新增供应商不需要改这里。
//
// 既不能漏认自己的文件，也不能把别的 provider 的凭证误吞——误吞会让宿主
// 用本插件的结构覆盖对方账号，且是静默的。
func handleAuthParse(request []byte) ([]byte, error) {
	var rpc authParseRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	if len(rpc.RawJSON) == 0 {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	var raw map[string]any
	if errUnmarshal := json.Unmarshal(rpc.RawJSON, &raw); errUnmarshal != nil {
		// 非 JSON 的凭证文件不归本插件处理。
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	vendor, okVendor := core.ResolveVendor(rpc.FileName, rpc.Provider, raw)
	if !okVendor {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	credential, errCredential := vendor.Parse(rpc.RawJSON, rpc.FileName)
	if errCredential != nil {
		return nil, newPluginError("credential_invalid", errCredential.Error(), http.StatusUnprocessableEntity)
	}

	logger.Info("parsed auth %s (vendor=%s)",
		firstNonEmptyString(credential.LabelValue(), rpc.FileName, rpc.Path), vendor.ID())

	auth, errAuth := buildAuthDataFor(vendor, credential, rpc.FileName, rpc.RawJSON)
	if errAuth != nil {
		return nil, errAuth
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: auth})
}

// defaultRealmForParse 决定解析无 realm 声明的凭证时使用的兜底域。
//
// 两个域都启用时兜底 cn（历史数据以 cn 为主）；只启用一个域时用它。
func defaultRealmForParse(cfg pluginConfig) workbuddy.Region {
	if realmEnabled(cfg, string(workbuddy.RegionCN)) {
		return workbuddy.RegionCN
	}
	return workbuddy.RegionGlobal
}

// buildAuthDataFor 构造宿主契约的凭证记录（供应商无关）。
//
// fileName 由调用方决定：
//   - auth.parse 用宿主传来的原文件名（不重命名用户已有的文件）；
//   - auth.login.poll 用 core.FileNameFor 生成的 <vendor>-<uid>.json。
func buildAuthDataFor(vendor core.Vendor, credential *core.Credential, fileName string, storageJSON []byte) (pluginapi.AuthData, error) {
	id := strings.TrimSuffix(fileName, ".json")
	if id == "" {
		id = credential.UIDValue()
	}
	storage, errStorage := ensureVendorField(storageJSON, vendor.ID())
	if errStorage != nil {
		return pluginapi.AuthData{}, errStorage
	}
	return pluginapi.AuthData{
		Provider:    providerKey,
		ID:          id,
		FileName:    fileName,
		Label:       credential.LabelValue(),
		StorageJSON: storage,
		Metadata:    credentialMetadata(credential, vendor.ID()),
		Attributes:  credentialAttributes(credential, vendor.ID()),
		// 让宿主在启动后不久就驱动一次刷新校验。
		NextRefreshAfter: time.Now().Add(authRefreshInterval),
	}, nil
}

// ensureVendorField 保证凭证 JSON 里带 vendor 字段。
//
// 这是供应商归属的**最强证据**：文件名可能被用户改掉，而 vendor 字段是
// 插件自己写进去的。缺了它，用户改名后的凭证只能靠内容嗅探归属，
// 对结构相似的供应商（WorkBuddy 与 Qoder 都有 token 字段）不可靠。
func ensureVendorField(storageJSON []byte, vendorID string) ([]byte, error) {
	if len(storageJSON) == 0 {
		return storageJSON, nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(storageJSON, &payload); errUnmarshal != nil {
		// 解析不了就原样返回：凭证内容由供应商负责，这里不强改。
		return storageJSON, nil
	}
	if existing, okExisting := payload[core.VendorKey].(string); okExisting &&
		strings.EqualFold(strings.TrimSpace(existing), vendorID) {
		return storageJSON, nil
	}
	payload[core.VendorKey] = vendorID
	merged, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return nil, fmt.Errorf("encode vendor field: %w", errMarshal)
	}
	return merged, nil
}

// credentialMetadata 是回给宿主的可变元数据。
//
// refresh_token 必须在这里（见文件头注释）；token 本身绝不回传。
func credentialMetadata(credential *core.Credential, vendorID string) map[string]any {
	metadata := map[string]any{
		core.VendorKey: vendorID,
		"realm":        credential.RegionValue(),
	}
	if uid := credential.UIDValue(); uid != "" {
		metadata["uid"] = uid
	}
	if label := credential.LabelValue(); label != "" {
		metadata["nickname"] = label
	}
	if refreshToken := credential.RefreshTokenValue(); refreshToken != "" {
		metadata["refresh_token"] = refreshToken
		metadata["auth_mode"] = "oauth"
	}
	if expiresAt := credential.ExpiresAtValue(); expiresAt > 0 {
		metadata["expires_at"] = expiresAt
	}
	return metadata
}

// credentialAttributes 是回给宿主的不可变属性（不含任何凭证材料）。
func credentialAttributes(credential *core.Credential, vendorID string) map[string]string {
	attributes := map[string]string{
		core.VendorKey: vendorID,
		"realm":        credential.RegionValue(),
	}
	if uid := credential.UIDValue(); uid != "" {
		attributes["uid"] = uid
	}
	if label := credential.LabelValue(); label != "" {
		attributes["nickname"] = label
	}
	if credential.RefreshTokenValue() != "" {
		attributes["auth_mode"] = "oauth"
	}
	return attributes
}

// handleAuthRefresh 校验凭证并在必要时刷新。
//
// 刷新逻辑**交给凭证所属的供应商**：各家的续期链路差异很大
// （WorkBuddy 是 X-Refresh-Token 换 token，Qoder 是 jobToken 交换），
// 共性只有「凭证失效要报错、抖动要保留、成功要写回」这三条处置策略。
//
// 错误分三类处置：
//   - 凭证确实失效（401/403）→ 返回错误，让宿主把该账号标记为坏；
//   - 上游抖动/未知错误 → 保留原凭证并给一个较短的下次刷新时间（不误伤账号）；
//   - 成功 → 若上游轮换了 token，把新凭证写回。
func handleAuthRefresh(request []byte) ([]byte, error) {
	var rpc authRefreshRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx, cancel := context.WithTimeout(httpx.WithCallbackID(context.Background(), rpc.HostCallbackID), authRefreshHTTPTimeout)
	defer cancel()

	credential, errCredential := credentialForAuth(ctx, rpc.HostCallbackID, rpc.StorageJSON, rpc.AuthID, rpc.Attributes)
	if errCredential != nil {
		return nil, newPluginError("credential_missing", errCredential.Error(), http.StatusUnauthorized)
	}
	vendor, okVendor := core.VendorByID(credential.VendorIDValue())
	if !okVendor {
		return nil, newPluginError("credential_missing",
			"credential has no resolvable vendor", http.StatusUnauthorized)
	}

	updated, refreshed, errRefresh := vendor.Refresh(ctx, credential)
	if errRefresh != nil {
		if isCredentialRejected(errRefresh) {
			logger.Error("auth refresh rejected for %s: %v", rpc.AuthID, errRefresh)
			return nil, errorToPluginError(errRefresh)
		}
		// 抖动或未知错误：保留原凭证，稍后重试。
		logger.Debug("auth refresh deferred for %s: %v", rpc.AuthID, errRefresh)
		auth, errData := refreshedAuthData(rpc, rpc.StorageJSON, credential)
		if errData != nil {
			return nil, errData
		}
		return okEnvelope(pluginapi.AuthRefreshResponse{
			Auth:             auth,
			NextRefreshAfter: time.Now().Add(authRefreshRetryAfter),
		})
	}

	// 上游没下发新令牌时不重写文件：否则 mtime 会无意义地变动，
	// 用户无法从文件时间判断账号是否真的续期过。
	storage := rpc.StorageJSON
	if refreshed {
		if merged, errStorage := mergeVendorStorage(rpc.StorageJSON, updated, vendor); errStorage == nil {
			storage = merged
		}
	}
	logger.Info("auth refresh succeeded for %s (vendor=%s refreshed=%t)", rpc.AuthID, vendor.ID(), refreshed)
	auth, errData := refreshedAuthData(rpc, storage, updated)
	if errData != nil {
		return nil, errData
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: time.Now().Add(authRefreshInterval),
	})
}

// mergeVendorStorage 把刷新后的凭证合并回原始 StorageJSON。
//
// 合并逻辑**交给供应商自己**（经 core.StorageMerger 可选接口）：各家的 JSON
// 结构不同（WorkBuddy 是嵌套的 auth/account，Qoder 是扁平字段，Cline 是平铺的
// accessToken/refreshToken），通用合并会把顶层字段写乱。
//
// 供应商没实现该接口时原样返回：宁可不动，也不要用错误的结构覆盖用户文件。
// 合并后补 vendor 字段，保证供应商归属始终可判定。
func mergeVendorStorage(original []byte, credential *core.Credential, vendor core.Vendor) ([]byte, error) {
	merger, okMerger := vendor.(core.StorageMerger)
	if !okMerger {
		return original, nil
	}
	merged, errMerge := merger.MergeStorageJSON(original, credential)
	if errMerge != nil {
		return nil, errMerge
	}
	return ensureVendorField(merged, vendor.ID())
}

// refreshedAuthData 在刷新后重建凭证记录，保留宿主侧的既有字段。
//
// 供应商取自凭证自身的 vendor 字段/区域（刷新场景下凭证已存在，
// 归属必然可判定），因此不需要调用方再传。
func refreshedAuthData(rpc authRefreshRPCRequest, storageJSON []byte, credential *core.Credential) (pluginapi.AuthData, error) {
	vendor, okVendor := core.VendorByID(credential.VendorIDValue())
	if !okVendor {
		return pluginapi.AuthData{}, fmt.Errorf("unknown vendor %q", credential.VendorIDValue())
	}
	data, errData := buildAuthDataFor(vendor, credential, rpc.AuthID, storageJSON)
	if errData != nil {
		return pluginapi.AuthData{}, errData
	}
	// 保留宿主传入的标识字段，避免因插件重建而丢失。
	if strings.TrimSpace(rpc.AuthID) != "" {
		data.ID = rpc.AuthID
	}
	return data, nil
}

// isCredentialRejected 报告错误是否表示凭证真的失效。
func isCredentialRejected(err error) bool {
	var upstreamErr *workbuddy.Error
	if !errors.As(err, &upstreamErr) {
		return false
	}
	switch upstreamErr.Kind {
	case workbuddy.KindSessionDead, workbuddy.KindHardCredit:
		return true
	}
	return upstreamErr.Status == http.StatusUnauthorized || upstreamErr.Status == http.StatusForbidden
}

// credentialCacheEntry 是凭证回源查找的缓存项。
type credentialCacheEntry struct {
	credential *core.Credential
	fetchedAt  time.Time
}

var (
	credentialCacheMu sync.Mutex
	credentialCache   = map[string]credentialCacheEntry{}
)

// credentialForAuth 取回某个账号的凭证。
//
// 优先用宿主随请求发来的 StorageJSON（最准），缺失时按 auth_index 回源。
//
// **必须支持回源**：宿主的管理端额度路由只发 AuthIndex/AuthID +
// Metadata/Attributes，**不发 StorageJSON**；而插件设计上又不把 token 放进
// metadata/attributes，因此只认 StorageJSON 会把额度查询误报成「凭证缺失」。
//
// 供应商归属由凭证自身决定（vendor 字段 → 文件名 → 内容嗅探），
// 因此这里不需要调用方指定供应商，新增供应商也不必改本函数。
func credentialForAuth(ctx context.Context, callbackID string, storageJSON []byte, authID string, attributes map[string]string) (*core.Credential, error) {
	if len(storageJSON) > 0 {
		if credential, okParse := parseVendorCredential(storageJSON, authID, attributes); okParse {
			return credential, nil
		}
	}
	if trimmed := strings.TrimSpace(authID); trimmed != "" {
		if cached, okCache := cachedCredential(trimmed); okCache {
			return cached, nil
		}
		raw, errFetch := fetchAuthJSON(ctx, callbackID, trimmed)
		if errFetch == nil && len(raw) > 0 {
			if credential, okParse := parseVendorCredential(raw, trimmed, attributes); okParse {
				storeCredential(trimmed, credential)
				return credential, nil
			}
		}
	}
	return nil, fmt.Errorf("credential is not available for auth %s", authID)
}

// parseVendorCredential 按归属解析一份凭证；解析失败返回 false。
//
// 归属判定用 authID 作为文件名提示（宿主侧的 ID 就是去掉 .json 的文件名），
// 这样即便凭证缺 vendor 字段也能靠文件名前缀归属。
func parseVendorCredential(raw []byte, authID string, attributes map[string]string) (*core.Credential, bool) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, false
	}
	fileName := strings.TrimSpace(authID)
	if fileName != "" && !strings.HasSuffix(strings.ToLower(fileName), ".json") {
		fileName += ".json"
	}
	vendor, okVendor := core.ResolveVendor(fileName, attributes["provider"], payload)
	if !okVendor {
		return nil, false
	}
	credential, errParse := vendor.Parse(raw, fileName)
	if errParse != nil {
		return nil, false
	}
	applyAttributeOverrides(credential, attributes)
	return credential, true
}

// applyAttributeOverrides 用宿主传来的属性覆盖凭证字段。
//
// realm 允许被覆盖（账号可以自带域声明）；token 类字段不允许——
// 属性里根本不该有它们，若出现也应忽略而不是采信。
func applyAttributeOverrides(credential *core.Credential, attributes map[string]string) {
	if credential == nil || len(attributes) == 0 {
		return
	}
	if realm := strings.TrimSpace(attributes["realm"]); realm != "" {
		credential.SetRegion(realm)
	}
}

func cachedCredential(authID string) (*core.Credential, bool) {
	credentialCacheMu.Lock()
	defer credentialCacheMu.Unlock()
	entry, okEntry := credentialCache[authID]
	if !okEntry || time.Since(entry.fetchedAt) > credentialCacheTTL {
		return nil, false
	}
	return entry.credential, true
}

func storeCredential(authID string, credential *core.Credential) {
	credentialCacheMu.Lock()
	defer credentialCacheMu.Unlock()
	credentialCache[authID] = credentialCacheEntry{credential: credential, fetchedAt: time.Now()}
}

// resetCredentialCache 清空凭证缓存（配置变更或测试使用）。
func resetCredentialCache() {
	credentialCacheMu.Lock()
	defer credentialCacheMu.Unlock()
	credentialCache = map[string]credentialCacheEntry{}
}

// firstNonEmptyString 返回第一个非空字符串。
func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// resolveVendorCredential 取回凭证并解析出它所属的供应商。
//
// 把「取凭证」与「定供应商」合成一步：几乎所有 ABI 处理器都需要这一对，
// 分开写会到处重复 VendorByID + 错误处理。
func resolveVendorCredential(ctx context.Context, callbackID string, storageJSON []byte, authID string, attributes map[string]string) (*core.Credential, core.Vendor, error) {
	credential, errCredential := credentialForAuth(ctx, callbackID, storageJSON, authID, attributes)
	if errCredential != nil {
		return nil, nil, newPluginError("credential_missing", errCredential.Error(), http.StatusUnauthorized)
	}
	vendor, okVendor := core.VendorByID(credential.VendorIDValue())
	if !okVendor {
		return nil, nil, newPluginError("credential_missing",
			"credential has no resolvable vendor", http.StatusUnauthorized)
	}
	return credential, vendor, nil
}
