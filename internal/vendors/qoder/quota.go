package qoder

// 本文件实现 Qoder 的额度查询。
//
// 与 WorkBuddy 的积分余额不同，Qoder 的额度是「套餐用量桶」：上游返回
// userQuota / addOnQuota 两个对象，各有 used / total / remaining。
// 归一成宿主的 QuotaFetchResponse（Summary 放关键数字，Groups 放明细）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/vendors/qoder/bridge"
	"freetier2api-plugin/internal/vendors/qoder/cosy"
	"freetier2api-plugin/internal/vendors/qoder/qoderapi"
)

// quotaHTTPTimeout 是额度相关请求的上限。
const quotaHTTPTimeout = 30 * time.Second

// QuotaBucket 是一个额度桶。
type QuotaBucket struct {
	Used      float64
	Total     float64
	Remaining float64
	ResetTime string
}

// Quota 是一次额度查询结果。
type Quota struct {
	Plan            string
	UserQuota       *QuotaBucket
	AddonQuota      *QuotaBucket
	IsQuotaExceeded bool
	ExpiresAt       int64
}

// FetchQuota 查询账号额度。
func FetchQuota(ctx context.Context, cred *Credential) (*Quota, error) {
	bearer, errToken := quotaBearerToken(ctx, cred)
	if errToken != nil {
		return nil, errToken
	}
	endpoints := qoderapi.GetEndpoints(cred.Region)

	raw, errQuota := httpGetBearerJSON(ctx, endpoints.QuotaEndpoint, bearer)
	if errQuota != nil {
		return nil, errQuota
	}
	return &Quota{
		Plan:            fetchPlanTierName(ctx, endpoints, bearer),
		IsQuotaExceeded: raw["isQuotaExceeded"] == true,
		ExpiresAt:       int64(toFloat(raw, "expiresAt")),
		UserQuota:       extractQuotaBucket(raw, "userQuota"),
		AddonQuota:      extractQuotaBucket(raw, "addOnQuota"),
	}, nil
}

// quotaBearerToken 把凭证换成额度接口认识的 bearer token。
//
// 设备令牌（dt-…）可直接用；PAT（pt-…）必须先经 jobToken 交换拿到
// securityOauthToken——上游额度接口不认 PAT 本身。
func quotaBearerToken(ctx context.Context, cred *Credential) (string, error) {
	deviceToken, _ := bridge.ParseOAuthSecret(cred.Token)
	if strings.HasPrefix(deviceToken, "dt-") {
		return deviceToken, nil
	}
	seed := cosy.FingerprintSeed("", cred.Token)
	endpoints := qoderapi.GetEndpoints(cred.Region)
	jobToken, errExchange := cosy.ExchangeJobToken(ctx, cred.Token,
		cosy.DeriveMachineID(seed), cosy.DeriveMachineToken(seed), cosy.DeriveMachineType(seed),
		endpoints.JobTokenURL)
	if errExchange != nil {
		return "", classifyCredentialError(fmt.Errorf("exchange token: %w", errExchange))
	}
	oauthToken := bridge.StrVal(jobToken, "securityOauthToken")
	if oauthToken == "" {
		return "", NewError("qoder_credential_invalid", "jobToken 响应里没有 securityOauthToken", http.StatusUnauthorized)
	}
	return oauthToken, nil
}

// httpGetBearerJSON 发一个带 Bearer 的 GET 并解析成 JSON 对象。
func httpGetBearerJSON(ctx context.Context, endpoint, token string) (map[string]any, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, NewError("invalid_request", errRequest.Error(), http.StatusBadRequest)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, errDo := httpx.Client(ctx, quotaHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, NewError("qoder_upstream_transient", errDo.Error(), http.StatusBadGateway)
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, NewError("qoder_upstream_error", errRead.Error(), http.StatusBadGateway)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			status = http.StatusUnauthorized
		}
		return nil, NewError("qoder_upstream_error",
			fmt.Sprintf("额度接口返回 HTTP %d: %s", resp.StatusCode, truncateForMessage(string(body), 300)), status)
	}
	var result map[string]any
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		return nil, NewError("qoder_upstream_error", "额度接口返回的不是 JSON: "+errUnmarshal.Error(), http.StatusBadGateway)
	}
	return result, nil
}

// fetchPlanTierName 查询套餐名；失败不影响额度展示，只留空。
func fetchPlanTierName(ctx context.Context, endpoints qoderapi.Endpoints, token string) string {
	result, errPlan := httpGetBearerJSON(ctx, endpoints.PlanEndpoint, token)
	if errPlan != nil {
		return ""
	}
	if name, okName := result["plan_tier_name"].(string); okName {
		return name
	}
	return ""
}

