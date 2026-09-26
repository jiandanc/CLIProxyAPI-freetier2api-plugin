package cb

// 本文件实现账号级接口：余额查询、签到、注册激活与试用领取。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Balance 是账号的资源余额。
type Balance struct {
	// Remain 是剩余积分。
	Remain int64
	// Total 是本期总额度。
	Total int64
	// ExpiringSoon 是即将过期的积分（窗口由调用方定义）。
	ExpiringSoon int64
	// Packages 是套餐明细。
	Packages []BalancePackage
}

// BalancePackage 是单项资源包的余额。
type BalancePackage struct {
	// Name 是资源包名称。
	Name string
	// Remain 是剩余量。
	Remain int64
	// Total 是总量。
	Total int64
	// ExpireAt 是过期时刻（Unix 秒，0 表示不过期）。
	ExpireAt int64
}

// billingResourceAccount 是余额响应里的单个套餐。
//
// 字段是**上游原样的 PascalCase**（上游该接口不使用 camelCase）。
type billingResourceAccount struct {
	PackageName         string `json:"PackageName"`
	CycleEndTime        string `json:"CycleEndTime"` // "2006-01-02 15:04:05"，空 = 无到期
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
}

// billingResourceResponse 是余额接口的响应结构。
//
// 双层信封：Response.Data.Accounts。请求体也必须带分页与产品码，
// 否则上游返回空列表（这一点早期实现漏了，导致余额恒为 0）。
type billingResourceResponse struct {
	Response struct {
		Data struct {
			Accounts []billingResourceAccount `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// packageEndLayout 是套餐到期时间的墙钟格式（UTC+8）。
const packageEndLayout = "2006-01-02 15:04:05"

// packageRemainUsed 聚合单个套餐的 remain/used/size。
//
// 取数规则（照抄上游的单一事实来源，勿改）：
//   - Cycle 期套餐优先（CycleCapacitySize > 0）：用 Cycle 三字段，
//     remain 先钳到 [0, size]；used 取 CycleUsed 与 size-remain 的较大者，
//     且当 used 更大时反推 remain = size - used；
//   - 否则回退 Capacity 三字段，used 为 0 但 size > remain 时按差值补 used。
func packageRemainUsed(account billingResourceAccount) (remain, used, size int64) {
	if account.CycleCapacitySize > 0 {
		remain = account.CycleCapacityRemain
		size = account.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if account.CycleCapacityUsed > used {
			used = account.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = account.CapacityRemain
	used = account.CapacityUsed
	size = account.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// FetchBalance 查询账号余额。
//
// global 域先试无 /v2 的路径，404 时回落带 /v2 的路径；CN 只有带 /v2 的。
// 这个差异来自上游两个部署的历史，不是笔误。
func (c *Client) FetchBalance(cred *Credential) (*Balance, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	endpoints := GetEndpoints(region)
	paths := []string{billingMeterPathV2}
	if region.IsGlobal() {
		paths = []string{billingMeterPath, billingMeterPathV2}
	}

	// 请求体是上游要求的固定形状：缺 ProductCode 或分页会拿到空列表。
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}

	var lastErr error
	for _, path := range paths {
		data, errCall := c.billingJSON(cred, endpoints.BillingBase+path, http.MethodPost, body)
		if errCall != nil {
			lastErr = errCall
			// 只在 404 时换路径：其他错误换路径也救不回来。
			if upstreamErr, okErr := errCall.(*Error); okErr && upstreamErr.Kind == KindNotFound {
				continue
			}
			return nil, errCall
		}
		var payload billingResourceResponse
		if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
			lastErr = fmt.Errorf("decode balance: %w", errUnmarshal)
			continue
		}
		return parseBalance(payload), nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("balance endpoint is unavailable")
}

// parseBalance 归一化余额响应。
//
// remain/total 是各套餐的**求和**（用户关心的是账号总量，不是单个套餐）。
func parseBalance(payload billingResourceResponse) *Balance {
	balance := &Balance{}
	for _, account := range payload.Response.Data.Accounts {
		remain, _, size := packageRemainUsed(account)
		balance.Remain += remain
		balance.Total += size
		balance.Packages = append(balance.Packages, BalancePackage{
			Name:     strings.TrimSpace(account.PackageName),
			Remain:   remain,
			Total:    size,
			ExpireAt: parsePackageExpiry(account.CycleEndTime),
		})
	}
	return balance
}

// parsePackageExpiry 解析套餐到期时刻（UTC+8 墙钟）；解析失败返回 0。
func parsePackageExpiry(raw string) int64 {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0
	}
	parsed, errParse := time.ParseInLocation(packageEndLayout, trimmed, cstZone)
	if errParse != nil {
		return 0
	}
	return parsed.Unix()
}

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	// Already 表示今天已经签过（幂等成功，不是错误）。
	Already bool
	// Credit / Energy 是本次签到所得。
	Credit int64
	Energy int64
	// Streak 是连续签到天数。
	Streak int64
}

// DailyCheckin 执行每日签到。
//
// 「今天已签到」被识别为幂等成功：上游对重复签到返回业务错误，
// 直接当失败会让自动签到任务每天报错一次。
func (c *Client) DailyCheckin(cred *Credential) (*CheckinResult, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	endpoints := GetEndpoints(region)
	paths := []string{dailyCheckinPathV2}
	if region.IsGlobal() {
		paths = []string{dailyCheckinPath, dailyCheckinPathV2}
	}

	var lastErr error
	for _, path := range paths {
		data, errCall := c.billingJSON(cred, endpoints.BillingBase+path, http.MethodPost, map[string]any{})
		if errCall != nil {
			if IsAlreadyCheckin(errCall) {
				return &CheckinResult{Already: true}, nil
			}
			lastErr = errCall
			if upstreamErr, okErr := errCall.(*Error); okErr && upstreamErr.Kind == KindNotFound {
				continue
			}
			return nil, errCall
		}
		var payload struct {
			Credit int64 `json:"credit"`
			Energy int64 `json:"energy"`
			Streak int64 `json:"streak"`
		}
		if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
			// 签到成功但响应结构不认识：仍算成功（副作用已发生）。
			return &CheckinResult{}, nil
		}
		return &CheckinResult{Credit: payload.Credit, Energy: payload.Energy, Streak: payload.Streak}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("checkin endpoint is unavailable")
}

// billingJSON 发起一次计费域请求并解开信封。
func (c *Client) billingJSON(cred *Credential, url, method string, body any) (json.RawMessage, error) {
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return nil, fmt.Errorf("encode request: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(c.opts.Context, method, url, strings.NewReader(string(encoded)))
	if errReq != nil {
		return nil, fmt.Errorf("build request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	return c.doJSON(req)
}

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

// FetchQuota 汇总账号的额度信息（供宿主的管理端展示）。
func (c *Client) FetchQuota(cred *Credential) (*Balance, error) {
	return c.FetchBalance(cred)
}

// 确保 io 包被使用（closeBody 的签名依赖它）。
var _ = io.Discard
