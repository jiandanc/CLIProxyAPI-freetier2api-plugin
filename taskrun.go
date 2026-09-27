package main

// 本文件实现任务调度循环。
//
// 调度模型是**整点小时列表**而非 cron 表达式：任务只需要「每天在 9 点、21 点各跑一次」
// 这种粒度，小时列表比 cron 更好配置也更好理解。
//
// 原项目支持四类独立任务（签到、旅行、活跃、保活）加夜猫子，各自有独立开关。
// 本插件沿用同一模型，但排程配置存在 state.json（页面上可改）。

import (
	"context"
	"sort"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// 默认排程（本地时区整点）。
var defaultSchedule = map[string][]int{
	taskKindCheckin:   {9, 21},
	taskKindTravel:    {9, 21},
	taskKindActivity:  {10},
	taskKindKeepalive: {22},
	taskKindBlackcat:  {23},
}

// 任务种类。
const (
	taskKindCheckin   = "checkin"
	taskKindTravel    = "travel"
	taskKindActivity  = "activity"
	taskKindKeepalive = "keepalive"
	taskKindBlackcat  = "blackcat"
	taskKindSchool    = "school"
)

// 调度循环参数。
const (
	// schedulerWakeGrace 是迟到唤醒的宽限等待。
	//
	// 背景：机器从休眠恢复后，网络栈/DNS 需要 1-2 秒才可用，
	// 准点唤醒会因 DNS 失败而白跑一轮。晚于计划时刻才等这段宽限，准点则零延迟。
	schedulerWakeGrace = 5 * time.Second
	// schedulerLateThreshold 是判定「迟到」的阈值。
	schedulerLateThreshold = 1 * time.Second
	// schedulerReconfigure 是配置变更信号缓冲。
	schedulerReconfigure = "reconfigure"
)

var (
	schedulerMu      sync.Mutex
	schedulerPoke    = make(chan string, 4)
	schedulerRunning bool
)

// runTaskScheduler 是后台调度循环。
//
// 每轮计算下一次唤醒时刻与该时刻要跑的任务族，睡到点后并行派发。
// 配置变更（页面改排程）通过 pokeScheduler 打断睡眠重新计算。
func runTaskScheduler(ctx context.Context) {
	logger.Info("task scheduler started")
	for {
		kinds, next := nextScheduledWake(time.Now())
		timer := time.NewTimer(time.Until(next))

		select {
		case <-ctx.Done():
			timer.Stop()
			logger.Info("task scheduler stopped")
			return
		case signal := <-schedulerPoke:
			timer.Stop()
			if signal == schedulerReconfigure {
				logger.Debug("task scheduler reconfigured")
			}
			continue
		case <-timer.C:
			// 迟到唤醒时给网络栈一点恢复时间。
			if delay := time.Since(next); delay > schedulerLateThreshold {
				logger.Debug("task scheduler woke %s late, waiting grace period", delay.Round(time.Millisecond))
				if !sleepCtx(ctx, schedulerWakeGrace) {
					return
				}
			}
			runTaskBatch(ctx, kinds)
		}
	}
}

// runTaskBatch 并行派发同一时刻的多个任务族。
//
// 并行而非串行：签到与旅行互不依赖，串行只会让总时长叠加。
func runTaskBatch(ctx context.Context, kinds []string) {
	var waitGroup sync.WaitGroup
	for _, kind := range kinds {
		waitGroup.Add(1)
		go func(name string) {
			defer waitGroup.Done()
			runScheduledTaskContext(ctx, name)
		}(kind)
	}
	waitGroup.Wait()
}

// nextScheduledWake 返回下一次唤醒时刻与要执行的任务族。
//
// 多个任务族配在同一小时时一并返回（一次唤醒跑多个任务）。
func nextScheduledWake(now time.Time) ([]string, time.Time) {
	schedule := effectiveSchedule()
	byHour := map[int][]string{}
	hours := make([]int, 0, len(schedule))
	for kind, list := range schedule {
		for _, hour := range list {
			if hour < 0 || hour > 23 {
				continue
			}
			if _, okHour := byHour[hour]; !okHour {
				hours = append(hours, hour)
			}
			byHour[hour] = append(byHour[hour], kind)
		}
	}
	if len(hours) == 0 {
		// 没有任何排程：睡一小时再检查（用户可能随时在页面上启用）。
		return nil, now.Add(time.Hour)
	}
	sort.Ints(hours)

	// 先看今天剩下的整点，都不行就取明天最早的那个。
	for _, hour := range hours {
		candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if candidate.After(now) {
			return byHour[hour], candidate
		}
	}
	first := hours[0]
	tomorrow := now.AddDate(0, 0, 1)
	return byHour[first], time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), first, 0, 0, 0, now.Location())
}

// effectiveSchedule 返回生效的排程（页面设置优先于默认值）。
//
// 空列表表示「未配置」，回落默认值而不是禁用——沿用原项目的语义
// （用独立的 enabled 开关表达禁用，而不是用空列表这种哨兵值）。
func effectiveSchedule() map[string][]int {
	state := snapshotState()
	out := make(map[string][]int, len(defaultSchedule))
	for kind, fallback := range defaultSchedule {
		out[kind] = fallback
	}
	for kind, hours := range map[string][]int{
		taskKindCheckin:   state.CheckinHours,
		taskKindTravel:    state.TravelHours,
		taskKindActivity:  state.ActivityHours,
		taskKindKeepalive: state.KeepaliveHours,
		taskKindBlackcat:  state.BlackcatHours,
	} {
		if len(hours) > 0 {
			out[kind] = hours
		}
	}
	return out
}

