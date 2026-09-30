package minimaxcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"freetier2api-plugin/internal/core"
)

// Checkin 执行账号每日签到（仅国际版支持）。
func Checkin(ctx context.Context, cred *Credential, baseURLOverride string) (*core.CheckinResult, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", http.StatusUnauthorized)
	}

	if cred.Region == RegionCN {
		return &core.CheckinResult{
			Already: false,
			Message: "国内版暂无签到活动",
		}, nil
	}

	base := AgentHostFor(cred.Region, baseURLOverride)

	// 1. 签到前必须先调用 prepare 确保账号 record 在 agent 端建立
	_ = prepare(ctx, cred, baseURLOverride)

	// 2. 查询签到看板状态
	statusTarget := buildSigninTarget(cred, base, EpCheckinStatus, http.MethodGet, "")
	statusResp, errStatus := doRequest(ctx, cred, statusTarget, base, 30*time.Second)
	if errStatus != nil {
		return nil, fmt.Errorf("check signin status: %w", errStatus)
	}
	defer func() { _ = statusResp.Body.Close() }()

	if statusResp.StatusCode == http.StatusOK {
		var statusRaw map[string]any
		if errDec := json.NewDecoder(statusResp.Body).Decode(&statusRaw); errDec == nil {
			coreMap := statusRaw
			if data, ok := statusRaw["data"].(map[string]any); ok {
				coreMap = data
			}
			if days, ok := coreMap["days"].([]any); ok {
				for _, d := range days {
					if dm, ok := d.(map[string]any); ok {
						isToday, _ := dm["is_today"].(bool)
						status := getInt(dm, "status")
						// status == 3 表示今日已领取
						if isToday && status == 3 {
							return &core.CheckinResult{
								Already: true,
								Message: "今日已完成签到",
							}, nil
						}
					}
				}
			}
		}
	}

	// 3. 领取签到积分
	claimTarget := buildSigninTarget(cred, base, EpCheckinClaim, http.MethodPost, "{}")
	claimResp, errClaim := doRequest(ctx, cred, claimTarget, base, 30*time.Second)
	if errClaim != nil {
		return nil, fmt.Errorf("claim signin: %w", errClaim)
	}
	defer func() { _ = claimResp.Body.Close() }()

	if claimResp.StatusCode == http.StatusUnauthorized || claimResp.StatusCode == http.StatusForbidden {
		return nil, core.NewPluginError("minimax_unauthorized", "MiniMax 凭证无效或已过期", http.StatusUnauthorized)
	}
	if claimResp.StatusCode >= 400 {
		return nil, fmt.Errorf("claim signin HTTP %d", claimResp.StatusCode)
	}

	var claimRaw map[string]any
	if errDec := json.NewDecoder(claimResp.Body).Decode(&claimRaw); errDec != nil {
		return nil, fmt.Errorf("decode claim response: %w", errDec)
	}

	coreMap := claimRaw
	if data, ok := claimRaw["data"].(map[string]any); ok {
		coreMap = data
	}

	claimResult := getInt(coreMap, "claim_result", "result")
	points := getInt64(coreMap, "points")

	// claim_result == 2 表示幂等命中（今日已领取过）
	if claimResult == 2 {
		return &core.CheckinResult{
			Already: true,
			Message: "今日已完成签到",
		}, nil
	}

	return &core.CheckinResult{
		Already: false,
		Credit:  points,
		Message: fmt.Sprintf("签到成功，获得 %d 积分", points),
	}, nil
}
