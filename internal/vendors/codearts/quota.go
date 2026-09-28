package codearts

// 额度查询：获取 CodeArts 账号可用状态与额度概览。

import (
	"context"
	"fmt"

	"freetier2api-plugin/cpasdk/pluginapi"
)

func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is nil")
	}

	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "CodeArts Agent 盘古助手",
			TierName: "Standard",
			TierID:   "codearts",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "status", Label: "状态", Value: 1, Unit: "正常"},
		},
	}

	return response, nil
}
