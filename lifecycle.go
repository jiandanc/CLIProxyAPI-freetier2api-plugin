package main

// 本文件管理插件生命周期中的两类并发资源：
//   - 后台循环（任务调度、模型清单刷新）：可取消，register 后启动，reconfigure 不重启；
//   - 在途流式转发：每个请求一个 goroutine，shutdown 时必须先取消并等待，
//     让它们在宿主卸载动态库之前完成 host.stream.close。
//
// 与 hostgate.go 的关系：后台循环与流式转发都会发起宿主调用，
// 因此必须先停它们，再等待宿主调用排空。

import (
	"context"
	"sync"
	"time"

	"freetier2api-plugin/internal/logger"
)

var (
	pluginLifecycleMu sync.Mutex
	pluginRegistered  bool

	backgroundMu     sync.Mutex
	backgroundCancel context.CancelFunc
	backgroundWG     sync.WaitGroup
)

// startBackgroundWork 启动后台循环（幂等：已在运行时不重复启动）。
func startBackgroundWork() {
	backgroundMu.Lock()
	defer backgroundMu.Unlock()
	if backgroundCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	backgroundCancel = cancel

	backgroundWG.Add(1)
	go func() {
		defer backgroundWG.Done()
		runTaskScheduler(ctx)
	}()
}

// stopBackgroundWork 取消后台循环并等待其退出。
func stopBackgroundWork() {
	backgroundMu.Lock()
	cancel := backgroundCancel
	backgroundCancel = nil
	backgroundMu.Unlock()
	if cancel != nil {
		cancel()
	}
	backgroundWG.Wait()
}

// backgroundRunning 报告后台循环是否在运行（供管理页展示）。
func backgroundRunning() bool {
	backgroundMu.Lock()
	defer backgroundMu.Unlock()
	return backgroundCancel != nil
}

var (
	streamMu       sync.Mutex
	streamCancels  = map[uint64]context.CancelFunc{}
	streamWG       sync.WaitGroup
	nextStreamKey  uint64
	pluginStreamID uint64
)

// beginPluginStream 注册一个流式转发任务，返回绑定它的 context、取消函数与结束回调。
//
// 结束回调必须被调用（通常 defer），否则 shutdown 会一直等到超时。
// 内部用 sync.Once 保证重复调用安全。
func beginPluginStream() (context.Context, context.CancelFunc, func()) {
	ctx, cancel := context.WithCancel(context.Background())

	streamMu.Lock()
	nextStreamKey++
	key := nextStreamKey
	streamCancels[key] = cancel
	streamMu.Unlock()
	streamWG.Add(1)

	var once sync.Once
	finish := func() {
		once.Do(func() {
			streamMu.Lock()
			delete(streamCancels, key)
			streamMu.Unlock()
			cancel()
			streamWG.Done()
		})
	}
	return ctx, cancel, finish
}

// waitPluginStreams 取消所有在途流式转发，并在限定时间内等待它们退出。
//
// 这里的等待是**有界**的：取消之后这些 goroutine 只做清理（关闭上游流与宿主流），
// 即使它们阻塞在不可取消的宿主调用上，随后的宿主调用排空也会兜住。
func waitPluginStreams(timeout time.Duration) {
	streamMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(streamCancels))
	for _, cancel := range streamCancels {
		cancels = append(cancels, cancel)
	}
	inflight := len(streamCancels)
	streamMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	if inflight == 0 {
		streamWG.Wait()
		return
	}
	if timeout <= 0 {
		timeout = shutdownWaitTimeout
	}
	done := make(chan struct{})
	go func() {
		streamWG.Wait()
		close(done)
	}()
	start := time.Now()
	select {
	case <-done:
		return
	case <-time.After(timeout):
		streamMu.Lock()
		remaining := len(streamCancels)
		streamMu.Unlock()
		logger.Error("shutdown: %d stream task(s) still running after %s",
			remaining, time.Since(start).Round(time.Millisecond))
	}
}

// nextPluginStreamID 生成进程内唯一的流标识（仅用于日志与诊断）。
func nextPluginStreamID() uint64 {
	streamMu.Lock()
	defer streamMu.Unlock()
	pluginStreamID++
	return pluginStreamID
}
