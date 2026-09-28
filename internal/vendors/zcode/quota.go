package zcode

// 额度查询：获取 ZCode 账号额度或余量信息。
// 参考：/Users/jiandan/Workspaces/zcode2api/app/quota.py

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/httpx"
)

var ErrQuotaUnsupported = errors.New("zcode 不支持该账号类型的额度查询")

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

	// 优先使用 JWT 方式查询 plan 余额
	if cred.JWTToken != "" {
		req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, DefaultZCodeOrigin+PathBillingBalance, nil)
		if errReq != nil {
			return nil, errReq
		}
		applyHeaders(req, cred)

		client := httpx.Client(ctx, 30*time.Second)
		resp, errDo := client.Do(req)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == http.StatusOK {
			var result struct {
				Data struct {
					Balance float64 `json:"balance"`
					Total   float64 `json:"total"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &result); err == nil {
				response.Summary = append(response.Summary,
					pluginapi.QuotaMetric{Key: "balance", Label: "剩余额度", Value: result.Data.Balance, Unit: "tokens"},
					pluginapi.QuotaMetric{Key: "total", Label: "总额度", Value: result.Data.Total, Unit: "tokens"},
				)
				return response, nil
			}
		}
	}

	response.Summary = append(response.Summary,
		pluginapi.QuotaMetric{Key: "status", Label: "账号状态", Value: 1, Unit: "正常"},
	)
	return response, nil
}
