package zcode

// 凭证刷新与可用性校验。
//
// ZCode 的凭证没有可续期语义：Coding Plan JWT 由上游按会话下发，access_token
// 兑换出的 API Key 长期有效，两者都不提供 refresh 流程。因此这里的职责是
// **如实校验**——判定凭证是否仍可用，而不是假装续期。

import (
	"context"
	"strings"
)

// ValidateCredential 校验凭证形态与上游可用性。
//
// 只做本地形态校验，不产生上游流量：宿主会在每次刷新时调用本方法，
// 让它去打上游等于把额度查询变成了周期性的风控信号。
func ValidateCredential(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return transientErr("zcode credential is nil")
	}
	if strings.TrimSpace(cred.APIKey) == "" && strings.TrimSpace(cred.JWTToken) == "" {
		return transientErr("zcode credential missing both api_key and jwt_token")
	}
	return nil
}
