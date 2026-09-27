package qoder

// 本文件实现 Qoder 的每日签到（领 100 Credits）。
//
// 关键抓包事实（移植自 qoder2api，结论性证据）：
//   - 真实发放走 campaigns 流程：GET /sash/api/v1/me/campaigns → POST .../{id}/claim（空 body）；
//   - legacy 的 /daily-check-in/claim 已 DISABLED，对未领取日也返回 409，
//     只能读 status 做统计，**绝不能用它判断「是否已领取」**；
//   - 请求只需 device token + cosy-clienttype: 10（无需 cosy 签名）；
//   - 活动窗口是 10:00 → 次日 10:00，因此用「窗口开始日期」而不是自然日。
//
// 区域差异：签到域名必须按区域选。国际版（openapi.qoder.sh）**根本没有
// 每日签到计划**，只有 VIEW_DETAILS 类促销活动。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/vendors/qoder/bridge"
	"freetier2api-plugin/internal/vendors/qoder/qoderapi"
)

// 签到状态取值。
const (
	// CheckinClaimed 表示本次成功领取。
	CheckinClaimed = "claimed"
	// CheckinAlreadyClaimed 表示今日已领取（幂等命中）。
	CheckinAlreadyClaimed = "already_claimed"
	// CheckinNoCampaign 表示没有可领取的活动。
	CheckinNoCampaign = "no_campaign"
	// CheckinNoToken 表示凭证里没有可用 token。
	CheckinNoToken = "no_token"
	// CheckinError 表示执行失败。
	CheckinError = "error"
)

// checkinHTTPTimeout 是签到相关请求的上限。
const checkinHTTPTimeout = 60 * time.Second

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	// Status 是结果状态（见上面的常量）。
	Status string
	// Amount 是本次领取的积分数。
	Amount int
	// Message 是可读说明。
	Message string
	// WindowDate 是签到窗口日期（YYYY-MM-DD），用于按窗口幂等。
	WindowDate string
	// StreakDays 是连续签到天数（来自只读统计）。
	StreakDays int
	// TotalClaimDays 是累计领取天数。
	TotalClaimDays int
	// TotalRewardCredits 是累计获得积分。
	TotalRewardCredits int
}

// campaignInfo 是活动列表条目。
type campaignInfo struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	StartAt     int64  `json:"startAt"`
	EndAt       int64  `json:"endAt"`
	Benefit     *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
}

// claimResponse 是领取响应。
type claimResponse struct {
	GrantID  string `json:"grantId"`
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
	Benefit  *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
}

// dailyCheckinStatus 是 legacy 接口的只读统计。
type dailyCheckinStatus struct {
	Status             string `json:"status"`
	CurrentStreakDays  int    `json:"currentStreakDays"`
	TotalClaimDays     int    `json:"totalClaimDays"`
	TotalRewardCredits int    `json:"totalRewardCredits"`
}

// Checkin 执行一次签到。
func Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error) {
	result := &CheckinResult{Status: CheckinError}
	bearer := checkinBearer(cred.Token)
	if bearer == "" {
		result.Status = CheckinNoToken
		result.Message = "凭证里没有可用 token"
		return result, nil
	}
	result = campaignsCheckin(ctx, cred.Region, bearer, result)
	readDailyCheckinStats(ctx, cred.Region, bearer, result)
	return result, nil
}

// checkinBearer 取签到用的 bearer。
//
// 只需 device token：上游对签到接口不校验 cosy 签名，用 PAT 反而会失败。
func checkinBearer(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	deviceToken, _ := bridge.ParseOAuthSecret(trimmed)
	if deviceToken != "" {
		return deviceToken
	}
	return trimmed
}

// campaignsCheckin 是真实发放积分的领取流程。
func campaignsCheckin(ctx context.Context, region qoderapi.Region, bearer string, result *CheckinResult) *CheckinResult {
	status, body, raw := doCheckinRequest(ctx, region, http.MethodGet, "/sash/api/v1/me/campaigns", bearer, nil)
	if status != http.StatusOK {
		result.Message = fmt.Sprintf("查询活动失败 HTTP %d: %s", status, truncateForMessage(raw, 300))
		return result
	}
	list, okList := body.(map[string]any)
	if !okList {
		result.Message = fmt.Sprintf("活动列表格式异常: %s", truncateForMessage(raw, 300))
		return result
	}
	campaigns := parseCampaigns(list["campaigns"])

	var target *campaignInfo
	var benefitCampaign *campaignInfo
	alreadyClaimed := false
	for index := range campaigns {
		campaign := &campaigns[index]
		if campaign.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		benefitCampaign = campaign
		switch campaign.ClaimStatus {
		case "CLAIMABLE":
			target = campaign
		case "CLAIMED":
			alreadyClaimed = true
		}
	}

	// 窗口日期取活动 startAt：每日窗口 10:00 → 次日 10:00，
	// 用窗口日期可避免 10:00 前把昨日窗口误记到今天。
	windowSource := target
	if windowSource == nil {
		windowSource = benefitCampaign
	}
	if windowSource != nil && windowSource.StartAt > 0 {
		result.WindowDate = time.Unix(windowSource.StartAt, 0).Format("2006-01-02")
	}

	if target == nil {
		if alreadyClaimed {
			result.Status = CheckinAlreadyClaimed
			result.Message = "今日已领取"
			return result
		}
		result.Status = CheckinNoCampaign
		// 实测：国际版根本没有每日签到计划（/sash/api/v1/me/daily-check-in*
		// 返回 404），只有 VIEW_DETAILS 类促销活动。明确说明比笼统的
		// 「无可用活动」好——否则用户会反复排查自己哪里配错了。
		if region == qoderapi.RegionGlobal {
			result.Message = "该区域无每日签到活动（国际版没有签到计划）"
		} else {
			result.Message = "无可用签到活动"
		}
		return result
	}

	claimPath := fmt.Sprintf("/sash/api/v1/me/campaigns/%s/claim", target.CampaignID)
	status, body, raw = doCheckinRequest(ctx, region, http.MethodPost, claimPath, bearer, nil)
	if status != http.StatusOK {
		result.Message = fmt.Sprintf("领取失败 HTTP %d: %s", status, truncateForMessage(raw, 300))
		return result
	}
	var claim claimResponse
	encoded, _ := json.Marshal(body)
	if json.Unmarshal(encoded, &claim) != nil {
		result.Message = fmt.Sprintf("领取响应格式异常: %s", truncateForMessage(raw, 300))
		return result
	}
	switch claim.Status {
	case "CLAIMED":
		if claim.Replayed {
			// 上游对重复领取返回 CLAIMED + replayed:true，语义是幂等命中。
			result.Status = CheckinAlreadyClaimed
			result.Message = "今日已领取"
			return result
		}
		result.Status = CheckinClaimed
		if claim.Benefit != nil {
			result.Amount = claim.Benefit.Amount
		}
		if result.Amount == 0 {
			result.Amount = 100
		}
		result.Message = fmt.Sprintf("领取成功 +%d (%s)", result.Amount, target.CampaignKey)
		return result
	default:
		result.Message = fmt.Sprintf("未知状态: %s", claim.Status)
		return result
	}
}

