package workbuddy

// 本文件实现 WorkBuddy 的每日签到。
//
// 签到是 CodeBuddy 国内版独有的活动；国际版没有该活动，调用方应在
// SupportsCheckin 为 false 时跳过（不报错、不发请求）。

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	// Already 表示今天已经签过（幂等成功，不是错误）。
	Already bool
	// Credit / Energy 是本次签到所得。
	Credit int64
	Energy int64
	// Streak 是连续签到天数。
	Streak int64
}

// DailyCheckin 执行每日签到。
//
// 「今天已签到」被识别为幂等成功：上游对重复签到返回业务错误，
// 直接当失败会让自动签到任务每天报错一次。
func (c *Client) DailyCheckin(cred *Credential) (*CheckinResult, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	endpoints := GetEndpoints(region)
	paths := []string{dailyCheckinPathV2}
	if region.IsGlobal() {
		paths = []string{dailyCheckinPath, dailyCheckinPathV2}
	}

	var lastErr error
	for _, path := range paths {
		data, errCall := c.billingJSON(cred, endpoints.BillingBase+path, http.MethodPost, map[string]any{})
		if errCall != nil {
			if IsAlreadyCheckin(errCall) {
				return &CheckinResult{Already: true}, nil
			}
			lastErr = errCall
			if upstreamErr, okErr := errCall.(*Error); okErr && upstreamErr.Kind == KindNotFound {
				continue
			}
			return nil, errCall
		}
		var payload struct {
			Credit int64 `json:"credit"`
			Energy int64 `json:"energy"`
			Streak int64 `json:"streak"`
		}
		if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
			// 签到成功但响应结构不认识：仍算成功（副作用已发生）。
			return &CheckinResult{}, nil
		}
		return &CheckinResult{Credit: payload.Credit, Energy: payload.Energy, Streak: payload.Streak}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("checkin endpoint is unavailable")
}
