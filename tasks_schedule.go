package main

// 本文件实现五个定时任务族。
//
// 全部遵循同一形状：遍历可用账号 → 每个账号执行 → 结果写状态。
// 国际版账号在每一族入口都直接跳过（它们没有这套成长体系）。

import (
	"context"
	"fmt"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/workbuddy"
	"freetier2api-plugin/internal/vendors/workbuddy/tasks"
)

// runCheckinAll 执行全账号签到。
//
// 签到末尾会驱动连登闭环与开学季（与原项目一致）：这三件事的数据互相依赖
// （签到提升连登天数 → 解锁兑换档位 → 兑换产出抽奖次数），分开排程反而要处理时序。
func runCheckinAll(ctx context.Context) {
	if !effectiveAutoCheckin() {
		logger.Debug("checkin skipped: auto checkin is disabled")
		return
	}
	logger.Info("checkin: starting for all accounts")
	count := 0
	forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		// 由供应商自己决定是否提供签到：WorkBuddy 国际版与 Qoder 国际版
		// 都实测没有签到活动（签到端点 404），这里不硬编码区域判断。
		if !account.vendor.SupportsCheckin() {
			return nil
		}
		result, errCheckin := account.vendor.Checkin(ctx, account.credential)
		if errCheckin != nil {
			logger.Error("checkin %s: %v", account.label(), errCheckin)
			return nil
		}
		if result == nil {
			return nil
		}
		recordCheckinResult(account.credential, result)
		if result.Already {
			logger.Info("checkin %s: already checked in today", account.label())
		} else {
			logger.Info("checkin %s: %s", account.label(), result.Message)
		}
		count++
		return nil
	})
	logger.Info("checkin: finished for %d account(s)", count)

	// 签到后驱动从属闭环。
	if effectiveAutoTasks() {
		runStreakBonusAll(ctx)
		runSchoolAll(ctx)
	}
}

// runActivityAll 执行全账号活跃上报。
//
// 一条上报同时点亮 growth 连登与解锁 first_buddy（领养前置），
// 因此它对任务体系是必需品，不只是「保活」。
func runActivityAll(ctx context.Context) {
	logger.Info("activity: starting for all accounts")
	count := 0
	forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		if account.isGlobal() {
			return nil
		}
		client := newUpstreamClient(ctx)
		conversationID := fmt.Sprintf("wb2api-%d", nowUnixMs())
		if errReport := client.ReportChatActivity(ctx, accountNative(account), conversationID, "", "", ""); errReport != nil {
			logger.Error("activity %s: %v", account.label(), errReport)
			return nil
		}
		// 只读自检：上报成功但连登天数仍为 0 说明事件被静默丢弃。
		checkActivityStreak(ctx, client, account)
		count++
		return nil
	})
	logger.Info("activity: finished for %d account(s)", count)
}

// checkActivityStreak 回读连登天数做自检。
//
// 只观测不重试：上报本身按天幂等，重试没有意义，但已知上游存在
// 「200 但静默丢弃」的形态，留下日志便于对账。
func checkActivityStreak(ctx context.Context, client *workbuddy.Client, account *accountContext) {
	streak, errStreak := client.GrowthStreak(ctx, accountNative(account))
	if errStreak != nil {
		logger.Debug("activity %s: streak check failed: %v", account.label(), errStreak)
		return
	}
	if streak.Days == 0 {
		logger.Error("activity %s: report succeeded but streak.days=0 (event may have been silently dropped)", account.label())
		return
	}
	logger.Info("activity %s: streak days=%d", account.label(), streak.Days)
}