// readDailyCheckinStats 只读 legacy 统计（绝不通过它领取）。
//
// 该接口对未领取日也返回 409，因此它的 status 不能用来判断「是否已领取」；
// 但它的连续天数与累计数据是准的，用来补充展示。
func readDailyCheckinStats(ctx context.Context, region qoderapi.Region, bearer string, result *CheckinResult) {
	status, body, _ := doCheckinRequest(ctx, region, http.MethodGet, "/sash/api/v1/me/daily-check-in/status", bearer, nil)
	if status != http.StatusOK {
		return
	}
	var parsed dailyCheckinStatus
	encoded, _ := json.Marshal(body)
	if json.Unmarshal(encoded, &parsed) != nil || parsed.Status == "" {
		return
	}
	if parsed.CurrentStreakDays > result.StreakDays {
		result.StreakDays = parsed.CurrentStreakDays
	}
	if parsed.TotalClaimDays > result.TotalClaimDays {
		result.TotalClaimDays = parsed.TotalClaimDays
	}
	if parsed.TotalRewardCredits > result.TotalRewardCredits {
		result.TotalRewardCredits = parsed.TotalRewardCredits
	}
}

// parseCampaigns 把活动列表条目解成结构体。
func parseCampaigns(raw any) []campaignInfo {
	items, okItems := raw.([]any)
	if !okItems {
		return nil
	}
	out := make([]campaignInfo, 0, len(items))
	for _, item := range items {
		encoded, errMarshal := json.Marshal(item)
		if errMarshal != nil {
			continue
		}
		var parsed campaignInfo
		if json.Unmarshal(encoded, &parsed) == nil {
			out = append(out, parsed)
		}
	}
	return out
}

// checkinHeaders 是桌面端签到所需的请求头（抓包确认）。
//
// cosy-clienttype: 10 是桌面端标识；上游按它放行签到接口，
// 不需要 cosy 签名（这是签到与对话接口最大的差异）。
func checkinHeaders(bearer string) map[string]string {
	return map[string]string{
		"authorization":   "Bearer " + bearer,
		"accept":          "application/json",
		"accept-language": "zh-CN",
		"user-agent":      "Qoder",
		"cosy-clienttype": "10",
	}
}

// checkinBase 返回该区域签到接口的域名。
//
// 必须按账号区域选：国际版 device token 打到国内域名会得到 401 TOKEN_EXPIRE
// （上游 qoder2api 写死了国内域名，因此国际版账号签到必然失败）。
func checkinBase(region qoderapi.Region) string {
	return qoderapi.GetEndpoints(region).SashBase
}

// doCheckinRequest 发送签到相关请求，返回 (状态码, 解析后的 JSON, 原始正文)。
func doCheckinRequest(ctx context.Context, region qoderapi.Region, method, path, bearer string, reqBody any) (int, any, string) {
	base := checkinBase(region)
	url := base + path
	var body io.Reader
	if reqBody != nil {
		encoded, errMarshal := json.Marshal(reqBody)
		if errMarshal != nil {
			return 0, nil, "encode request: " + errMarshal.Error()
		}
		body = bytes.NewReader(encoded)
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, url, body)
	if errRequest != nil {
		return 0, nil, fmt.Sprintf("build request: %v", errRequest)
	}
	for key, value := range checkinHeaders(bearer) {
		req.Header.Set(key, value)
	}
	if method == http.MethodPost {
		req.Header.Set("origin", base)
		if reqBody == nil {
			// 抓包确认 claim 请求无 body；显式置 0 避免上游把空 body 当异常。
			req.ContentLength = 0
		}
	}
	resp, errDo := httpx.Client(ctx, checkinHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, nil, fmt.Sprintf("do request: %v", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &parsed)
	}
	return resp.StatusCode, parsed, string(data)
}
