package tabbit

// 凭证刷新与可用性校验。

import (
	"context"
	"fmt"
)

func ValidateCredential(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("credential is nil")
	}
	if cred.APIKey == "" && cred.SessionToken == "" {
		return fmt.Errorf("tabbit credential missing api_key and session_token")
	}
	return nil
}
