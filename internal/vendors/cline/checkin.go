package cline

// 本文件实现 Cline 的签到。
//
// Cline 没有签到活动，因此这里只有一个明确的「不支持」。
//
// 保留本文件而不是省略：三个供应商的签到能力都叫 checkin.go，
// 新增供应商时照抄文件名即可，不必先确认「这家有没有这个功能」。

import "errors"

// ErrCheckinUnsupported 表示本供应商没有签到活动。
var ErrCheckinUnsupported = errors.New("cline does not provide a daily check-in")
