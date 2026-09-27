package qoder

// 本文件提供适配层需要的辅助：请求模板、Bridge 凭证串、凭证指纹、
// 内置模型兜底清单、续期。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"freetier2api-plugin/internal/vendors/qoder/bridge"
	"freetier2api-plugin/internal/vendors/qoder/cosy"
	"freetier2api-plugin/internal/vendors/qoder/qoderapi"
)

// 请求模板（basePromptRaw）与它的构造函数在 template.go —— 那里是移植
// 自 qoder2api 的原始实现，本文件只提供适配层需要的辅助。

// BridgeSecret 返回要交给 bridge.NewBridge 的凭证串。
//
// 上游 NewBridge 内部会自己调 ParseOAuthSecret：传 dt- 明文会丢掉
// refresh_token（会话无法续期），所以有 refresh token 时必须按 qoder2api
// 导出的 JSON 形态传，与上游桌面端行为保持一致。
func BridgeSecret(cred *Credential) string {
	if cred == nil {
		return ""
	}
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return cred.Token
	}
	payload, errMarshal := json.Marshal(map[string]string{
		"device_token":  cred.Token,
		"refresh_token": cred.RefreshToken,
	})
	if errMarshal != nil {
		return cred.Token
	}
	return string(payload)
}

// CredentialFingerprint 生成凭证指纹，用于 Bridge 缓存键。
//
// 不直接用 token 明文做缓存键：缓存键会进日志与内存 dump。
func CredentialFingerprint(cred *Credential) string {
	if cred == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(cred.Token + "|" + cred.RefreshToken + "|" + string(cred.Region)))
	return hex.EncodeToString(sum[:16])
}

// RefreshCredential 校验并刷新凭证。
//
// 两条路径的语义差别：
//   - PAT（pt-…）走 jobToken 交换，**会拿到新 refreshToken**；
//   - 设备令牌（dt-…）用 userinfo 保活，内容不变时不写盘
//     （否则 mtime 会无意义地变动，用户无法判断是否真的续期过）。
//
// refreshed=false 表示仅校验通过、上游没下发新凭证。
func RefreshCredential(ctx context.Context, cred *Credential) (*Credential, bool, error) {
	if cred == nil {
		return nil, false, fmt.Errorf("credential is nil")
	}
	deviceToken, refreshToken := bridge.ParseOAuthSecret(cred.Token)
	if strings.HasPrefix(deviceToken, "dt-") {
		// 设备令牌：用 userinfo 保活，内容不变。
		if _, errInfo := bridge.FetchUserInfoWithToken(ctx, deviceToken, cred.Region); errInfo != nil {
			return nil, false, ClassifyCredentialError(errInfo)
		}
		return cred, false, nil
	}

	// PAT：换 jobToken 拿新的 securityOauthToken / refreshToken。
	seed := cosy.FingerprintSeed("", cred.Token)
	endpoints := qoderapi.GetEndpoints(cred.Region)
	jobToken, errExchange := cosy.ExchangeJobToken(ctx, cred.Token,
		cosy.DeriveMachineID(seed), cosy.DeriveMachineToken(seed), cosy.DeriveMachineType(seed),
		endpoints.JobTokenURL)
	if errExchange != nil {
		return nil, false, ClassifyCredentialError(errExchange)
	}
	oauthToken := bridge.StrVal(jobToken, "securityOauthToken")
	if oauthToken == "" {
		return nil, false, NewError("qoder_credential_invalid",
			"jobToken 响应里没有 securityOauthToken", 401)
	}
	nextRefresh := bridge.StrVal(jobToken, "refreshToken")
	if nextRefresh == "" {
		nextRefresh = refreshToken
	}
	if oauthToken == cred.Token && nextRefresh == cred.RefreshToken {
		// 上游回了同样的值：算「有效」但不算「已刷新」，避免无意义写盘。
		return cred, false, nil
	}
	updated := &Credential{
		Token:        oauthToken,
		RefreshToken: nextRefresh,
		Region:       cred.Region,
		Label:        cred.Label,
		Email:        cred.Email,
	}
	return updated, true, nil
}

// BundledQoderModels 是内置的兜底模型清单。
//
// 来源：移植自 qoder2api 的 HandleListModels 兜底清单，不是凭空构造的；
// 真实可用集合以「刷新模型」拉到的实时清单为准。探测失败时用它兜底——
// 没有模型注册比注册一份可能过时的清单更糟（客户端会看不到任何模型）。
func BundledQoderModels() []bridge.QoderModel {
	return []bridge.QoderModel{
		{Key: "auto", DisplayName: "Auto", Enable: true, IsDefault: true},
		{Key: "ultimate", DisplayName: "Ultimate", Enable: true},
		{Key: "performance", DisplayName: "Performance", Enable: true},
		{Key: "efficient", DisplayName: "Efficient", Enable: true},
		{Key: "qmodel_38max", DisplayName: "Qwen3.8-Max", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "qmodel_latest", DisplayName: "Qwen3.7-Max", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "qmodel", DisplayName: "Qwen3.7-Plus", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "kmodel_latest", DisplayName: "Kimi-K3", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "kmodel", DisplayName: "Kimi-K2.8-Preview", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "gmodel", DisplayName: "GLM-5.3", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "gfmodel", DisplayName: "GLM-5.3-Flash", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro", Enable: true, IsReasoning: true, ContextWindow: 180000, MaxOutputTokens: 32768},
		{Key: "dfmodel", DisplayName: "DeepSeek-Flash", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
		{Key: "mmodel", DisplayName: "MiniMax-M3", Enable: true, ContextWindow: 180000, MaxOutputTokens: 16384},
	}
}
