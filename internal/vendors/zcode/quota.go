package zcode

// 额度查询：billing/current（套餐）+ billing/balance（余量）。
//
// 只对 Coding Plan（JWT）账号有意义——API Key 是用户自带的按量付费 Key，
// 没有套餐窗口。参考 zcode2api/app/quota.py。

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// FetchQuota 查询 ZCode 账号的套餐与余量。
func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil {
		return nil, transientErr("zcode credential is nil")
	}

	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "ZCode Coding Plan",
			TierName: "Standard",
			TierID:   "zcode",
		},
	}

	if reason := billingBlockReason(cred); reason != "" {
		// 非 Coding Plan 账号：如实说明，不伪装成「查询成功但无数据」。
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key: "status", Label: "账号状态", Value: 1, Unit: reason,
		})
		return response, nil
	}

	fillSubscription(ctx, cred, response)
	metrics, errBalance := fetchBalances(ctx, cred)
	if errBalance != nil {
		return nil, errBalance
	}
	response.Summary = append(response.Summary, metrics...)
	return response, nil
}

// fillSubscription 用 billing/current 补充套餐名（失败时保留默认值）。
func fillSubscription(ctx context.Context, cred *Credential, response *pluginapi.QuotaFetchResponse) {
	code, body, errRequest := billingRequest(ctx, cred, "GET", PathBillingCurrent, nil)
	if errRequest != nil || code != codeSuccess {
		return
	}
	plans, _ := dataMap(body)["plans"].([]any)
	if len(plans) == 0 {
		return
	}
	first, ok := plans[0].(map[string]any)
	if !ok {
		return
	}
	if name := stringField(first, "name"); name != "" {
		response.Subscription.Plan = name
	}
	if planID := firstNonEmpty(stringField(first, "plan_id"), stringField(first, "planId")); planID != "" {
		response.Subscription.TierName = planID
	}
}

// fetchBalances 读取各模型余量并转成指标。
func fetchBalances(ctx context.Context, cred *Credential) ([]pluginapi.QuotaMetric, error) {
	code, body, errRequest := billingRequest(ctx, cred, "GET", PathBillingBalance, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	if code != codeSuccess {
		return nil, Classify(0, classifyBillingOutcome(code, body))
	}

	balances, _ := dataMap(body)["balances"].([]any)
	if len(balances) == 0 {
		return nil, nil
	}

	metrics := make([]pluginapi.QuotaMetric, 0, len(balances))
	for _, item := range balances {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := firstNonEmpty(stringField(entry, "show_name"), stringField(entry, "model"))
		if name == "" {
			continue
		}
		metrics = append(metrics, pluginapi.QuotaMetric{
			Key:   "remaining_" + sanitizeMetricKey(name),
			Label: name + " 剩余",
			Value: floatField(entry, "remaining_units"),
			Unit:  "tokens",
		})
	}
	return metrics, nil
}

// sanitizeMetricKey 把模型名转成稳定的指标键。
func sanitizeMetricKey(name string) string {
	replacer := strings.NewReplacer(" ", "_", ".", "_", "/", "_", "-", "_")
	return strings.ToLower(replacer.Replace(strings.TrimSpace(name)))
}

// floatField 宽松取浮点字段（JSON 数字一律 float64）。
func floatField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}
