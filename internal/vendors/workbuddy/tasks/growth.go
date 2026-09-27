package tasks

// 本文件实现连登兑换与抽奖、猫猫旅行、夜猫子、开学季四套闭环。
//
// 共同特征：都挂在「签到之后」或独立整点执行，且全部按天幂等。

import (
	"freetier2api-plugin/internal/vendors/workbuddy"
	"context"
	"fmt"
	"strings"
	"time"
)

// 连登状态类型（Streak / StreakTier）定义在 activity.go —— 它们是上游接口的
// 返回结构，属于协议层；本文件只消费它们。合并前两处各有一份同名同形的定义，
// 是跨包重复，已去重。

// RunStreakBonus 执行单账号的连登闭环。
//
// 步骤（顺序有依赖）：
//  1. 补签昨日（有漏签且有补签卡时）；
//  2. 领取新手礼包与活动补偿（每号一次，无则静默跳过）；
//  3. 兑换所有已解锁档位；
//  4. 抽完所有抽奖次数。
func RunStreakBonus(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential) (string, error) {
	parts := make([]string, 0, 4)

	if message := makeupYesterday(ctx, client, credential); message != "" {
		parts = append(parts, message)
	}

	// 礼包与补偿：上游用业务错误表达「已领过」，因此错误被忽略。
	for _, claim := range []struct {
		name string
		path string
	}{
		{"新手礼包", workbuddy.ClaimGiftPath()},
		{"活动补偿", workbuddy.ClaimCompensationPath()},
	} {
		if credit, errClaim := client.ClaimBillingReward(ctx, credential, claim.path); errClaim == nil && credit > 0 {
			parts = append(parts, fmt.Sprintf("%s +%d", claim.name, credit))
		}
	}

	streak, errStreak := client.GrowthStreak(ctx, credential)
	if errStreak != nil {
		return strings.Join(parts, "；"), errStreak
	}

	// 兑换已解锁档位。未解锁时上游返回 403，属预期，静默跳过。
	redeemed := 0
	for _, tier := range streak.Tiers {
		if strings.EqualFold(tier.Status, "locked") || strings.EqualFold(tier.Status, "claimed") {
			continue
		}
		if errRedeem := client.GrowthRedeemTier(ctx, credential, tier.Tier); errRedeem != nil {
			continue
		}
		redeemed++
		if !sleepCtx(ctx, reportGap) {
			return strings.Join(parts, "；"), ctx.Err()
		}
	}
	if redeemed > 0 {
		parts = append(parts, fmt.Sprintf("兑换 %d 个档位", redeemed))
	}

	// 抽奖次数只能从兑换获得，因此必须在兑换之后。
	chances, errChances := client.LotteryChances(ctx, credential)
	if errChances == nil && chances > 0 {
		drawn := 0
		for index := 0; index < chances; index++ {
			if _, errDraw := client.LotteryDraw(ctx, credential); errDraw != nil {
				break
			}
			drawn++
			if !sleepCtx(ctx, 2*time.Second) {
				return strings.Join(parts, "；"), ctx.Err()
			}
		}
		if drawn > 0 {
			parts = append(parts, fmt.Sprintf("抽奖 %d 次", drawn))
		}
	}
	return strings.Join(parts, "；"), nil
}

