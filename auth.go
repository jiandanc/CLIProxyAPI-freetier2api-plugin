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
	"freetier2api-plugin/internal/vendors/workbuddy"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/logger"
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
// 归属判定分三层（证据由强到弱）：显式 provider 键 → 特征字段 → 文件名启发式。
// 既不能漏认自己的文件，也不能把别的 provider 的凭证误吞。
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
	if !workbuddy.LooksLikeCredential(raw, rpc.FileName, rpc.Provider) {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	cfg := loadedConfig()
	credential, errCredential := workbuddy.ParseCredential(rpc.RawJSON, defaultRealmForParse(cfg))
	if errCredential != nil {
		return nil, newPluginError("workbuddy_credential_invalid", errCredential.Error(), http.StatusUnprocessableEntity)
	}

	label := firstNonEmptyString(credential.NicknameValue(), credential.UIDValue(), rpc.FileName, rpc.Path)
	logger.Info("parsed workbuddy auth %s (realm=%s, oauth=%t)",
		label, credential.Realm(), credential.RefreshTokenValue() != "")

	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    buildAuthData(credential, rpc.FileName, rpc.RawJSON),
	})
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

// buildAuthData 构造宿主契约的凭证记录。
func buildAuthData(credential *workbuddy.Credential, fileName string, storageJSON []byte) pluginapi.AuthData {
	id := strings.TrimSuffix(fileName, ".json")
	if id == "" {
		id = credential.UIDValue()
	}
	return pluginapi.AuthData{
		Provider:    providerKey,
		ID:          id,
		FileName:    fileName,
		Label:       firstNonEmptyString(credential.NicknameValue(), credential.UIDValue()),
		StorageJSON: storageJSON,
		Metadata:    credentialMetadata(credential),
		Attributes:  credentialAttributes(credential),
		// 让宿主在启动后不久就驱动一次刷新校验。
		NextRefreshAfter: time.Now().Add(authRefreshInterval),
	}
}

// credentialMetadata 是回给宿主的可变元数据。
//
// refresh_token 必须在这里（见文件头注释）；token 本身绝不回传。
func credentialMetadata(credential *workbuddy.Credential) map[string]any {
	metadata := map[string]any{
		"realm": string(credential.Realm()),
	}
	if uid := credential.UIDValue(); uid != "" {
		metadata["uid"] = uid
	}
	if nickname := credential.NicknameValue(); nickname != "" {
		metadata["nickname"] = nickname
	}
	if refreshToken := credential.RefreshTokenValue(); refreshToken != "" {
		metadata["refresh_token"] = refreshToken
		metadata["auth_mode"] = "oauth"
	}
	if expiresAt := credential.ExpiresAt; expiresAt > 0 {
		metadata["expires_at"] = expiresAt
	}
	return metadata
}

// credentialAttributes 是回给宿主的不可变属性（不含任何凭证材料）。
func credentialAttributes(credential *workbuddy.Credential) map[string]string {
	attributes := map[string]string{
		"realm": string(credential.Realm()),
	}
	if uid := credential.UIDValue(); uid != "" {
		attributes["uid"] = uid
	}
	if nickname := credential.NicknameValue(); nickname != "" {
		attributes["nickname"] = nickname
	}
	if domain := credential.DomainValue(); domain != "" {
		attributes["domain"] = domain
	}
	if credential.RefreshTokenValue() != "" {
		attributes["auth_mode"] = "oauth"
	}
	return attributes
}

// handleAuthRefresh 校验凭证并在必要时刷新。
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
		return nil, newPluginError("workbuddy_credential_missing", errCredential.Error(), http.StatusUnauthorized)
	}

	client := newUpstreamClient(ctx)
	// 先尝试刷新（refresh_token 存在且临近过期时才有实际动作）。
	if errRefresh := client.RefreshToken(credential); errRefresh != nil {
		if isCredentialRejected(errRefresh) {
			logger.Error("auth refresh rejected for %s: %v", rpc.AuthID, errRefresh)
			return nil, errorToPluginError(errRefresh)
		}
		// 抖动或未知错误：保留原凭证，稍后重试。
		logger.Debug("auth refresh deferred for %s: %v", rpc.AuthID, errRefresh)
		return okEnvelope(pluginapi.AuthRefreshResponse{
			Auth:             refreshedAuthData(rpc, rpc.StorageJSON, credential),
			NextRefreshAfter: time.Now().Add(authRefreshRetryAfter),
		})
	}

	storage := rpc.StorageJSON
	if updated, errStorage := workbuddy.MergeStorageJSON(rpc.StorageJSON, credential); errStorage == nil {
		storage = updated
	}
	logger.Info("auth refresh succeeded for %s (realm=%s)", rpc.AuthID, credential.Realm())
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             refreshedAuthData(rpc, storage, credential),
		NextRefreshAfter: time.Now().Add(authRefreshInterval),
	})
}

// refreshedAuthData 在刷新后重建凭证记录，保留宿主侧的既有字段。
func refreshedAuthData(rpc authRefreshRPCRequest, storageJSON []byte, credential *workbuddy.Credential) pluginapi.AuthData {
	data := buildAuthData(credential, rpc.AuthID, storageJSON)
	// 保留宿主传入的标识字段，避免因插件重建而丢失。
	if strings.TrimSpace(rpc.AuthID) != "" {
		data.ID = rpc.AuthID
	}
	return data
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
	credential *workbuddy.Credential
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
func credentialForAuth(ctx context.Context, callbackID string, storageJSON []byte, authID string, attributes map[string]string) (*workbuddy.Credential, error) {
	cfg := loadedConfig()
	if len(storageJSON) > 0 {
		credential, errParse := workbuddy.ParseCredential(storageJSON, defaultRealmForParse(cfg))
		if errParse == nil {
			applyAttributeOverrides(credential, attributes)
			return credential, nil
		}
	}
	if trimmed := strings.TrimSpace(authID); trimmed != "" {
		if cached, okCache := cachedCredential(trimmed); okCache {
			return cached, nil
		}
		raw, errFetch := fetchAuthJSON(ctx, callbackID, trimmed)
		if errFetch == nil && len(raw) > 0 {
			credential, errParse := workbuddy.ParseCredential(raw, defaultRealmForParse(cfg))
			if errParse == nil {
				applyAttributeOverrides(credential, attributes)
				storeCredential(trimmed, credential)
				return credential, nil
			}
		}
	}
	return nil, fmt.Errorf("credential is not available for auth %s", authID)
}

// applyAttributeOverrides 用宿主传来的属性覆盖凭证字段。
//
// realm 允许被覆盖（账号可以自带域声明）；token 类字段不允许——
// 属性里根本不该有它们，若出现也应忽略而不是采信。
func applyAttributeOverrides(credential *workbuddy.Credential, attributes map[string]string) {
	if credential == nil || len(attributes) == 0 {
		return
	}
	if realm := strings.TrimSpace(attributes["realm"]); realm != "" {
		credential.SetRealm(workbuddy.NormalizeRegion(realm))
	}
}

func cachedCredential(authID string) (*workbuddy.Credential, bool) {
	credentialCacheMu.Lock()
	defer credentialCacheMu.Unlock()
	entry, okEntry := credentialCache[authID]
	if !okEntry || time.Since(entry.fetchedAt) > credentialCacheTTL {
		return nil, false
	}
	return entry.credential, true
}

func storeCredential(authID string, credential *workbuddy.Credential) {
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
