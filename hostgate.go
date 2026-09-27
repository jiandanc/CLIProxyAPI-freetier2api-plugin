package main

// 本文件管理宿主回调（host.*）的并发准入与关闭排空。
//
// 宿主回调在当前 CPA ABI 下**不可取消**：一旦发起，就必须等宿主返回。
// 这带来两个必须处理的问题：
//  1. 并发准入 —— 限制同时在飞的 CGO 调用数量。上游挂起时，超时返回的调用仍会占用
//     一个 OS 线程直到宿主返回，不设上限会让线程数无限增长；
//  2. 关闭排空 —— Shutdown 时先禁止新的宿主调用，再等待在飞调用全部返回，最后才允许
//     宿主 dlclose 本动态库。在仍然活跃的 Go/CGO 栈上卸载会直接崩溃进程。

import (
	"errors"
	"sync"
	"time"

	"freetier2api-plugin/internal/logger"
)

const (
	// maxConcurrentHostCalls 是在飞宿主调用的上限。
	// 流式转发会同时使用 host.http.stream_read 与 host.stream.emit，
	// 任务队列还会并发跑多个账号，因此不能设得过小。
	maxConcurrentHostCalls = 64

	// shutdownDiagnosticInterval 是关闭排空期打印诊断日志的间隔。
	shutdownDiagnosticInterval = 5 * time.Second

	// shutdownWaitTimeout 只用于等待本插件自己的流式 goroutine。
	// 它们可以被取消，因此可以有界等待；宿主调用排空则是无界等待。
	shutdownWaitTimeout = 10 * time.Second
)

// errHostCallsShuttingDown 表示插件已进入关闭序，不再接受新的宿主调用。
var errHostCallsShuttingDown = errors.New("host callbacks unavailable: plugin is shutting down")

var (
	hostCallMu           sync.Mutex
	hostCallShuttingDown bool
	hostCallGate         = make(chan struct{}, maxConcurrentHostCalls)
	hostCallWG           sync.WaitGroup
)

// tryAcquireHostCall 申请一个宿主调用槽位。
//
// 准入检查与 WaitGroup.Add 必须在同一把锁内完成，否则 Wait 可能观察到零计数，
// 导致关闭排空提前返回并在活跃栈上 dlclose。
func tryAcquireHostCall() error {
	hostCallMu.Lock()
	if hostCallShuttingDown {
		hostCallMu.Unlock()
		return errHostCallsShuttingDown
	}
	hostCallWG.Add(1)
	hostCallMu.Unlock()
	hostCallGate <- struct{}{}
	return nil
}

// releaseHostCall 归还槽位。必须与 tryAcquireHostCall 成对出现。
func releaseHostCall() {
	<-hostCallGate
	hostCallWG.Done()
}

// hostCallInflight 返回当前在飞的宿主调用数。
func hostCallInflight() int { return len(hostCallGate) }

// closeHostCallAdmission 关闭准入：此后所有新的宿主调用立即失败。
func closeHostCallAdmission() {
	hostCallMu.Lock()
	hostCallShuttingDown = true
	hostCallMu.Unlock()
}

// waitHostCallsForShutdown 关闭准入并等待所有在飞宿主调用返回。
//
// 这里**刻意不做有界等待**：宿主回调不可取消，提前返回会在仍然执行的 Go/C 栈上
// 触发 dlclose。等待期间每 diagEvery 输出一次诊断，便于定位卡住的上游。
func waitHostCallsForShutdown(diagEvery time.Duration) {
	closeHostCallAdmission()
	if diagEvery <= 0 {
		diagEvery = shutdownDiagnosticInterval
	}
	if hostCallInflight() == 0 {
		hostCallWG.Wait()
		return
	}
	done := make(chan struct{})
	go func() {
		hostCallWG.Wait()
		close(done)
	}()
	ticker := time.NewTicker(diagEvery)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			logger.Error("shutdown: waiting for in-flight host callbacks inflight=%d waited=%s (host callbacks are not cancelable)",
				hostCallInflight(), time.Since(start).Round(time.Millisecond))
		}
	}
}
