package core

import "time"

// nowUnix 返回当前 Unix 秒。抽成函数便于测试注入固定时间。
func nowUnix() int64 { return time.Now().Unix() }
