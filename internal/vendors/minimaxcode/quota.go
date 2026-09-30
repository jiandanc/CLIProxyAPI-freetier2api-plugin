package minimaxcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// FetchQuota 查询 MiniMax 账号的额度余额。
func FetchQuota(ctx context.Context, cred *Credential, baseURLOverride string) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", http.StatusUnauthorized)
	}

	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildSigninTarget(cred, base, EpMembership, http.MethodPost, "{}")

	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return nil, fmt.Errorf("fetch quota: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, core.NewPluginError("minimax_unauthorized", fmt.Sprintf("minimax quota HTTP %d", resp.StatusCode), http.StatusUnauthorized)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("minimax quota HTTP %d", resp.StatusCode)
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return nil, fmt.Errorf("decode quota response: %w", errDec)
	}

	coreMap := raw
	if data, ok := raw["data"].(map[string]any); ok {
		coreMap = data
	}

	planName := getString(coreMap, "plan_name")
	if planName == "" {
		planName = "MiniMax Code"
	}

	var totalRemaining, freeRemaining, purchasedRemaining float64

	if summary, ok := coreMap["op_credit_summary"].(map[string]any); ok {
		totalRemaining = parseFloat(summary["total_remaining_amount"])
		freeRemaining = parseFloat(summary["free_remaining_amount"])
		purchasedRemaining = parseFloat(summary["purchased_remaining_amount"])
	} else {
		totalRemaining = parseFloat(coreMap["opcredit_balance"])
		if totalRemaining == 0 {
			totalRemaining = parseFloat(coreMap["total_remains_credit"])
		}
	}

	summaryMetrics := []pluginapi.QuotaMetric{
		{
			Key:   "remaining",
			Label: "总剩余额度",
			Value: totalRemaining,
			Unit:  "点",
		},
	}
	if freeRemaining > 0 {
		summaryMetrics = append(summaryMetrics, pluginapi.QuotaMetric{
			Key:   "free",
			Label: "免费额度",
			Value: freeRemaining,
			Unit:  "点",
		})
	}
	if purchasedRemaining > 0 {
		summaryMetrics = append(summaryMetrics, pluginapi.QuotaMetric{
			Key:   "purchased",
			Label: "付费额度",
			Value: purchasedRemaining,
			Unit:  "点",
		})
	}

	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     planName,
			TierName: planName,
			TierID:   "minimax",
		},
		Summary: summaryMetrics,
	}, nil
}

func parseFloat(val any) float64 {
	if val == nil {
		return 0
	}
	switch v := val.(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	}
	return 0
}
