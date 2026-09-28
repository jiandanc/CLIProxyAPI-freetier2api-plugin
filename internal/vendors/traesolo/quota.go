package traesolo

// 额度查询：获取 TRAE SOLO 积分与权益包使用情况。
// 参考：/Users/jiandan/Workspaces/trae2api-more/internal/upstream/

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

// FetchQuota 查询 TRAE SOLO 账号积分与用量。
func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential or access token missing")
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, UgHost+EpEntUsage, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+cred.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ClientUA)

	client := httpx.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("traesolo quota request failed: %w", errDo)
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

	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "TRAE SOLO 权益包",
			TierName: "SOLO",
			TierID:   "traesolo",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "remaining_credit", Label: "剩余积分", Value: result.Data.RemainingCredit, Unit: "credits"},
			{Key: "total_credit", Label: "总额度", Value: result.Data.TotalCredit, Unit: "credits"},
			{Key: "used_credit", Label: "已用积分", Value: result.Data.UsedCredit, Unit: "credits"},
		},
	}

	return response, nil
}
