package tabbit

// 额度查询：Tabbit 上游无独立额度查询接口。

import (
	"context"

	"freetier2api-plugin/cpasdk/pluginapi"
)

func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "Tabbit",
			TierName: "Standard",
			TierID:   "tabbit",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "status", Label: "状态", Value: 1, Unit: "有效"},
		},
	}, nil
}