// runKeepaliveAll 执行全账号 token 保活。
//
// 目的：refresh token 长期不用会失效，定期刷新能延长账号寿命。
// 连续失败达到阈值才禁用账号——单次失败可能只是网络抖动。
func runKeepaliveAll(ctx context.Context) {
	logger.Info("keepalive: starting for all accounts")
	count := 0
	forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		client := newUpstreamClient(ctx)
		if errRefresh := client.RefreshToken(accountNative(account)); errRefresh != nil {
			logger.Error("keepalive %s: %v", account.label(), errRefresh)
			return nil
		}
		raw, okRaw := getAuthJSONByIndex(ctx, account.callbackID, account.entry.AuthIndex)
		if okRaw {
			if updated, errMerge := workbuddy.MergeStorageJSON(raw, accountNative(account)); errMerge == nil {
				if _, errSave := callHostScoped(account.callbackID, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
					Name: account.entry.Name,
					JSON: updated,
				}); errSave != nil {
					logger.Debug("keepalive %s: host.auth.save failed: %v", account.label(), errSave)
				}
			}
		}
		if errSave := workbuddy.SaveCredentialFile(account.credential.FilePath, accountNative(account)); errSave != nil {
			logger.Debug("keepalive %s: save credential failed: %v", account.label(), errSave)
		}
		logger.Info("keepalive %s: token refreshed and saved", account.label())
		count++
		return nil
	})
	logger.Info("keepalive: finished for %d account(s)", count)
}

// runTravelAll 执行全账号猫猫旅行。
//
// 每趟只做一个动作（领养 / 派出 / 领奖），不轮询等待：
// 旅行是长周期状态机，一轮做完一件事即可，下次排程自然推进。
func runTravelAll(ctx context.Context) {
	logger.Info("travel: starting for all accounts")
	count := 0
	forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		if account.isGlobal() {
			return nil
		}
		client := newUpstreamClient(ctx)
		message := tasks.RunTravel(ctx, client, accountNative(account))
		if message != "" {
			logger.Info("travel %s: %s", account.label(), message)
			count++
		}
		return nil
	})
	logger.Info("travel: finished for %d account(s)", count)
}

// runBlackcatAll 执行夜猫子任务补足。
//
// 判据要求对话发生在 23:00-08:00 窗口内，因此**窗口外直接跳过**：
// 窗口外跑一遍既消耗额度又不计分。
func runBlackcatAll(ctx context.Context) {
	if !tasks.InNightWindow(nowFunc()) {
		logger.Debug("blackcat: skipped (outside the 23:00-08:00 scoring window)")
		return
	}
	logger.Info("blackcat: starting for all accounts")
	count := 0
	forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		if account.isGlobal() {
			return nil
		}
		client := newUpstreamClient(ctx)
		message, errRun := tasks.RunBlackcat(ctx, client, accountNative(account))
		if errRun != nil {
			logger.Error("blackcat %s: %v", account.label(), errRun)
			return nil
		}
		if message != "" {
			logger.Info("blackcat %s: %s", account.label(), message)
			count++
		}
		return nil
	})
	logger.Info("blackcat: finished for %d account(s)", count)
}

// runSchoolAll 执行开学季活动闭环。
//
// 活动期外整段静默跳过（原项目的 in_period 判定），因此活动结束后
// 无需下线代码，排程继续跑也不会做无用功。
func runSchoolAll(ctx context.Context) {
	logger.Info("school: starting for all accounts")
	count := 0
	forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		if account.isGlobal() {
			return nil
		}
		client := newUpstreamClient(ctx)
		summary, errRun := tasks.RunSchool(ctx, client, accountNative(account))
		if errRun != nil {
			logger.Error("school %s: %v", account.label(), errRun)
			return nil
		}
		if summary != "" {
			logger.Info("school %s: %s", account.label(), summary)
			count++
		}
		return nil
	})
	logger.Info("school: finished for %d account(s)", count)
}

// runStreakBonusAll 执行连登兑换与抽奖闭环。
//
// 挂在新签到之后：连登档位按天数解锁，签到当天恰好是「够天数」的那天，
// 此时兑换 + 抽奖能在同一次排程里一次做完。
func runStreakBonusAll(ctx context.Context) {
	logger.Info("streak: starting for all accounts")
	count := 0
	forEachAccount(ctx, "", func(ctx context.Context, account *accountContext) error {
		if account.isGlobal() {
			return nil
		}
		client := newUpstreamClient(ctx)
		summary, errRun := tasks.RunStreakBonus(ctx, client, accountNative(account))
		if errRun != nil {
			logger.Error("streak %s: %v", account.label(), errRun)
			return nil
		}
		if summary != "" {
			logger.Info("streak %s: %s", account.label(), summary)
			count++
		}
		return nil
	})
	logger.Info("streak: finished for %d account(s)", count)
}
