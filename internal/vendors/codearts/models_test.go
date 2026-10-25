package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// stubHostHTTPDo 把宿主 HTTP 桥换成「记录请求 + 回空 JSON」的桩。
func stubHostHTTPDo(t *testing.T) **http.Request {
	t.Helper()
	return stubHostWithBody(t, `{}`)
}

// stubHostWithBody 与 stubHostHTTPDo 相同，但回指定的响应体。
func stubHostWithBody(t *testing.T, responseBody string) **http.Request {
	t.Helper()
	var captured *http.Request
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
		}
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		httpReq, errNew := http.NewRequest(req.Method, req.URL, nil)
		if errNew != nil {
			return nil, errNew
		}
		for key, values := range req.Headers {
			for _, value := range values {
				httpReq.Header.Add(key, value)
			}
		}
		captured = httpReq
		return json.Marshal(map[string]any{
			"StatusCode": 200,
			"Headers":    map[string][]string{"Content-Type": {"application/json"}},
			"Body":       []byte(responseBody),
		})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
	return &captured
}

// TestChatRequestSendsBenefitChannelHeader 锁定福利通道的开关。
//
// 上游用 maas_type 区分「每日免费额度」与「订阅积分」：带 benefit 才走免费额度。
// 这个头还必须**在签名之前**设置，否则签名覆盖不到它，等于没带。
func TestChatRequestSendsBenefitChannelHeader(t *testing.T) {
	captured := stubHostHTTPDo(t)

	_, errChat := Chat(context.Background(), "", ChatRequest{
		Credential: &Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "ST"},
		Body:       []byte(`{"model":"glm-5.3-flash"}`),
		Benefit:    true,
	})
	if errChat != nil {
		t.Fatalf("chat: %v", errChat)
	}
	if *captured == nil {
		t.Fatal("没有截到请求")
	}
	if got := (*captured).Header.Get(HeaderMaasType); got != MaasBenefit {
		t.Fatalf("%s = %q, want %q", HeaderMaasType, got, MaasBenefit)
	}
	if signed := (*captured).Header.Get("Authorization"); !strings.Contains(signed, "maas_type") {
		t.Fatalf("Authorization 未覆盖 %s: %s", HeaderMaasType, signed)
	}
}

// TestChatRequestWithoutBenefitChannel 守住反向：显式走积分通道时不带这个头。
func TestChatRequestWithoutBenefitChannel(t *testing.T) {
	captured := stubHostHTTPDo(t)

	_, errChat := Chat(context.Background(), "", ChatRequest{
		Credential: &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"},
		Body:       []byte(`{"model":"glm-5.2"}`),
	})
	if errChat != nil {
		t.Fatalf("chat: %v", errChat)
	}
	if *captured != nil && (*captured).Header.Get(HeaderMaasType) != "" {
		t.Fatalf("积分通道不应带 %s，实际 %q", HeaderMaasType, (*captured).Header.Get(HeaderMaasType))
	}
}

// TestBuildAuthorizeURLCarriesCallbackPort 锁定授权链接的 port 参数。
//
// 门户按 port 拼回调地址；少了它就回落到 80 端口，浏览器停在打不开的
// 127.0.0.1 地址上，插件则永远等不到回调。
func TestBuildAuthorizeURLCarriesCallbackPort(t *testing.T) {
	url := buildAuthorizeURL("ticket-1", "challenge-1")
	for _, want := range []string{
		"port=" + strconv.Itoa(LocalCallbackPort),
		"client_id=" + ClientID,
		"ticket_id=ticket-1",
		"code_challenge_method=SHA-256",
		"plugin-name=" + PluginName,
	} {
		if !strings.Contains(url, want) {
			t.Fatalf("授权链接缺少 %s: %s", want, url)
		}
	}
	// 票据密钥不能进授权链接：它会出现在浏览器历史与日志里。
	if strings.Contains(url, "secret") {
		t.Fatalf("授权链接不应携带 secret: %s", url)
	}
}

// TestRequestTokenUsesSTSHost 锁定令牌端点主机。
//
// /v1/oauth2/tokens 只在 sts 上，snap-manager 上没有这个 API（实测 404 APIG.0101）。
func TestRequestTokenUsesSTSHost(t *testing.T) {
	if !strings.Contains(STSHost, "sts.cn-north-4.myhuaweicloud.com") {
		t.Fatalf("STSHost = %q", STSHost)
	}
	if strings.Contains(STSHost, "snap-manager") {
		t.Fatalf("令牌端点不应指向 snap-manager: %q", STSHost)
	}
}

