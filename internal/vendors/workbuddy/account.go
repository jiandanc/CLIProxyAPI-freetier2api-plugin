package workbuddy

// 本文件实现 WorkBuddy 国际版的账号激活与试用领取。
//
// 国际版账号首次使用前需要补注册地区并激活，否则部分接口返回
// 「region required」；激活后还能领一次性的试用加油包。
// 国内版没有这套流程（账号在注册时已激活）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// CompleteGlobalRegistration 完成 global 账号的注册激活（幂等）。
//
// global 账号首次使用前需要补地区并激活，否则部分接口返回「region required」。
// 失败不阻断调用方：多数情况下账号已经激活过。
func (c *Client) CompleteGlobalRegistration(cred *Credential) error {
	if cred == nil || !cred.Realm().IsGlobal() {
		return nil
	}
	activated, needsRegion, errStatus := c.globalRegisterStatus(cred)
	if errStatus != nil {
		return errStatus
	}
	if activated && !needsRegion {
		return nil
	}
	if needsRegion {
		countries, errCountries := c.globalCountries(cred)
		if errCountries != nil {
			return errCountries
		}
		if len(countries) == 0 {
			return fmt.Errorf("no allowed region available")
		}
		// 白名单首个（HK）是上游展示顺序里的首选。
		if errSubmit := c.globalSubmitRegion(cred, countries[0]); errSubmit != nil {
			return errSubmit
		}
	}
	// 提交后再验证一次，确认已激活。
	activated, _, errVerify := c.globalRegisterStatus(cred)
	if errVerify != nil {
		return errVerify
	}
	if !activated {
		return fmt.Errorf("global registration did not activate the account")
	}
	return nil
}

// globalRegisterStatus 查询 global 账号的激活状态。
func (c *Client) globalRegisterStatus(cred *Credential) (activated, needsRegion bool, err error) {
	url := GetEndpoints(RegionGlobal).BillingBase + globalRegisterPath + "?userId=" + cred.UIDValue()
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodGet, url, nil)
	if errReq != nil {
		return false, false, fmt.Errorf("build register status request: %w", errReq)
	}
	c.applyGlobalWebHeaders(req, cred)

	data, errCall := c.doJSON(req)
	if errCall != nil {
		// 上游用业务码而非 HTTP 状态表达状态，因此错误里也带着信息。
		message := strings.ToLower(errCall.Error())
		if strings.Contains(message, "region required") || strings.Contains(message, "code=500") {
			return false, true, nil
		}
		if strings.Contains(message, "code=200") {
			return true, false, nil
		}
		return false, false, errCall
	}
	// data 是双层信封（JSON 字符串里再包一层 JSON），需要二次解析。
	var status struct {
		Code int `json:"code"`
	}
	if errUnmarshal := json.Unmarshal(unwrapDoubleEncoded(data), &status); errUnmarshal == nil && status.Code == 200 {
		return true, false, nil
	}
	return false, false, nil
}

// globalCountries 拉取允许的注册地区列表。
func (c *Client) globalCountries(cred *Credential) ([]GlobalCountry, error) {
	body := map[string]any{"filterForbidden": 1}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return nil, errEncode
	}
	url := GetEndpoints(RegionGlobal).BillingBase + globalCountryCodePath
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost, url, strings.NewReader(string(encoded)))
	if errReq != nil {
		return nil, fmt.Errorf("build countries request: %w", errReq)
	}
	c.applyGlobalWebHeaders(req, cred)

	data, errCall := c.doJSON(req)
	if errCall != nil {
		return nil, errCall
	}
	var countries []GlobalCountry
	if errUnmarshal := json.Unmarshal(unwrapDoubleEncoded(data), &countries); errUnmarshal != nil {
		return nil, fmt.Errorf("decode countries: %w", errUnmarshal)
	}
	return countries, nil
}

// globalSubmitRegion 提交注册地区。
func (c *Client) globalSubmitRegion(cred *Credential, country GlobalCountry) error {
	body := map[string]any{
		"attributes": map[string]any{
			// 上游要求这三个值都是**单元素数组**，不是字符串。
			"countryCode":     []string{country.Code},
			"countryFullName": []string{country.EnName},
			"countryName":     []string{country.IOS2},
		},
	}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return errEncode
	}
	url := GetEndpoints(RegionGlobal).ChatBase + globalSubmitRegionPath
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost, url, strings.NewReader(string(encoded)))
	if errReq != nil {
		return fmt.Errorf("build submit region request: %w", errReq)
	}
	c.applyGlobalWebHeaders(req, cred)

	_, errCall := c.doJSON(req)
	return errCall
}

// GlobalCountry 是一个可选的注册地区。
type GlobalCountry struct {
	EnName string `json:"enName"`
	Name   string `json:"name"`
	IOS2   string `json:"ios2"`
	IOS3   string `json:"ios3"`
	Code   string `json:"code"`
}

// globalAllowedCountries 是国际版允许的注册地区白名单（顺序对齐上游展示）。
var globalAllowedCountries = []string{"HK", "MO", "SG", "TH", "PH", "MY", "ID"}

// IsAllowedCountry 报告地区代码是否在白名单内。
func IsAllowedCountry(code string) bool {
	for _, item := range globalAllowedCountries {
		if strings.EqualFold(item, strings.TrimSpace(code)) {
			return true
		}
	}
	return false
}

// applyGlobalWebHeaders 写入 global 注册链路使用的浏览器形态头。
func (c *Client) applyGlobalWebHeaders(req *http.Request, cred *Credential) {
	endpoints := GetEndpoints(RegionGlobal)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", endpoints.Origin)
	req.Header.Set("Referer", endpoints.Origin+"/")
	req.Header.Set("User-Agent", chromeUA)
	applyAuthHeaders(req, cred)
}

// unwrapDoubleEncoded 处理双层信封：data 可能是 JSON 字符串而非对象。
//
// 上游部分接口把 data 序列化成字符串再包进信封，因此需要二次解析。
func unwrapDoubleEncoded(data json.RawMessage) []byte {
	trimmed := strings.TrimSpace(string(data))
	if !strings.HasPrefix(trimmed, `"`) {
		return data
	}
	var inner string
	if errUnmarshal := json.Unmarshal(data, &inner); errUnmarshal != nil {
		return data
	}
	return []byte(inner)
}

// ClaimTrial 领取 global 账号的一次性试用加油包。
//
// 返回 claimed=false 表示已经领过（幂等码 14051），不是错误。
func (c *Client) ClaimTrial(cred *Credential) (bool, error) {
	if cred == nil {
		return false, fmt.Errorf("credential is required")
	}
	if !cred.Realm().IsGlobal() {
		// CN 账号没有这个活动。
		return false, nil
	}
	url := GetEndpoints(RegionGlobal).BillingBase + trialPath
	data, errCall := c.billingJSON(cred, url, http.MethodPost, map[string]any{})
	if errCall != nil {
		if isTrialAlreadyClaimed(errCall) {
			return false, nil
		}
		return false, errCall
	}
	_ = data
	return true, nil
}

// isTrialAlreadyClaimed 判断错误是否为「试用已领取」。
//
// 上游用两种形态表达：业务码形式（code=14051）与原始 JSON body
// （HTTP ≥400 时把 body 直接塞进消息）。
func isTrialAlreadyClaimed(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "code=14051") || strings.Contains(message, `"code":14051`)
}
