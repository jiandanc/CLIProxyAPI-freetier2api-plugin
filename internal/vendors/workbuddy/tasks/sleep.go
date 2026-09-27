package tasks

import (
	"context"
	"time"
)

// sleepCtx 可取消的睡眠。返回 false 表示上下文已取消。
//
// 任务链上的每个动作之间都要按上游的风控节奏间隔（见 fingerprint.go 的节流
// 参数），而整个闭环必须可被取消——用 time.Sleep 会让取消后仍要等满间隔。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
