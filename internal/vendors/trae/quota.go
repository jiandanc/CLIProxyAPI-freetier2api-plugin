package trae

// 额度查询：获取 Trae 账号积分与用量。

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

func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential missing")
	}

	cfg := ConfigFor(cred.Region)

	if cred.Region == RegionCN {
		req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, cfg.AuthHost+EpEntUsage, nil)
		if errReq != nil {
			return nil, errReq
		}
		req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Trae/"+cfg.IDEVersion)

		client := httpx.Client(ctx, 30*time.Second)
		resp, errDo := client.Do(req)
		if errDo != nil {
			return nil, fmt.Errorf("trae quota request failed: %w", errDo)
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode >= 400 {
			return nil, Classify(resp.StatusCode, string(body))
		}

		var result struct {
			Code int `json:"code"`
			Data struct {
				TotalCredit     float64 `json:"total_credit"`
				RemainingCredit float64 `json:"remaining_credit"`
				UsedCredit      float64 `json:"used_credit"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &result)

		return &pluginapi.QuotaFetchResponse{
			Subscription: &pluginapi.QuotaSubscription{
				Plan:     "Trae 国内版权益",
				TierName: "Standard",
				TierID:   "traecn",
			},
			Summary: []pluginapi.QuotaMetric{
				{Key: "remaining_credit", Label: "剩余积分", Value: result.Data.RemainingCredit, Unit: "credits"},
				{Key: "total_credit", Label: "总额度", Value: result.Data.TotalCredit, Unit: "credits"},
				{Key: "used_credit", Label: "已用积分", Value: result.Data.UsedCredit, Unit: "credits"},
			},
		}, nil
	}

	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "Trae 国际版",
			TierName: "Standard",
			TierID:   "traeglobal",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "status", Label: "状态", Value: 1, Unit: "有效"},
		},
	}, nil
}
