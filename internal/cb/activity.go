package cb

// 本文件实现成长体系的活动接口：连登、旅行、夜猫子、开学季、礼包与补偿。
//
// 这些接口全部挂在对话域或计费域，形态各异（信封嵌套、幂等码、业务错误表达状态），
// 因此逐个封装成方法，让上层逻辑只管业务语义。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Streak 是连登状态。
type Streak struct {
	// Days 是连续登录天数。
	Days int64
	// MakeupCards 是补签卡余额。
	MakeupCards int64
	// Tiers 是各兑换档位。
	Tiers []StreakTier
}

// StreakTier 是一个兑换档位。
type StreakTier struct {
	// Tier 是档位标识（7d / 14d / 28d）。
	Tier string
	// Status 是状态：locked / claimed / 空（可兑换）。
	Status string
}

// GrowthStreak 查询连登状态。
func (c *Client) GrowthStreak(ctx context.Context, cred *Credential) (*Streak, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthStreakPath, nil)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		Streak struct {
			Days int64 `json:"days"`
		} `json:"streak"`
		MakeupCards struct {
			Balance int64 `json:"balance"`
		} `json:"makeup_cards"`
		RedemptionStatus struct {
			// 档位状态按 tier_7d_status / tier_14d_status / tier_28d_status 三个键给出。
			Tier7d  string `json:"tier_7d_status"`
			Tier14d string `json:"tier_14d_status"`
			Tier28d string `json:"tier_28d_status"`
		} `json:"redemption_status"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode streak: %w", errUnmarshal)
	}
	streak := &Streak{Days: payload.Streak.Days, MakeupCards: payload.MakeupCards.Balance}
	for _, item := range []struct {
		tier   string
		status string
	}{
		{"7d", payload.RedemptionStatus.Tier7d},
		{"14d", payload.RedemptionStatus.Tier14d},
		{"28d", payload.RedemptionStatus.Tier28d},
	} {
		if strings.TrimSpace(item.status) == "" {
			continue
		}
		streak.Tiers = append(streak.Tiers, StreakTier{Tier: item.tier, Status: item.status})
	}
	return streak, nil
}

// GrowthRedeemTier 兑换连登档位。
//
// 未解锁时上游返回 403「连续登录天数不足」，属预期结果，调用方静默跳过。
func (c *Client) GrowthRedeemTier(ctx context.Context, cred *Credential, tier string) error {
	clientToken := newHexID()
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, growthRedeemPath, map[string]any{
		"tier": strings.TrimSpace(tier), "client_token": clientToken,
	})
	return errCall
}

// LotteryChances 查询可用抽奖次数。
func (c *Client) LotteryChances(ctx context.Context, cred *Credential) (int, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthLotterySummary, nil)
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Chances int `json:"chances"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return 0, fmt.Errorf("decode lottery summary: %w", errUnmarshal)
	}
	return payload.Chances, nil
}

// LotteryDraw 执行一次抽奖。
func (c *Client) LotteryDraw(ctx context.Context, cred *Credential) (string, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, growthLotteryDraw, map[string]any{
		"client_token": newHexID(),
	})
	if errCall != nil {
		return "", errCall
	}
	return string(data), nil
}

// HeatmapYesterdayMissed 报告昨日是否漏签。
func (c *Client) HeatmapYesterdayMissed(ctx context.Context, cred *Credential) (bool, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthHeatmapPath, nil)
	if errCall != nil {
		return false, errCall
	}
	yesterday := time.Now().AddDate(0, 0, -1).In(cstZone).Format("2006-01-02")
	// 响应形态：cells 数组或日期为键的 map，两种都试。
	var wrapper struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int64  `json:"score"`
		} `json:"cells"`
	}
	if errUnmarshal := json.Unmarshal(data, &wrapper); errUnmarshal == nil {
		for _, cell := range wrapper.Cells {
			if cell.Date == yesterday {
				return cell.Score == 0, nil
			}
		}
	}
	var byDate map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &byDate); errUnmarshal == nil {
		if raw, okRaw := byDate[yesterday]; okRaw {
			var cell struct {
				Score int64 `json:"score"`
			}
			if errCell := json.Unmarshal(raw, &cell); errCell == nil {
				return cell.Score == 0, nil
			}
		}
	}
	return false, nil
}

// UseMakeupCard 使用补签卡补签指定日期。
func (c *Client) UseMakeupCard(ctx context.Context, cred *Credential, date string) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, growthMakeupCardsPath, map[string]any{
		"target_date": strings.TrimSpace(date),
	})
	return errCall
}

// cstZone 是上游自然日使用的时区（固定 UTC+8，不依赖系统 tzdata）。
var cstZone = time.FixedZone("CST", 8*3600)

// BuddyInfo 是猫档案。
type BuddyInfo struct {
	// HasBuddy 表示是否已领养。
	HasBuddy bool
}