// extractQuotaBucket 提取额度桶。
//
// 「桶不存在」的判据是**空对象**（上游对没有的桶返回 `{}`），不是「值全零」：
// Free 计划的 userQuota 就是 `{used:0, total:0, remaining:0}`，按全零吞掉会让
// 页面显示「上游未返回额度」，而真实语义是「有额度信息，只是余额为 0」。
func extractQuotaBucket(data map[string]any, key string) *QuotaBucket {
	obj, okObj := data[key].(map[string]any)
	if !okObj || len(obj) == 0 {
		return nil
	}
	bucket := &QuotaBucket{
		Used:      toFloat(obj, "used"),
		Total:     toFloat(obj, "total"),
		Remaining: toFloat(obj, "remaining"),
	}
	if reset, okReset := obj["resetTime"].(string); okReset {
		bucket.ResetTime = reset
	} else if reset, okReset := obj["reset_time"].(string); okReset {
		bucket.ResetTime = reset
	}
	return bucket
}

// BuildQuotaResponse 把上游额度翻译成宿主的归一化结构。
func BuildQuotaResponse(quota *Quota) pluginapi.QuotaFetchResponse {
	response := pluginapi.QuotaFetchResponse{}
	if quota.Plan != "" {
		response.Subscription = &pluginapi.QuotaSubscription{
			Plan: quota.Plan, TierName: quota.Plan, TierID: quota.Plan,
		}
	}
	if group := quotaGroup("套餐额度", quota.UserQuota); group != nil {
		response.Groups = append(response.Groups, *group)
	}
	if group := quotaGroup("个人拓展包", quota.AddonQuota); group != nil {
		response.Groups = append(response.Groups, *group)
	}
	if quota.UserQuota != nil {
		// total 必须一并输出：页面的「剩余/总额 + 使用率」要它才能算比例。
		// 上游对 Free 计划可能给 total=0（额度按剩余量计），此时仍输出 0——
		// 页面会显示 "—"，比编一个数诚实。
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "user_quota_remaining", Label: "套餐剩余额度", Value: quota.UserQuota.Remaining, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "user_quota_total", Label: "套餐总额度", Value: quota.UserQuota.Total, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "user_quota_used", Label: "套餐已用额度", Value: quota.UserQuota.Used, Unit: "credits"})
	}
	if quota.AddonQuota != nil {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "addon_quota_remaining", Label: "拓展包剩余额度", Value: quota.AddonQuota.Remaining, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "addon_quota_total", Label: "拓展包总额度", Value: quota.AddonQuota.Total, Unit: "credits"},
			pluginapi.QuotaMetric{Key: "addon_quota_used", Label: "拓展包已用额度", Value: quota.AddonQuota.Used, Unit: "credits"})
	}
	if quota.IsQuotaExceeded {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "quota_exceeded", Label: "额度状态", Value: 1, Unit: "已用尽"})
	}
	if quota.ExpiresAt > 0 {
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "expires_at", Label: "额度到期时间", Value: float64(quota.ExpiresAt), Format: "number", Unit: "unix"})
	}
	return response
}

// quotaGroup 把额度桶转成「剩余比例」分组。
func quotaGroup(displayName string, bucket *QuotaBucket) *pluginapi.QuotaGroup {
	if bucket == nil {
		return nil
	}
	fraction := 0.0
	switch {
	case bucket.Total > 0:
		fraction = clampFraction(bucket.Remaining / bucket.Total)
	case bucket.Remaining > 0:
		fraction = 1
	}
	description := fmt.Sprintf("已用 %.0f / 总计 %.0f，剩余 %.0f", bucket.Used, bucket.Total, bucket.Remaining)
	return &pluginapi.QuotaGroup{
		DisplayName: displayName,
		Buckets: []pluginapi.QuotaBucket{{
			Window:            "billing-cycle",
			RemainingFraction: fraction,
			ResetTime:         bucket.ResetTime,
			Description:       description,
		}},
	}
}

// clampFraction 把比例夹到 [0, 1]。
func clampFraction(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

// toFloat 把 JSON 里的数值字段读成 float64（兼容多种数字形态）。
func toFloat(m map[string]any, key string) float64 {
	switch value := m[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		parsed, _ := value.Float64()
		return parsed
	}
	return 0
}

// truncateForMessage 截断过长的上游响应，避免错误信息淹没日志。
func truncateForMessage(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "..."
}
