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

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/vendors/workbuddy"
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
//
// 取数交给凭证所属的供应商：各家的额度接口与归一方式完全不同
// （WorkBuddy 是积分余额，Qoder 是套餐用量桶）。
func handleQuotaFetch(request []byte) ([]byte, error) {
	var rpc pluginapi.QuotaFetchRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := httpx.WithCallbackID(context.Background(), "")
	// 宿主的额度路由**不发 StorageJSON**（只发 AuthIndex/AuthID + 属性），
	// 因此这里必须能按 index 回源，否则会误报「凭证缺失」。
	credential, vendor, errResolve := resolveVendorCredential(ctx, "", rpc.StorageJSON,
		firstNonEmptyString(rpc.AuthIndex, rpc.AuthID), rpc.Attributes)
	if errResolve != nil {
		return nil, errResolve
	}

	response, errQuota := vendor.Quota(ctx, credential)
	if errQuota != nil {
		return nil, errorToPluginError(errQuota)
	}
	return okEnvelope(response)
}

// handleQuotaReset 执行一次签到以恢复额度。
//
// 语义由供应商决定：WorkBuddy 的「重置」是签到；不支持签到的供应商
// （国际版）返回明确的不可用说明，而不是假装成功。
func handleQuotaReset(request []byte) ([]byte, error) {
	var rpc pluginapi.QuotaResetRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := httpx.WithCallbackID(context.Background(), "")
	credential, vendor, errResolve := resolveVendorCredential(ctx, "", rpc.StorageJSON,
		firstNonEmptyString(rpc.AuthIndex, rpc.AuthID), rpc.Attributes)
	if errResolve != nil {
		return nil, errResolve
	}
	if !vendor.SupportsCheckin() {
		return okEnvelope(pluginapi.QuotaResetResponse{
			Success: false,
			Message: "本供应商不提供签到；额度按上游周期自动重置",
		})
	}

	result, errCheckin := vendor.Checkin(ctx, credential)
	if errCheckin != nil {
		return nil, errorToPluginError(errCheckin)
	}
	if result == nil {
		return okEnvelope(pluginapi.QuotaResetResponse{Success: false, Message: "签到未返回结果"})
	}
	return okEnvelope(pluginapi.QuotaResetResponse{Success: true, Message: result.Message})
}

// buildQuotaResponse 把账号额度转成宿主的归一化额度结构。
//
// 结构层次：Summary 放关键数字（宿主的额度卡片直接读它），
// Groups/Buckets 放明细（展开后看）。
func buildQuotaResponse(credential *workbuddy.Credential, balance *workbuddy.Balance) pluginapi.QuotaFetchResponse {
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
func realmDisplayName(region workbuddy.Region) string {
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