// makeupYesterday 补签昨日（有漏签且有补签卡时）。
func makeupYesterday(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential) string {
	missed, errMissed := client.HeatmapYesterdayMissed(ctx, credential)
	if errMissed != nil || !missed {
		return ""
	}
	streak, errStreak := client.GrowthStreak(ctx, credential)
	if errStreak != nil || streak.MakeupCards <= 0 {
		return ""
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if errUse := client.UseMakeupCard(ctx, credential, yesterday); errUse != nil {
		return ""
	}
	return "补签昨日"
}

// RunTravel 执行单账号猫猫旅行的一个动作。
//
// 状态机：无猫 → 领养；有猫 → 按 status 分派（idle 派出 / arrived 领奖 / traveling 跳过）。
func RunTravel(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential) string {
	info, errInfo := client.BuddyInfo(ctx, credential)
	if errInfo != nil {
		return ""
	}
	if !info.HasBuddy {
		return adoptBuddy(ctx, client, credential)
	}

	status, errStatus := client.TravelStatus(ctx, credential)
	if errStatus != nil {
		return ""
	}
	switch status.State {
	case "arrived":
		if status.RecordID <= 0 {
			return ""
		}
		credit, errClaim := client.TravelClaim(ctx, credential, status.RecordID)
		if errClaim != nil {
			return ""
		}
		return fmt.Sprintf("旅行领奖 +%d", credit)
	case "idle":
		if status.DailyLimitReached {
			return ""
		}
		if errDepart := client.TravelDepart(ctx, credential, travelLocationID); errDepart != nil {
			return ""
		}
		return "派出旅行"
	default:
		// traveling 或未知状态：本轮不做动作。
		return ""
	}
}

// travelLocationID 是旅行目的地（古镇客栈）。
//
// 四个地点的收益与时长完全相同，因此没有「最优解」，取固定值即可。
const travelLocationID = 4

// adoptBuddy 领养第一只猫。
//
// **必须先上报一条活跃事件**：上游的 first_buddy 任务以「当日有活跃上报」
// 为前置，缺了它领养恒失败于 "first_buddy task not completed yet"。
func adoptBuddy(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential) string {
	conversationID := fmt.Sprintf("wb2api-adopt-%d", nowUnixMs())
	if errReport := client.ReportChatActivity(ctx, credential, conversationID, "", "", ""); errReport != nil {
		return ""
	}
	if !sleepCtx(ctx, reportGap) {
		return ""
	}
	// 同意协议是幂等的，重复调用无害。
	if errAgreement := client.BuddyAgreement(ctx, credential); errAgreement != nil {
		return ""
	}
	credit, errFirst := client.BuddyFirst(ctx, credential)
	if errFirst != nil {
		// 门槛未达（当日活跃上报不足）：明天再试，今天不再重试轰炸。
		return ""
	}
	return fmt.Sprintf("领养成功 +%d", credit)
}

// RunBlackcat 补足夜猫子任务。
//
// 窗口外不做任何事（调用方应先检查），窗口内按差额补对话次数。
func RunBlackcat(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential) (string, error) {
	if !InNightWindow(time.Now()) {
		return "", nil
	}
	need, errNeed := client.BlackcatNeed(ctx, credential)
	if errNeed != nil {
		return "", errNeed
	}
	if need <= 0 {
		return "", nil
	}
	completed := int64(0)
	for index := int64(0); index < need; index++ {
		conversationID := fmt.Sprintf("wb2api-night-%d-%d", nowUnixMs(), index)
		// 真实对话：上游要求夜猫子判据是「窗口内的真实 glm-5.2 对话」。
		if errChat := client.DesktopChat(ctx, credential, "", "glm-5.2", "GLM-5.2", conversationID); errChat != nil {
			break
		}
		if errReport := client.ReportChatActivity(ctx, credential, conversationID, "", "glm-5.2", "GLM-5.2"); errReport != nil {
			break
		}
		completed++
		if !sleepCtx(ctx, nightChatGap) {
			return "", ctx.Err()
		}
	}
	if completed == 0 {
		return "", nil
	}
	return fmt.Sprintf("夜间对话 %d 次", completed), nil
}

// InNightWindow 报告当前是否在夜猫子计分窗口内（23:00 - 08:00）。
func InNightWindow(now time.Time) bool {
	hour := now.Hour()
	return hour >= 23 || hour < 8
}

// RunSchool 执行开学季闭环。
//
// 活动期外静默返回（不报错）：活动结束后排程继续跑也不会做无用功，
// 无需下线代码。
func RunSchool(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential) (string, error) {
	status, errStatus := client.SchoolTasks(ctx, credential)
	if errStatus != nil {
		return "", errStatus
	}
	if !status.InPeriod {
		return "", nil
	}

	completed := 0
	for _, task := range status.Tasks {
		switch task.Code {
		case "task_student_verify":
			// 需微信真实学生认证，无法自动化。
			continue
		}
		if strings.EqualFold(task.Status, "claimed") {
			continue
		}
		// viewed 是计数前置：未激活的任务，后续行为事件不计分。
		if errView := client.SchoolTaskViewed(ctx, credential, task.Code); errView != nil {
			continue
		}
		if errRun := runSchoolTask(ctx, client, credential, task.Code); errRun != nil {
			continue
		}
		// 等异步计分落定再领奖。
		if !waitSchoolDone(ctx, client, credential, task.Code) {
			continue
		}
		if _, errClaim := client.SchoolTaskClaim(ctx, credential, task.Code); errClaim != nil {
			continue
		}
		completed++
	}

	// 抽完抽奖次数。
	drawn := 0
	if chances, errChances := client.SchoolChances(ctx, credential); errChances == nil {
		for index := 0; index < chances; index++ {
			if _, errDraw := client.SchoolDraw(ctx, credential); errDraw != nil {
				break
			}
			drawn++
			if !sleepCtx(ctx, 2*time.Second) {
				break
			}
		}
	}
	if completed == 0 && drawn == 0 {
		return "", nil
	}
	return fmt.Sprintf("完成 %d 个任务、抽奖 %d 次", completed, drawn), nil
}

// runSchoolTask 执行单个开学季任务的行为。
func runSchoolTask(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential, code string) error {
	switch code {
	case "share_invite":
		// 纯前端上报，服务端不校验真实分享回执。
		return client.SchoolShareComplete(ctx, credential)
	case "desktop_chat_1_time":
		// 需要一次真实桌面对话 + 桌面事件链。
		conversationID := fmt.Sprintf("wb2api-school-%d", nowUnixMs())
		requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, "", "fast-model", conversationID)
		if errChat != nil {
			return errChat
		}
		return client.ReportDesktopEvents(ctx, credential, withFingerprint(credential,
			DesktopChatSequence(conversationID, requestID, randomHex(16), "fast-model", "fast-model")))
	case "chat_3_times":
		// 3 条小程序对话埋点（无需真实会话）。
		for index := 0; index < 3; index++ {
			conversationID := fmt.Sprintf("wb2api-chat-%d-%d", nowUnixMs(), index)
			if errReport := client.ReportMPEvents(ctx, credential, SchoolChatTimesEvents(conversationID, "")); errReport != nil {
				return errReport
			}
			if !sleepCtx(ctx, mpActionGap) {
				return ctx.Err()
			}
		}
		return nil
	case "expert_use":
		// 小程序专家链（用固定的返校季专家）。
		conversationID := fmt.Sprintf("wb2api-exp-%d", nowUnixMs())
		return client.ReportMPEvents(ctx, credential,
			workbuddy.SchoolExpertUseEvents(schoolExpertID, schoolExpertName, conversationID))
	}
	return fmt.Errorf("unsupported school task %s", code)
}

