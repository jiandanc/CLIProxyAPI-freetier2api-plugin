package zcode

// 签到相关实现：ZCode 没有签到活动体系。

import (
	"context"
	"errors"
)

// ErrCheckinUnsupported 标识本供应商不支持签到。
var ErrCheckinUnsupported = errors.New("zcode 不支持签到活动")

// SupportsCheckin 报告本供应商是否支持签到。
func SupportsCheckin() bool {
	return false
}

// Checkin 执行签到（空实现）。
func Checkin(ctx context.Context, cred *Credential) error {
	return ErrCheckinUnsupported
}
