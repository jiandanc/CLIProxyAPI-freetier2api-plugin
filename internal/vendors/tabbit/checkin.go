package tabbit

// 签到相关实现：Tabbit 不支持签到。

import (
	"context"
	"errors"
)

var ErrCheckinUnsupported = errors.New("tabbit 不支持签到活动")

func SupportsCheckin() bool {
	return false
}

func Checkin(ctx context.Context, cred *Credential) error {
	return ErrCheckinUnsupported
}