// 开学季专家（活动内置的固定专家）。
const (
	schoolExpertID   = "ex_jB0dyFIQJEWa"
	schoolExpertName = "论文写作导师"
)

// waitSchoolDone 轮询等待开学季任务的异步计分落定。
//
// 上报 200 ≠ 计分：上游计分是异步的（实测 2.5 秒内落定），
// 立即回读会误判「未达标」从而跳过领奖。
func waitSchoolDone(ctx context.Context, client *workbuddy.Client, credential *workbuddy.Credential, code string) bool {
	for attempt := 0; attempt < schoolPollLoops; attempt++ {
		if !sleepCtx(ctx, schoolPollGap) {
			return false
		}
		status, errStatus := client.SchoolTasks(ctx, credential)
		if errStatus != nil {
			continue
		}
		for _, task := range status.Tasks {
			if task.Code == code && task.TargetCount > 0 && task.Progress >= task.TargetCount {
				return true
			}
		}
	}
	return false
}

// withFingerprint 给事件链注入桌面指纹（供包内直接调用 ReportDesktopEvents 的场景）。
func withFingerprint(credential *workbuddy.Credential, events []map[string]any) []map[string]any {
	base := desktopFingerprint(credential)
	out := make([]map[string]any, 0, len(events))
	for _, event := range events {
		out = append(out, mergeEvent(base, event))
	}
	return out
}