// BuddyInfo 查询猫档案。
func (c *Client) BuddyInfo(ctx context.Context, cred *Credential) (*BuddyInfo, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, buddyInfoPath, nil)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode buddy info: %w", errUnmarshal)
	}
	trimmed := strings.TrimSpace(string(payload.Buddy))
	return &BuddyInfo{HasBuddy: trimmed != "" && trimmed != "null"}, nil
}

// BuddyAgreement 同意领养协议（幂等）。
func (c *Client) BuddyAgreement(ctx context.Context, cred *Credential) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyAgreementPath, map[string]any{"agree": true})
	return errCall
}

// BuddyFirst 领取第一只猫。
//
// 失败通常因为「当日活跃上报不足」（first_buddy 任务未完成），
// 这不是账号问题，调用方应视为「今天跳过」而不是错误。
func (c *Client) BuddyFirst(ctx context.Context, cred *Credential) (int64, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyFirstPath, map[string]any{})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &payload)
	return payload.Credit, nil
}

// TravelStatus 是旅行状态。
type TravelStatus struct {
	// State 是状态：idle（可派出）/ traveling（在途）/ arrived（可领奖）。
	State string
	// RecordID 是本次旅行的记录标识（领奖必填，为 0 表示无可领）。
	RecordID int64
	// DailyLimitReached 表示今日已派出过。
	DailyLimitReached bool
}

// TravelStatus 查询旅行状态。
func (c *Client) TravelStatus(ctx context.Context, cred *Credential) (*TravelStatus, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, buddyTravelStatusPath, nil)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		State             string `json:"state"`
		Status            string `json:"status"`
		RecordID          int64  `json:"record_id"`
		DailyLimitReached bool   `json:"daily_limit_reached"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode travel status: %w", errUnmarshal)
	}
	return &TravelStatus{
		State:             firstNonEmpty(payload.State, payload.Status),
		RecordID:          payload.RecordID,
		DailyLimitReached: payload.DailyLimitReached,
	}, nil
}

// TravelDepart 派猫出门旅行。
func (c *Client) TravelDepart(ctx context.Context, cred *Credential, locationID int) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyTravelDepartPath, map[string]any{
		"location_id": locationID,
	})
	return errCall
}

// TravelClaim 领取旅行奖励。
func (c *Client) TravelClaim(ctx context.Context, cred *Credential, recordID int64) (int64, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyTravelClaimPath, map[string]any{
		"record_id": recordID,
	})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		RewardCredit int64 `json:"reward_credit"`
		Credit       int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &payload)
	return firstNonZero(payload.RewardCredit, payload.Credit), nil
}

// BlackcatNeed 返回夜猫子任务的缺口次数。
func (c *Client) BlackcatNeed(ctx context.Context, cred *Credential) (int64, error) {
	tasks, errTasks := c.ListTasks(ctx, cred)
	if errTasks != nil {
		return 0, errTasks
	}
	for _, task := range tasks {
		if task.Code != "black_cat" {
			continue
		}
		if task.Claimed || task.Done() {
			return 0, nil
		}
		need := task.Target - task.Current
		if need < 0 {
			need = 0
		}
		return need, nil
	}
	return 0, nil
}

// 礼包与补偿的路径访问器（供上层遍历调用）。
func ClaimGiftPath() string         { return claimGiftPath }
func ClaimCompensationPath() string { return claimCompPath }

// ClaimBillingReward 领取计费域的一次性奖励（礼包 / 补偿）。
//
// 已领过时上游返回业务错误，调用方按「无新增」处理。
func (c *Client) ClaimBillingReward(ctx context.Context, cred *Credential, path string) (int64, error) {
	endpoints := GetEndpoints(cred.Realm())
	data, errCall := c.billingJSON(cred, endpoints.BillingBase+strings.TrimSpace(path), http.MethodPost, map[string]any{})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &payload)
	return payload.Credit, nil
}

// SchoolStatus 是开学季活动状态。
type SchoolStatus struct {
	// InPeriod 表示当前是否在活动期内。
	InPeriod bool
	// Tasks 是活动任务。
	Tasks []SchoolTask
}

// SchoolTask 是一个开学季任务。
type SchoolTask struct {
	// Code 是任务标识。
	Code string
	// Status 是状态：pending / in_progress / claimed / completed。
	Status string
	// Progress / TargetCount 是进度。
	Progress    int64
	TargetCount int64
}

// SchoolTasks 查询开学季任务。
func (c *Client) SchoolTasks(ctx context.Context, cred *Credential) (*SchoolStatus, error) {
	endpoints := GetEndpoints(cred.Realm())
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, schoolBasePath+"/tasks", nil)
	if errCall != nil {
		return nil, errCall
	}
	_ = endpoints
	var payload struct {
		InPeriod bool `json:"in_period"`
		Tasks    []struct {
			Code        string `json:"task_code"`
			Status      string `json:"status"`
			Progress    int64  `json:"progress"`
			TargetCount int64  `json:"target_count"`
		} `json:"tasks"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode school tasks: %w", errUnmarshal)
	}
	status := &SchoolStatus{InPeriod: payload.InPeriod}
	for _, item := range payload.Tasks {
		status.Tasks = append(status.Tasks, SchoolTask{
			Code: item.Code, Status: item.Status,
			Progress: item.Progress, TargetCount: item.TargetCount,
		})
	}
	return status, nil
}

