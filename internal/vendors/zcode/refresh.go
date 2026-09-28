package zcode

// 凭证刷新与可用性校验。

import (
	"context"
	"fmt"
)

// ValidateCredential 校验凭证是否可用。
func ValidateCredential(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("credential is nil")
	}
	if cred.APIKey == "" && cred.JWTToken == "" {
		return fmt.Errorf("credential missing api_key or jwt_token")
	}
	return nil
}
