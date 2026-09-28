package zcode

// 签到相关实现：实现 ZCode (Z.AI) Coding Plan 套餐领取与每日签到。
// 参考：D:\Workspace\zcode2api\app\claim.py

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"freetier2api-plugin/internal/httpx"
)

// SupportsCheckin 报告本供应商是否支持签到（ZCode 支持领取每日 Coding Plan）。
func SupportsCheckin() bool {
	return true
}

type CheckinOutcome struct {
	Already bool
	Credit  int64
	Message string
}

// Checkin 执行 Coding Plan 套餐领取。
func Checkin(ctx context.Context, cred *Credential) (*CheckinOutcome, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	if cred.JWTToken == "" && cred.APIKey == "" {
		return nil, fmt.Errorf("zcode credential missing token")
	}

	client := httpx.Client(ctx, 30*time.Second)

	// 1. 上报激活事件（模拟桌面端日活信号）
	_ = reportActivation(ctx, client, cred)

	// 2. 查询可领取的套餐
	plans, errPreview := previewPlans(ctx, client, cred)
	if errPreview != nil {
		return nil, errPreview
	}
	if len(plans) == 0 {
		return &CheckinOutcome{
			Already: true,
			Message: "今日无可领取的 Coding Plan 套餐活动",
		}, nil
	}

	// 选优先级最高的套餐
	bestPlan := plans[0]

	// 3. 提交领取
	outcome, errClaim := claimPlan(ctx, client, cred, bestPlan.PlanID, bestPlan.Name)
	if errClaim != nil {
		return nil, errClaim
	}
	return outcome, nil
}

type PlanInfo struct {
	PlanID   string
	Name     string
	Priority int
}

func previewPlans(ctx context.Context, client *http.Client, cred *Credential) ([]PlanInfo, error) {
	url := fmt.Sprintf("%s%s?app_version=%s&platform=darwin-arm64", DefaultZCodeOrigin, PathBillingPreview, ClientAppVersion)
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return nil, errReq
	}
	applyBillingHeaders(req, cred)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("preview request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}

	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Plans []struct {
				PlanID   string `json:"plan_id"`
				Name     string `json:"name"`
				Priority int    `json:"priority"`
			} `json:"plans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("decode preview response: %w", err)
	}
	if data.Code != 0 {
		return nil, fmt.Errorf("preview error: code=%d msg=%s", data.Code, data.Msg)
	}

	var plans []PlanInfo
	for _, p := range data.Data.Plans {
		if strings.TrimSpace(p.PlanID) == "" {
			continue
		}
		plans = append(plans, PlanInfo{
			PlanID:   p.PlanID,
			Name:     p.Name,
			Priority: p.Priority,
		})
	}

	sort.Slice(plans, func(i, j int) bool {
		return plans[i].Priority > plans[j].Priority
	})

	return plans, nil
}

func claimPlan(ctx context.Context, client *http.Client, cred *Credential, planID, planName string) (*CheckinOutcome, error) {
	reqBody, _ := json.Marshal(map[string]string{"plan_id": planID})
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, DefaultZCodeOrigin+PathBillingClaim, bytes.NewReader(reqBody))
	if errReq != nil {
		return nil, errReq
	}
	applyBillingHeaders(req, cred)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("claim request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, errRead
	}

	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(body, &res)

	switch res.Code {
	case 0:
		name := planName
		if name == "" {
			name = planID
		}
		return &CheckinOutcome{
			Already: false,
			Message: fmt.Sprintf("签到成功：已领取套餐「%s」", name),
		}, nil
	case 1003:
		return &CheckinOutcome{
			Already: true,
			Message: "今天已经签到过了（该套餐已领取过）",
		}, nil
	case 1005:
		return &CheckinOutcome{
			Already: true,
			Message: "今日领取名额已用完",
		}, nil
	case 1002:
		return &CheckinOutcome{
			Already: true,
			Message: "活动暂未开放或已结束",
		}, nil
	default:
		msg := res.Msg
		if msg == "" {
			msg = string(body)
		}
		return nil, fmt.Errorf("领取失败 (%d): %s", res.Code, msg)
	}
}

func reportActivation(ctx context.Context, client *http.Client, cred *Credential) error {
	userID := extractUserIDFromJWT(cred.JWTToken)
	if userID == "" {
		return nil
	}

	events := []string{"app_launch", "app_daily_active"}
	for _, elem := range events {
		body, _ := json.Marshal(map[string]any{
			"element":           elem,
			"user_id":           userID,
			"screen_resolution": "2560x1440",
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, DefaultZCodeOrigin+PathEventReport, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", ClientUA)
			resp, errDo := client.Do(req)
			if errDo == nil {
				_ = resp.Body.Close()
			}
		}
	}
	return nil
}

func applyBillingHeaders(req *http.Request, cred *Credential) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ClientUA)
	req.Header.Set("HTTP-Referer", DefaultZCodeOrigin)
	req.Header.Set("X-Title", "Z Code@electron")
	req.Header.Set("X-ZCode-App-Version", ClientAppVersion)
	req.Header.Set("X-Platform", "darwin-arm64")
	req.Header.Set("X-Release-Channel", "stable")
	req.Header.Set("X-Client-Language", "zh-CN")
	req.Header.Set("X-Client-Timezone", "Asia/Shanghai")

	devMID := cred.DeviceMID
	if devMID == "" {
		devMID = "default-zcode-dev"
	}
	req.Header.Set("X-Device-Mid", devMID)
	req.Header.Set("x-request-id", randomUUID())

	if cred.JWTToken != "" {
		req.Header.Set("Authorization", "Bearer "+cred.JWTToken)
	} else if cred.APIKey != "" {
		req.Header.Set("x-api-key", cred.APIKey)
	}
}

func extractUserIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	seg := parts[1]
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	data, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		data, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	if uid, ok := payload["user_id"].(string); ok && uid != "" {
		return uid
	}
	if sub, ok := payload["sub"].(string); ok && sub != "" {
		return sub
	}
	return ""
}
