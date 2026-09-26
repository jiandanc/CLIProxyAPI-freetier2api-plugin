package cb

// 本文件实现四套客户端指纹的行为上报。
//
// 同一个 /v2/report 端点，服务端按请求指纹把事件归属到不同任务族。
// body 恒为**数组**；事件必须带 userId，缺失时服务端 200 但静默丢弃。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ReportDesktopEvents 用桌面指纹族上报事件（对话域 /v2/report）。
func (c *Client) ReportDesktopEvents(ctx context.Context, cred *Credential, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(cred.Realm())
	return c.postReport(ctx, cred, endpoints.ChatBase+reportPath, events, func(req *http.Request) {
		c.applyDesktopHeaders(req, cred)
	})
}

// ReportBillingEvents 用 CLI/billing 指纹族上报事件（计费域 /v2/report）。
func (c *Client) ReportBillingEvents(ctx context.Context, cred *Credential, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(cred.Realm())
	return c.postReport(ctx, cred, endpoints.BillingBase+reportPath, events, func(req *http.Request) {
		c.applyBillingHeaders(req, cred)
	})
}

// ReportWebEvents 用 web 指纹族上报事件（Web 域 /v2/report）。
func (c *Client) ReportWebEvents(ctx context.Context, cred *Credential, pageURL string, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(RegionCN)
	return c.postReport(ctx, cred, endpoints.WebBase+reportPath, events, func(req *http.Request) {
		applyWebHeaders(req, cred, pageURL)
	})
}

// ReportMPEvents 用小程序指纹族上报事件。
//
// 注意 base 用 CN 计费域硬编码而非按 realm 选：global 账号也没有小程序任务，
// 行为一致，跟随原项目实现。
func (c *Client) ReportMPEvents(ctx context.Context, cred *Credential, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(RegionCN)
	return c.postReport(ctx, cred, endpoints.BillingBase+reportPath, events, func(req *http.Request) {
		c.applyBillingHeaders(req, cred)
		applyMPHeaders(req)
	})
}

// postReport 是所有指纹族共用的上报出口。
func (c *Client) postReport(ctx context.Context, cred *Credential, url string, events []map[string]any, decorate func(*http.Request)) error {
	// body 必须是数组：上游按数组解析，单对象会被静默丢弃。
	encoded, errEncode := json.Marshal(events)
	if errEncode != nil {
		return fmt.Errorf("encode report events: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(encoded)))
	if errReq != nil {
		return fmt.Errorf("build report request: %w", errReq)
	}
	decorate(req)

	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return fmt.Errorf("report request: %w", errDo)
	}
	defer closeBody(resp.Body)

	body := make([]byte, 0, 1024)
	buffer := make([]byte, 1024)
	for {
		read, errRead := resp.Body.Read(buffer)
		if read > 0 {
			body = append(body, buffer[:read]...)
			if len(body) > apiBodyLimit {
				break
			}
		}
		if errRead != nil {
			break
		}
	}
	if resp.StatusCode >= 400 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	}
	// 上报接口对「已上报过」也返回成功，因此不检查业务码。
	return nil
}

// ReportChatActivity 上报一条对话活跃事件（CLI/billing 指纹）。
//
// 这一条上报同时点亮 growth 连登与解锁 first_buddy 任务，
// 因此是领养与连登体系的前置条件。
func (c *Client) ReportChatActivity(ctx context.Context, cred *Credential, conversationID, requestID, modelID, modelName string) error {
	if strings.TrimSpace(requestID) == "" {
		requestID = conversationID
	}
	if strings.TrimSpace(modelID) == "" {
		modelID = defaultReportModelID
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = defaultReportModelName
	}
	now := nowMs()
	event := map[string]any{
		"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
		"mode": "craft", "conversationId": conversationID, "requestId": requestID,
		"inputLength":      12,
		"requestModelId":   modelID,
		"requestModelName": modelName,
		"isPlan":           false, "isAutoExecuteTerminal": false, "isAutoModify": false,
		"codebaseEnable": false,
		"maxToken":       0, "maxSteps": 0, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": "", "knowledgeName": "",
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"expertId": "", "recommendId": "", "skillId": "", "skillCount": 0,
		"totalCount": 0, "fileUri": "", "presentAt": now,
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID,
		"agentName":            "default", "agentType": "conversation",
		// userId 缺失会让服务端静默丢弃这条事件。
		"userId": cred.UIDValue(),
	}
	return c.ReportBillingEvents(ctx, cred, []map[string]any{event})
}

// 上报默认模型（上游对活跃上报不强校验模型，用通用值即可）。
const (
	defaultReportModelID   = "deepseek-v4-flash"
	defaultReportModelName = "DeepSeek V4 Flash"
)

// ReportWebElementClick 上报一次 web 元素点击（Library_read 等任务）。
func (c *Client) ReportWebElementClick(ctx context.Context, cred *Credential, pageURL, elementID, elementName string) error {
	now := nowMs()
	event := map[string]any{
		"eventCode": "web_element_click", "timestamp": now, "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": chromeUA,
		"machineId":    deriveID(cred.UIDValue(), "webmachine"),
		"userId":       cred.UIDValue(),
		"userNickname": cred.NicknameValue(),
		"enterpriseId": cred.EnterpriseIDValue(),
	}
	return c.ReportWebEvents(ctx, cred, pageURL, []map[string]any{event})
}

// nowMs 返回当前毫秒时间戳。
func nowMs() int64 { return time.Now().UnixMilli() }

// randomHexForExport 生成随机 hex（供包内构造事件 id 使用）。
func randomHexForExport(n int) string {
	buffer := make([]byte, n)
	if _, errRand := rand.Read(buffer); errRand != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("fallback-%d", time.Now().UnixNano())))
		return hex.EncodeToString(sum[:n])
	}
	return hex.EncodeToString(buffer)
}
