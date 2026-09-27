package qoder

// 本文件实现 Qoder 的对话转发辅助。
//
// Qoder 的 chat-completions 走 bridge 子包（cosy 签名 + 协议转换），
// 这里只放适配层需要的那几个桥接辅助：凭证串构造与缓存指纹。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

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
