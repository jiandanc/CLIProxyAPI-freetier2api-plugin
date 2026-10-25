package codearts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/hostbridge"
)

type ChatRequest struct {
	Credential *Credential
	Body       []byte
	Stream     bool
	// Benefit 走每日免费福利额度通道（请求头带 maas_type: benefit）。
	//
	// 本插件只服务免费福利通道：上游把每日免费额度与订阅积分分成两条通道，
	// 同一个模型名两边都有，区别就是这个头。要接积分通道时在这里分开即可。
	Benefit bool
}

// Chat 执行一次非流式对话。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	resp, errDo := doCodeArtsRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read codearts response: %w", errRead)
	}

	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}

	return body, nil
}

// ChatStream 执行一次流式对话。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	resp, errDo := doCodeArtsRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, Classify(resp.StatusCode, string(body))
	}

	return resp, nil
}

func doCodeArtsRequest(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	if req.Credential == nil {
		return nil, fmt.Errorf("codearts credential is nil")
	}

	target := strings.TrimRight(baseURL, "/")
	if target == "" {
		target = SnapEngineApiHost
	}

	endpoint := target + EpChatV2

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("create codearts http request: %w", errReq)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	httpReq.Header.Set("Agent-Type", "PromptCenter")
	httpReq.Header.Set("User-Agent", "CodeArtsAgent/5.2.0")
	if req.Benefit {
		// 必须放在签名之前：签名覆盖该头，晚设就不生效，
		// 请求会被判成积分通道并按积分计费。
		httpReq.Header.Set(HeaderMaasType, MaasBenefit)
	}

	// 签名
	signHuaweiRequest(httpReq, req.Body, req.Credential)

	timeout := 120 * time.Second
	if req.Stream {
		timeout = 300 * time.Second
	}

	client := hostbridge.Client(ctx, timeout)
	resp, errCall := client.Do(httpReq)
	if errCall != nil {
		return nil, fmt.Errorf("codearts request failed: %w", errCall)
	}

	return resp, nil
}

// signHuaweiRequest 为请求注入华为云 SDK-HMAC-SHA256 签名。
func signHuaweiRequest(req *http.Request, body []byte, cred *Credential) {
	if cred.AccessKeyID == "" || cred.SecretAccessKey == "" {
		return
	}

	xDate := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Sdk-Date", xDate)
	req.Header.Set("Host", req.URL.Host)
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
		req.Header.Set("X-Auth-Token", cred.SecurityToken)
	}

	payloadHash := sha256Hex(body)
	req.Header.Set("X-Sdk-Content-Sha256", payloadHash)

	canonicalHeadersStr, signedHeadersStr := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		canonicalQuery(req.URL.RawQuery),
		canonicalHeadersStr,
		signedHeadersStr,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"SDK-HMAC-SHA256",
		xDate,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signature := hmacHex(cred.SecretAccessKey, stringToSign)
	req.Header.Set("Authorization", "SDK-HMAC-SHA256 Access="+cred.AccessKeyID+
		", SignedHeaders="+signedHeadersStr+
		", Signature="+signature)
}

func canonicalHeaders(req *http.Request) (headers, signed string) {
	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		v := req.Header.Get(k)
		sb.WriteString(k + ":" + strings.TrimSpace(v) + "\n")
	}
	return sb.String(), strings.Join(keys, ";")
}

func canonicalURI(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	out := strings.Join(segments, "/")
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := vals[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacHex(key, msg string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// 常量与端点定义：华为云 CodeArts Agent 配置。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/upstream/const.go

const (
	VendorID   = "codearts"
	VendorName = "CodeArts"

	// 华为云 CodeArts API 网关
	SnapEngineApiHost = "https://snap-access.cn-north-4.myhuaweicloud.com"
	SnapManagerHost   = SnapEngineApiHost + "/snap-manager"
	PortalHost        = "https://codearts.huaweicloud.com/portal"
	// STSHost 临时 AK/SK 与令牌服务。**不是** snap-manager：把 /v1/oauth2/tokens
	// 打到 snap-manager 会得到 404 APIG.0101（实测），续期与授权码换 token 都在这里。
	STSHost     = "https://sts.cn-north-4.myhuaweicloud.com"
	BenefitHost = "https://opengw.developer.huaweicloud.com"

	ClientID = "codearts-agent"

	// 授权链接里的 plugin-name / plugin-version：门户按这两个头识别调用方，
	// 与官方 CodeArts Agent 插件保持一致（逆向自 huaweicloud.authentication）。
	PluginName    = "snap_AIIDE"
	PluginVersion = "5.2.0"

	// 本机回调：门户按授权链接里的 port 拼出 http://127.0.0.1:<port>/oauth/callback。
	//
	// 插件与浏览器不在同一台机器时收不到，只能让用户把这条链接粘回控制台。
	LocalCallbackPort = 7866
	LocalCallbackPath = "/oauth/callback"
	LocalCallbackURL  = "http://127.0.0.1:7866" + LocalCallbackPath

	// 端点路径
	EpChatV2        = "/api/v2/chat/completions"
	EpModelBuiltin  = "/v1/model/builtin"
	EpBenefitConfig = "/api/v1/gateway/config"
	EpBenefitClaim  = "/api/v1/benefit/claim"
	// EpBenefitGateway 报告福利通道是否开放（注意在 snap 主机上，不是 benefit 主机）。
	EpBenefitGateway = "/v1/benefit-gateway-config"
	// EpBenefitBalance 查询每日 / 每月福利 Token 额度。
	EpBenefitBalance = "/api/v1/user/tokens/balance"

	// HeaderMaasType 区分推理通道：带 benefit 走每日免费额度，不带则按订阅积分计费。
	HeaderMaasType = "maas_type"
	MaasBenefit    = "benefit"
	EpLoginTicket  = "/v1/login/ticket"
	EpOAuthTokens  = "/v1/oauth2/tokens"
)

// clampMaxTokens 把请求体里的 max_tokens / max_completion_tokens 夹到上游硬顶。
//
// 目录报出的上限可以夹，但客户端（如 auto-compact 续传）会自己带更大的值，
// 上游对 max_tokens 的取值范围做平台级预校验，超限整条请求回
// InferHub.001001005.400。这里只做最小改写：能解析成 JSON 且确实超限才回写，
// 其余情况原样放行，不碰请求体的其它字段。
func clampMaxTokens(body []byte) []byte {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	changed := false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		var value int
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		if value > maxOutputTokenCap {
			payload[key] = json.RawMessage(strconv.Itoa(maxOutputTokenCap))
			changed = true
		}
	}
	if !changed {
		return body
	}
	clamped, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return clamped
}