// pokeScheduler 唤醒调度循环重新计算下次执行时刻。
func pokeScheduler() {
	select {
	case schedulerPoke <- schedulerReconfigure:
	default:
		// 缓冲已满说明已经有待处理的信号，丢掉即可。
	}
}

// runScheduledTask 在后台执行一个任务族（供管理接口手动触发）。
func runScheduledTask(kind string) {
	runScheduledTaskContext(context.Background(), kind)
}

// runScheduledTaskContext 执行一个任务族。
func runScheduledTaskContext(ctx context.Context, kind string) {
	switch kind {
	case taskKindCheckin:
		runCheckinAll(ctx)
	case taskKindActivity:
		runActivityAll(ctx)
	case taskKindKeepalive:
		runKeepaliveAll(ctx)
	case taskKindTravel:
		runTravelAll(ctx)
	case taskKindBlackcat:
		runBlackcatAll(ctx)
	case taskKindSchool:
		runSchoolAll(ctx)
	default:
		logger.Debug("unknown scheduled task kind %q", kind)
	}
}

// schedulerStatus 返回调度器状态（供管理页展示）。
func schedulerStatus() map[string]any {
	schedulerMu.Lock()
	running := schedulerRunning
	schedulerMu.Unlock()

	schedule := effectiveSchedule()
	kinds, next := nextScheduledWake(time.Now())
	readable := make(map[string][]int, len(schedule))
	for kind, hours := range schedule {
		readable[kind] = hours
	}
	return map[string]any{
		"running":    running,
		"schedule":   readable,
		"next_at":    next.Format(time.RFC3339),
		"next_tasks": kinds,
		"auto_tasks": effectiveAutoTasks(),
	}
}

// effectiveAutoTasks 返回是否启用任务自动闭环（页面设置优先）。
func effectiveAutoTasks() bool {
	state := snapshotState()
	if state.AutoTasks != nil {
		return *state.AutoTasks
	}
	return loadedConfig().AutoTasks
}

// effectiveAutoCheckin 返回是否启用自动签到（页面设置优先）。
func effectiveAutoCheckin() bool {
	state := snapshotState()
	if state.AutoCheckin != nil {
		return *state.AutoCheckin
	}
	return loadedConfig().AutoCheckin
}

// effectiveSettings 返回页面设置与配置合并后的可读视图。
func effectiveSettings() map[string]any {
	state := snapshotState()
	checkinAt := state.AutoCheckinAt
	if checkinAt == "" {
		checkinAt = loadedConfig().AutoCheckinAt
	}
	return map[string]any{
		"auto_checkin":    effectiveAutoCheckin(),
		"auto_checkin_at": checkinAt,
		"auto_tasks":      effectiveAutoTasks(),
		"schedule":        effectiveSchedule(),
	}
}

// sleepCtx 可取消的睡眠。返回 false 表示上下文已取消。
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

// forEachAccount 遍历本插件名下可用的账号。
//
// 统一的遍历入口：过滤掉已禁用、已不可用、域被关闭的账号，
// 并跳过没有 refresh token 的账号（它们无法参与任何需要鉴权的操作）。
func forEachAccount(ctx context.Context, callbackID string, fn func(context.Context, *accountContext) error) {
	entries, errList := listHostAuths(ctx, callbackID)
	if errList != nil {
		logger.Error("list accounts failed: %v", errList)
		return
	}
	cfg := loadedConfig()
	for _, entry := range entries {
		if entry.Disabled || entry.Unavailable {
			continue
		}
		raw, okRaw := getAuthJSONByIndex(ctx, callbackID, entry.AuthIndex)
		if !okRaw {
			continue
		}
		credential, vendor, errResolve := resolveVendorCredential(ctx, callbackID, raw, entry.AuthIndex, nil)
		if errResolve != nil {
			logger.Debug("skip account %s: %v", entry.Name, errResolve)
			continue
		}
		if !realmEnabled(cfg, vendor.Region()) {
			continue
		}
		if errRun := fn(ctx, &accountContext{
			entry:      entry,
			vendor:     vendor,
			credential: credential,
			callbackID: callbackID,
		}); errRun != nil {
			logger.Debug("account %s task failed: %v", firstNonEmptyString(entry.Label, entry.Name), errRun)
		}
		// 账号间限速：全账号瞬间并发容易被上游风控注意到。
		if !sleepCtx(ctx, accountGap) {
			return
		}
	}
}

// accountGap 是账号之间的操作间隔。
const accountGap = 800 * time.Millisecond

// accountContext 是遍历中单个账号的上下文。
type accountContext struct {
	entry      hostAuthEntry
	vendor     core.Vendor
	credential *core.Credential
	callbackID string
}

// uid 返回账号标识（用于状态记录）。
func (a *accountContext) uid() string { return a.credential.UIDValue() }

// label 返回展示名。
func (a *accountContext) label() string {
	return firstNonEmptyString(a.credential.LabelValue(), a.entry.Label, a.entry.Name, a.entry.ID)
}

// native 取回协议层凭证（任务动作需要它做上游调用）。
func (a *accountContext) native() (*workbuddy.Credential, bool) {
	native, okNative := a.credential.Native.(*workbuddy.Credential)
	return native, okNative && native != nil
}

// isGlobal 报告账号是否属于国际版。
func (a *accountContext) isGlobal() bool { return a.vendor.Region() == "global" }

// accountNative 取回账号的协议层凭证。
//
// 任务动作要调上游接口，而 core.Credential 是不带协议细节的通用形态，
// 因此需要取回供应商自己的凭证对象。取不到时返回 nil——调用方要么跳过
// 该账号，要么让上游调用报错（都好过 panic）。
func accountNative(account *accountContext) *workbuddy.Credential {
	native, _ := account.native()
	return native
}
