package qoder

// 本文件实现 Qoder 的凭证续期（校验 + 刷新）。
//
// 两条路径的语义差别：
//   - PAT（pt-…）走 jobToken 交换，**会拿到新 refreshToken**；
//   - 设备令牌（dt-…）用 userinfo 保活，内容不变时不写盘
//     （否则 mtime 会无意义地变动，用户无法判断是否真的续期过）。
//
// refreshed=false 表示仅校验通过、上游没下发新凭证。

import (
	"context"
	"fmt"
	"strings"

	"freetier2api-plugin/internal/vendors/qoder/bridge"
	"freetier2api-plugin/internal/vendors/qoder/cosy"
	"freetier2api-plugin/internal/vendors/qoder/qoderapi"
)

// RefreshCredential 校验并刷新凭证。
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
