package zcode

// 额度查询：获取 ZCode 账号额度、套餐与余量信息。
// 参考：D:\Workspace\zcode2api\app\quota.py

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/httpx"
)

// FetchQuota 查询 ZCode 账号配额与使用量。
func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is nil")
	}

	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "ZCode Coding Plan",
			TierName: "Standard",
			TierID:   "zcode",
		},
	}

	if cred.JWTToken != "" || cred.APIKey != "" {
		client := httpx.Client(ctx, 20*time.Second)

		// 1. 查询当前套餐
		reqCurrent, errReqCur := http.NewRequestWithContext(ctx, http.MethodGet, DefaultZCodeOrigin+PathBillingCurrent, nil)
		if errReqCur == nil {
			applyBillingHeaders(reqCurrent, cred)
			if respCur, errCur := client.Do(reqCurrent); errCur == nil {
				defer func() { _ = respCur.Body.Close() }()
				bodyCur, _ := io.ReadAll(io.LimitReader(respCur.Body, 1<<20))
				var resCur struct {
					Code int `json:"code"`
					Data struct {
						Plans []struct {
							Name   string `json:"name"`
							PlanID string `json:"plan_id"`
						} `json:"plans"`
					} `json:"data"`
				}
				if json.Unmarshal(bodyCur, &resCur) == nil && len(resCur.Data.Plans) > 0 {
					response.Subscription.Plan = resCur.Data.Plans[0].Name
					response.Subscription.TierName = resCur.Data.Plans[0].PlanID
				}
			}
		}

		// 2. 查询各模型余额
		reqBal, errReqBal := http.NewRequestWithContext(ctx, http.MethodGet, DefaultZCodeOrigin+PathBillingBalance, nil)
		if errReqBal == nil {
			applyBillingHeaders(reqBal, cred)
			if respBal, errBal := client.Do(reqBal); errBal == nil {
				defer func() { _ = respBal.Body.Close() }()
				bodyBal, _ := io.ReadAll(io.LimitReader(respBal.Body, 1<<20))
				var resBal struct {
					Code int `json:"code"`
					Data struct {
						Balances []struct {
							ShowName       string  `json:"show_name"`
							Model          string  `json:"model"`
							TotalUnits     float64 `json:"total_units"`
							UsedUnits      float64 `json:"used_units"`
							RemainingUnits float64 `json:"remaining_units"`
						} `json:"balances"`
					} `json:"data"`
				}
				if json.Unmarshal(bodyBal, &resBal) == nil && len(resBal.Data.Balances) > 0 {
					for _, b := range resBal.Data.Balances {
						name := firstNonEmpty(b.ShowName, b.Model)
						response.Summary = append(response.Summary,
							pluginapi.QuotaMetric{
								Key:   "rem_" + name,
								Label: name + " 剩余",
								Value: b.RemainingUnits,
								Unit:  "tokens",
							},
						)
					}
					return response, nil
				}
			}
		}
	}

	response.Summary = append(response.Summary,
		pluginapi.QuotaMetric{Key: "status", Label: "账号状态", Value: 1, Unit: "正常"},
	)
	return response, nil
}