// SchoolTaskViewed 激活一个开学季任务（pending → in_progress）。
//
// 这是**计数前置**：未激活的任务，后续行为事件不会被计分。
func (c *Client) SchoolTaskViewed(ctx context.Context, cred *Credential, code string) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost,
		schoolBasePath+"/tasks/"+url.PathEscape(strings.TrimSpace(code))+"/viewed", map[string]any{})
	return errCall
}

// SchoolTaskClaim 领取开学季任务奖励，返回获得的抽奖次数。
func (c *Client) SchoolTaskClaim(ctx context.Context, cred *Credential, code string) (int64, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost,
		schoolBasePath+"/tasks/"+url.PathEscape(strings.TrimSpace(code))+"/claim", map[string]any{})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		ChanceGranted int64 `json:"chance_granted"`
	}
	_ = json.Unmarshal(data, &payload)
	return payload.ChanceGranted, nil
}

// SchoolShareComplete 完成分享任务。
//
// 纯前端上报：服务端不校验真实分享回执，直接调用即点亮。
func (c *Client) SchoolShareComplete(ctx context.Context, cred *Credential) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, schoolBasePath+"/tasks/share-complete",
		map[string]any{"channel": "wechat"})
	return errCall
}

// SchoolChances 查询开学季抽奖次数余额。
func (c *Client) SchoolChances(ctx context.Context, cred *Credential) (int, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, schoolBasePath+"/config", nil)
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Chance struct {
			Balance int `json:"balance"`
		} `json:"chance"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return 0, fmt.Errorf("decode school config: %w", errUnmarshal)
	}
	return payload.Chance.Balance, nil
}

// SchoolDraw 执行一次开学季抽奖。
func (c *Client) SchoolDraw(ctx context.Context, cred *Credential) (string, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, schoolBasePath+"/wheel/draw",
		map[string]any{"draw_uuid": newHexID()})
	if errCall != nil {
		return "", errCall
	}
	return string(data), nil
}

// SchoolExpertUseEvents 构造开学季专家使用事件链（4 事件）。
func SchoolExpertUseEvents(expertID, expertName, conversationID string) []map[string]any {
	requestID := "wb2api-exp-req-" + randomHexForExport(6)
	return []map[string]any{
		{
			"eventCode": "expert_summon_click", "id": expertID, "name": expertName,
			"type": "16-BackToSchool", "position": 0,
		},
		{
			"eventCode": "expert_summoned", "id": expertID, "name": expertName,
			"type": "16-BackToSchool",
		},
		{
			"eventCode": "expert_actual_use", "id": expertID, "name": expertName,
			"type": "16-BackToSchool", "characterCount": 14, "expertType": "builtin",
			"conversationId": conversationID, "requestId": requestID,
		},
		{
			"eventCode": "chat_request_send", "inputLength": 14, "isPlan": false,
			"maxSteps": 500, "expertId": expertID, "expertName": expertName,
			"conversationId": conversationID, "requestId": requestID,
			"parentConversationId": conversationID,
			"codebuddy.session_id": conversationID,
		},
	}
}

// growthJSON 发起一次成长域请求并解开信封。
func (c *Client) growthJSON(ctx context.Context, cred *Credential, method, path string, body any) (json.RawMessage, error) {
	endpoints := GetEndpoints(cred.Realm())
	var reader *strings.Reader
	if body != nil {
		encoded, errEncode := json.Marshal(body)
		if errEncode != nil {
			return nil, fmt.Errorf("encode %s request: %w", path, errEncode)
		}
		reader = strings.NewReader(string(encoded))
	} else {
		reader = strings.NewReader("")
	}
	req, errReq := http.NewRequestWithContext(ctx, method, endpoints.ChatBase+path, reader)
	if errReq != nil {
		return nil, fmt.Errorf("build %s request: %w", path, errReq)
	}
	c.applyBillingHeaders(req, cred)
	// 开学季链路走计费域。
	if strings.HasPrefix(path, schoolBasePath) {
		data, errCall := c.billingJSON(cred, endpoints.BillingBase+path, method, bodyOrEmpty(body))
		return data, errCall
	}
	return c.doJSON(req)
}

// bodyOrEmpty 归一化请求体（nil 时给空对象，上游要求 body 是对象）。
func bodyOrEmpty(body any) any {
	if body == nil {
		return map[string]any{}
	}
	return body
}

// firstNonZero 返回第一个非零值。
func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
