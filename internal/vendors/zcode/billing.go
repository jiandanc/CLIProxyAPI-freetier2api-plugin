package zcode

// 计费族（billing/*、usage、event/report）的公共请求层。
//
// 这些端点都在 zcode.z.ai 的 zcode-plan 之下，共用同一套身份头，且都**必须**
// 带 X-Device-Mid——缺失时上游返回 code=3001。鉴权用 Coding Plan JWT
// （API Key 通道没有套餐概念，不参与计费族）。
//
// 参考 zcode2api/app/quota.py 的 _auth_headers 与 app/claim.py 的 _billing_request。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/httpx"
)

const (
	// billingHTTPTimeout 是计费族单次请求的上限。
	billingHTTPTimeout = 20 * time.Second
	// billingMaxBody 是计费响应的读取上限。
	billingMaxBody = 4 << 20
)

// billingHeaders 构造计费族请求头。
//
// 平台/语言/时区/设备 ID 全部取账号档案：上游按设备聚类做风控，
// 全局伪装值会让所有账号聚成同一台机器。
func billingHeaders(cred *Credential) map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   UserAgent,
	}
	BuildIdentityHeaders(cred.Profile).Apply(headers)
	headers["Authorization"] = "Bearer " + cred.JWTToken
	headers["x-request-id"] = NewUUID()
	return headers
}

// billingRequest 发起一次计费族请求并解析业务码。
//
// 返回业务码与响应体（解析失败时业务码为 -1）。网络与鉴权失败统一返回
// 分类后的上游错误，调用方不必各自判断状态码。
func billingRequest(ctx context.Context, cred *Credential, method, path string, payload any) (int, map[string]any, error) {
	var bodyReader io.Reader
	if payload != nil {
		encoded, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return 0, nil, errMarshal
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, errNew := http.NewRequestWithContext(ctx, method, BillingBase+path, bodyReader)
	if errNew != nil {
		return 0, nil, errNew
	}
	for key, value := range billingHeaders(cred) {
		req.Header.Set(key, value)
	}

	resp, errDo := httpx.Client(ctx, billingHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, nil, transientErr("计费请求失败: " + errDo.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, billingMaxBody))
	if errRead != nil {
		return 0, nil, transientErr("读取计费响应失败: " + errRead.Error())
	}

	// 非 2xx 一律交给 Classify：它内部已经处理了「403 先判验证码挑战、再判凭证失效」
	// 的顺序（见 errors.go），此处不重复那套判定。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, nil, Classify(resp.StatusCode, string(raw))
	}

	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return -1, nil, nil
	}
	return businessCode(parsed), parsed, nil
}

// businessCode 取上游业务码；缺失或非数字返回 -1（视为失败）。
func businessCode(body map[string]any) int {
	if body == nil {
		return -1
	}
	switch v := body["code"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if out, errParse := strconv.Atoi(strings.TrimSpace(v)); errParse == nil {
			return out
		}
	}
	return -1
}

// dataMap 取响应体的 data 对象。
func dataMap(body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	data, _ := body["data"].(map[string]any)
	return data
}

// billingBlockReason 报告该凭证为何不能打计费族；可打时返回空串。
//
// 计费族只服务 Coding Plan（JWT）。纯 API Key 账号没有套餐，不该产生
// billing 流量——既拿不到数据，又会白增一条上游风控信号。
func billingBlockReason(cred *Credential) string {
	if cred == nil {
		return "凭证缺失"
	}
	if !cred.UsesPlanChannel() {
		return "该账号没有 Coding Plan 凭证（JWT），不参与套餐与额度查询"
	}
	return ""
}

// classifyBillingOutcome 把计费族的业务失败码翻成可读文案。
func classifyBillingOutcome(code int, body map[string]any) string {
	serverMsg := firstNonEmpty(stringField(body, "msg"), stringField(body, "message"))
	base := map[int]string{
		1001: "套餐不存在",
		1002: "活动已结束或套餐暂不可领取",
		1003: "该套餐已经领取过",
		1004: "不符合领取条件",
		1005: "今日领取名额已用完",
		3001: "请求参数不全（缺少设备标识）",
		3007: "验证码校验失败",
		401:  "请先登录后再领取",
	}[code]
	if base == "" {
		base = "上游返回业务错误"
	}
	if serverMsg != "" {
		return base + "（" + serverMsg + "）"
	}
	return base
}
