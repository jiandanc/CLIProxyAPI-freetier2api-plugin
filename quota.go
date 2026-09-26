package main

// 本文件实现 quota provider 能力：向宿主暴露账号额度。
//
// 宿主的管理端用这些数据渲染额度视图；插件只负责取数与归一，
// 不负责缓存（宿主自己会按需调用）。

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"workbuddy2api-plugin/cpasdk/pluginapi"
	"workbuddy2api-plugin/internal/cb"
	"workbuddy2api-plugin/internal/httpx"
)

// handleQuotaDescribe 描述本插件的额度能力。
func handleQuotaDescribe(request []byte) ([]byte, error) {
	return okEnvelope(pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{providerKey},
		DisplayName:        pluginDisplayName,
		// 本插件的「重置」是执行一次签到（签到会恢复额度），因此支持。
		SupportsReset: true,
	})
}

// handleQuotaFetch 查询某个账号的额度。
func handleQuotaFetch(request []byte) ([]byte, error) {
	var rpc pluginapi.QuotaFetchRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := httpx.WithCallbackID(context.Background(), "")
	// 宿主的额度路由**不发 StorageJSON**（只发 AuthIndex/AuthID + 属性），
	// 因此这里必须能按 index 回源，否则会误报「凭证缺失」。
	credential, errCredential := credentialForAuth(ctx, "", rpc.StorageJSON, firstNonEmptyString(rpc.AuthIndex, rpc.AuthID), rpc.Attributes)
	if errCredential != nil {
		return nil, newPluginError("workbuddy_credential_missing", errCredential.Error(), http.StatusUnauthorized)
	}

	client := newUpstreamClient(ctx)
	balance, errBalance := client.FetchBalance(credential)
	if errBalance != nil {
		return nil, errorToPluginError(errBalance)
	}
	return okEnvelope(buildQuotaResponse(credential, balance))
}

// handleQuotaReset 执行一次签到以恢复额度。
func handleQuotaReset(request []byte) ([]byte, error) {
	var rpc pluginapi.QuotaResetRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := httpx.WithCallbackID(context.Background(), "")
	credential, errCredential := credentialForAuth(ctx, "", rpc.StorageJSON, firstNonEmptyString(rpc.AuthIndex, rpc.AuthID), rpc.Attributes)
	if errCredential != nil {
		return nil, newPluginError("workbuddy_credential_missing", errCredential.Error(), http.StatusUnauthorized)
	}
	if !credential.Realm().IsGlobal() {
		return okEnvelope(pluginapi.QuotaResetResponse{
			Success: false,
			Message: "每日签到仅国内版账号提供；国际版账号无签到活动，额度按周期自动重置",
		})
	}

	client := newUpstreamClient(ctx)
	result, errCheckin := client.DailyCheckin(credential)
	if errCheckin != nil {
		return nil, errorToPluginError(errCheckin)
	}
	message := fmt.Sprintf("签到成功：+%d 积分 +%d 能量", result.Credit, result.Energy)
	if result.Already {
		message = "今天已经签到过了（幂等，无新增）"
	}
	return okEnvelope(pluginapi.QuotaResetResponse{Success: true, Message: message})
}

// buildQuotaResponse 把账号额度转成宿主的归一化额度结构。
//
// 结构层次：Summary 放关键数字（宿主的额度卡片直接读它），
// Groups/Buckets 放明细（展开后看）。
func buildQuotaResponse(credential *cb.Credential, balance *cb.Balance) pluginapi.QuotaFetchResponse {
	region := credential.Realm()
	response := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     string(region),
			TierName: realmDisplayName(region),
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "剩余积分", Value: float64(balance.Remain), Format: "number"},
			{Key: "credit_total", Label: "总额度", Value: float64(balance.Total), Format: "number"},
		},
	}
	if balance.Total > 0 {
		used := balance.Total - balance.Remain
		if used < 0 {
			used = 0
		}
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "credit_used", Label: "已用积分", Value: float64(used), Format: "number"})
	}
	if len(balance.Packages) > 0 {
		buckets := make([]pluginapi.QuotaBucket, 0, len(balance.Packages))
		for _, pkg := range balance.Packages {
			fraction := 0.0
			if pkg.Total > 0 {
				fraction = float64(pkg.Remain) / float64(pkg.Total)
			}
			bucket := pluginapi.QuotaBucket{
				Window:            pkg.Name,
				RemainingFraction: fraction,
				Description:       fmt.Sprintf("%d / %d", pkg.Remain, pkg.Total),
			}
			if pkg.ExpireAt > 0 {
				bucket.ResetTime = formatUnixTime(pkg.ExpireAt)
			}
			buckets = append(buckets, bucket)
		}
		response.Groups = []pluginapi.QuotaGroup{{
			DisplayName: "资源包明细",
			Buckets:     buckets,
		}}
	}
	return response
}

// realmDisplayName 返回域的展示名。
func realmDisplayName(region cb.Region) string {
	if region.IsGlobal() {
		return "国际版 (WorkBuddy AI)"
	}
	return "国内版 (CodeBuddy)"
}

// formatUnixTime 把 Unix 秒格式化为 RFC3339（无效值返回空串）。
func formatUnixTime(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}