// TestFetchQuotaTreatsMissingBenefitAsZeroNotError 锁定「账号没有福利权益」的呈现方式。
//
// 上游对这种账号回 4004 benefit not found（实测：带签名、带各种身份头都一样），
// 同一账号的福利模型清单正常、签到也返回成功——所以这是账号状态，不是查询失败。
// 当成错误会让控制台整列显示「-- / --」，用户看不出到底是没有额度还是查询坏了。
func TestFetchQuotaTreatsMissingBenefitAsZeroNotError(t *testing.T) {
	stubHostWithBody(t, `{"error_code":"4004","error_msg":"benefit not found","result":null}`)

	quota, errQuota := FetchQuota(context.Background(), &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"})
	if errQuota != nil {
		t.Fatalf("没有福利权益不该报错: %v", errQuota)
	}
	if quota == nil || quota.Subscription == nil {
		t.Fatal("额度结果为空")
	}
	if len(quota.Summary) == 0 || quota.Summary[0].Key != "total" {
		t.Fatalf("未开通也要给出 total 键，否则控制台折算不出总额: %+v", quota.Summary)
	}
	if !strings.Contains(quota.Summary[0].Label, "未开通") {
		t.Fatalf("标签要说明原因: %q", quota.Summary[0].Label)
	}
}

// TestFetchQuotaUsesStandardRemainingKeys 锁定标准键。
//
// 控制台的「剩余 / 总额」只认 remaining / total / used（internal/auth.Totals），
// 用自定义键会让这两列永远是 0 / 0。
func TestFetchQuotaUsesStandardRemainingKeys(t *testing.T) {
	stubHostWithBody(t, `{"error_code":"0000","result":{"channel":"benefit",`+
		`"daily_token_limit":10000000,"daily_tokens_used":2500000,`+
		`"monthly_token_limit":100000000,"monthly_tokens_used":40000000}}`)

	quota, errQuota := FetchQuota(context.Background(), &Credential{AccessKeyID: "AK", SecretAccessKey: "SK"})
	if errQuota != nil {
		t.Fatalf("fetch quota: %v", errQuota)
	}
	values := map[string]float64{}
	for _, metric := range quota.Summary {
		values[metric.Key] = metric.Value
	}
	if values["total"] != 10000000 {
		t.Fatalf("total = %v, want 10000000", values["total"])
	}
	// 剩余必须由「每日额度 - 今日已用」算出来，而不是回传上游没有的字段。
	if values["remaining"] != 7500000 {
		t.Fatalf("remaining = %v, want 7500000", values["remaining"])
	}
	if values["used"] != 2500000 {
		t.Fatalf("used = %v, want 2500000", values["used"])
	}
}

// TestParseBuiltinModelsReadsBuiltinEnvelope 锁定内置目录的真实信封。
//
// 上游响应是 {"count":N,"builtinModels":[...]}，早期实现只认顶层 models[]，
// 于是每一轮都解析失败、静默回落内置清单——控制台长期显示 4 条写死的旧模型，
// 上下文 131072/8192 与真实值不符、图片能力与倍率全丢。
func TestParseBuiltinModelsReadsBuiltinEnvelope(t *testing.T) {
	body := `{"count":3,"builtinModels":[
	  {"model_id":"GLM-5.2","model_name":"GLM-5.2","model_desc":"Latest Flagship: Long-horizon tasks & complex projects","enable":true,"supports_images":false,
	   "credit":[{"input_from":0,"status":"0","ratio_display":"0.7x"},{"input_from":32001,"status":"0","ratio_display":"0.7x"},{"input_from":-1,"status":"0","ratio_display":"0.7x"}]},
	  {"model_id":"kimi-k2.6-vl","model_name":"Kimi-K2.6-VL","model_desc":"Outstanding programming performance","supports_images":true,"credit":[]},
	  {"model_id":"retired-model","model_name":"Retired","enable":false,"supports_images":false}]}`

	models, errParse := parseBuiltinModels([]byte(body))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if len(models) != 2 {
		t.Fatalf("enable=false 的模型应被跳过，实际 %d 条: %+v", len(models), models)
	}

	glm := models[0]
	if glm.ID != "GLM-5.2" || glm.Name != "GLM-5.2" {
		t.Fatalf("ID/Name 解析错: %+v", glm)
	}
	if glm.Credits != "0.7x" {
		t.Fatalf("倍率应取基础档 ratio_display，实际 %q", glm.Credits)
	}
	// 内置目录不带上限，由兜底清单补位；输出上限必须夹到上游硬顶。
	if glm.ContextWindow != 202752 {
		t.Fatalf("ContextWindow = %d, want 202752", glm.ContextWindow)
	}
	if glm.MaxTokens != DefaultMaxOutputTokens {
		t.Fatalf("MaxTokens = %d, want %d", glm.MaxTokens, DefaultMaxOutputTokens)
	}
	if !glm.SupportsTools {
		t.Fatal("CodeArts 模型都支持工具调用")
	}

	vl := models[1]
	if !vl.SupportsImages {
		t.Fatal("supports_images=true 的模型必须报图片能力")
	}
	if vl.Credits != "" {
		t.Fatalf("没有 credit 的模型倍率应为空，实际 %q", vl.Credits)
	}
	// 不在兜底清单里的模型不编造上下文，留给 core 层兜底。
	if vl.ContextWindow != 0 || vl.MaxTokens != 0 {
		t.Fatalf("未知上限应为 0，实际 %d/%d", vl.ContextWindow, vl.MaxTokens)
	}
}

// TestParseBuiltinModelsAcceptsLegacyModelsKey 守住兼容分支。
func TestParseBuiltinModelsAcceptsLegacyModelsKey(t *testing.T) {
	models, errParse := parseBuiltinModels([]byte(`{"models":[{"model_name":"glm-5.2"}]}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if len(models) != 1 || models[0].ID != "glm-5.2" {
		t.Fatalf("旧信封未兼容: %+v", models)
	}
}

// TestBaseCreditDisplayPicksBaseTier 锁定倍率取档规则。
//
// 上游按输入长度给多条档位（基础档 input_from=0），控制台只有一格，取基础档；
// status 显式非 "0" 的档视为未生效，跳过。ratio 与 ratio_display 不是一个量纲，
// 必须原样透传，不能换算。
func TestBaseCreditDisplayPicksBaseTier(t *testing.T) {
	zero, high := 0, 32001
	cases := []struct {
		name  string
		tiers []creditTier
		want  string
	}{
		{"取基础档", []creditTier{{InputFrom: &high, RatioDisplay: "1.0x"}, {InputFrom: &zero, RatioDisplay: "0.7x"}}, "0.7x"},
		{"跳过未生效档", []creditTier{{InputFrom: &zero, Status: "1", RatioDisplay: "9x"}, {InputFrom: &high, RatioDisplay: "0.7x"}}, "0.7x"},
		{"缺 input_from 当基础档", []creditTier{{RatioDisplay: "0.32x"}}, "0.32x"},
		{"空档位返回空", nil, ""},
	}
	for _, tc := range cases {
		if got := baseCreditDisplay(tc.tiers); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCapOutputTokensClampsToUpstreamHardLimit 锁定输出上限的硬顶。
//
// 实测上游预校验：max_tokens=65536 放行，65537 起整条请求 400
// （InferHub.001001005.400）。目录自述 131072/393216，直接照抄会让客户端
// 用超出上限的 max_tokens 把请求打到整条失败。
func TestCapOutputTokensClampsToUpstreamHardLimit(t *testing.T) {
	if got := capOutputTokens(65537); got != DefaultMaxOutputTokens {
		t.Fatalf("65537 应夹到 %d，实际 %d", DefaultMaxOutputTokens, got)
	}
	if got := capOutputTokens(393216); got != DefaultMaxOutputTokens {
		t.Fatalf("393216 应夹到 %d，实际 %d", DefaultMaxOutputTokens, got)
	}
	if got := capOutputTokens(8192); got != 8192 {
		t.Fatalf("未超限应原样保留，实际 %d", got)
	}
	if got := capOutputTokens(0); got != 0 {
		t.Fatalf("未知上限保持 0，实际 %d", got)
	}
}

// TestAdapterModelCredits 锁定倍率表与清单同源。
//
// 倍率与清单必须出自同一轮探测；没有倍率的模型不进表，控制台显示「—」，
// 不能把「上游没给」画成「免费」。
func TestAdapterModelCredits(t *testing.T) {
	v := &adapter{}
	infos := v.modelsToPluginAPI([]Model{
		{ID: "GLM-5.2", Name: "GLM-5.2", ContextWindow: 202752, MaxTokens: 65536, SupportsTools: true, Credits: "0.7x"},
		{ID: "kimi-k2.6-vl", Name: "Kimi-K2.6-VL", SupportsTools: true, SupportsImages: true},
	})
	if len(infos) != 2 {
		t.Fatalf("转换条数不对: %d", len(infos))
	}
	if !core.ModelSupportsImages(infos[1]) {
		t.Fatal("图片能力没有透传到宿主契约")
	}

	credits := v.ModelCredits()
	if got := credits["GLM-5.2"]; got != "0.70X" {
		t.Fatalf("倍率应归一化为 0.70X，实际 %q", got)
	}
	if _, exists := credits["kimi-k2.6-vl"]; exists {
		t.Fatal("没有倍率的模型不应进表")
	}
}
