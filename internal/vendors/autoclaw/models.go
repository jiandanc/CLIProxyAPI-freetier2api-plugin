package autoclaw

// 本文件实现 AutoClaw 的模型清单与账号信息获取。
//
// 清单来源（与 Agent2API 一致）：
//   - global：GET {base}/autoclaw-proxy/proxy/autoclaw-model-config（实时目录，
//     含 contextWindow / maxTokens / reasoning 等元数据）；
//   - cn：静态表（沙箱 relay 通道的模型，上游无对应实时目录接口可从
//     userapi 直接拉取；实时性以模型可用性实测为准）。
//
// 探测需要一把有效凭证；探测失败返回空清单而不是编造清单。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// modelsHTTPTimeout 是模型清单请求的上限。
const modelsHTTPTimeout = 30 * time.Second

// Model 是一条归一化后的模型条目。
type Model struct {
	ID              string
	Name            string
	Description     string
	ContextWindow   int64
	MaxOutputTokens int64
	IsReasoning     bool
	SupportsImages  bool
	SupportsTools   bool
	// Credits 是上游 creditConsumptionLevel 的档位文案（「低」/「中」/「高」）。
	//
	// 它是「这一档要扣多少点数」的文字档位，不是折扣倍数，**不能**换算成
	// 数字倍率；上游没给就是空串（控制台显示「—」，不臆造成免费）。
	Credits string
}

// modelConfigEntry 是 autoclaw-model-config 里的一条模型记录。
type modelConfigEntry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Tooltip       string   `json:"tooltip"`
	ContextWindow int64    `json:"contextWindow"`
	MaxTokens     int64    `json:"maxTokens"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input"`
	// CreditConsumptionLevel 是计费档位，实测是中文档位文案；也见过数字档位，
	// 因此用 RawMessage 收下再按形态解码，避免上游换形态时整份清单解析失败。
	CreditConsumptionLevel json.RawMessage `json:"creditConsumptionLevel"`
}

// modelConfigPayload 兼容上游两种响应形态：
//   - 旧信封：{"code":0,"msg":"SUCCESS","data":{"models":[...]}}；
//   - 实测国际版：顶层 {"models":[...]}（没有 code/msg/data 包装）。
//
// 只认后者会让前者之外的一切静默解析成空清单——宿主随后把「0 个模型」的
// 凭证丢掉，页面就整片看不到 AutoClaw 的模型。
type modelConfigPayload struct {
	Code   *int64             `json:"code"`
	Msg    string             `json:"msg"`
	Models []modelConfigEntry `json:"models"`
	Data   struct {
		Models []modelConfigEntry `json:"models"`
	} `json:"data"`
}

// entries 返回有效模型列表（顶层优先，回落信封）。
func (p modelConfigPayload) entries() []modelConfigEntry {
	if len(p.Models) > 0 {
		return p.Models
	}
	return p.Data.Models
}

// cnStaticModels 是国内版沙箱 relay 通道的模型表（Agent2API 2026-09 快照）。
var cnStaticModels = []string{
	"openclaw", "openclaw/default", "glm-5.3-flash", "glm-2", "zai_auto",
	"zai_auto-fast", "zai_glm-5.3-flash", "zaicoding_glm-5.3",
	"tdpsk_deepseek-v4-flash-202605", "tdpsk_deepseek-v4-pro-202606",
}

// FetchModels 拉取区域模型清单。
func FetchModels(ctx context.Context, baseURL string, token string, region Region) ([]Model, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("access token is required to list models")
	}
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is not configured")
	}
	if region.IsGlobal() {
		return fetchModelConfig(ctx, baseURL, token)
	}
	return staticCNModels(), nil
}

// fetchModelConfig 拉取国际版实时模型配置。
func fetchModelConfig(ctx context.Context, baseURL, token string) ([]Model, error) {
	endpoint := baseURL + ModelsConfigPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build model config request: %w", errReq)
	}
	ApplyModelsHeaders(req, token)

	resp, errDo := hostbridge.Client(ctx, modelsHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("fetch model config: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read model config response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, 0, string(body))
	}
	var payload modelConfigPayload
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("parse model config: %w", errUnmarshal)
	}
	if payload.Code != nil && *payload.Code != 0 {
		return nil, Classify(resp.StatusCode, *payload.Code, payload.Msg)
	}
	entries := payload.entries()
	if len(entries) == 0 {
		// 解析成功却一条模型都没有：多半是响应形态又变了。必须报错——返回
		// 空清单＋nil error 会让上传口只留下一行 "0 model(s)"，宿主再把该
		// 凭证当成没有模型可用而淘汰，问题就被掩盖成「模型列表是空的」。
		return nil, fmt.Errorf("model config returned 0 model(s)")
	}
	out := make([]Model, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		contextWindow := entry.ContextWindow
		if contextWindow == 0 {
			contextWindow = core.DefaultContextWindow(id)
		}
		maxTokens := entry.MaxTokens
		if maxTokens == 0 {
			maxTokens = core.DefaultMaxOutputTokens
		}
		out = append(out, Model{
			ID:              id,
			Name:            core.FirstNonEmpty(entry.Name, id),
			Description:     entry.Tooltip,
			ContextWindow:   contextWindow,
			MaxOutputTokens: maxTokens,
			IsReasoning:     entry.Reasoning,
			SupportsImages:  modelAcceptsImages(entry.Input),
			SupportsTools:   true,
			Credits:         levelText(entry.CreditConsumptionLevel),
		})
	}
	return out, nil
}

// levelText 把上游的计费档位解成展示文案。
//
// 实测是字符串（「低」/「中」/「高」）；数字是**档位编号**而不是倍率，
// 必须带上「档位」前缀，否则 core.FormatRate 会把 3 归一化成 "3.00X"，
// 在控制台变成一个看得见的伪倍率。对象等未知形态返回空串：宁可显示「—」。
func levelText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if errText := json.Unmarshal(raw, &text); errText == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if errNumber := json.Unmarshal(raw, &number); errNumber == nil {
		return "档位 " + number.String()
	}
	return ""
}

// modelAcceptsImages 报告上游声明的输入模态里是否含图片。
func modelAcceptsImages(input []string) bool {
	for _, modality := range input {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
}

// staticCNModels 返回国内版静态模型表。
func staticCNModels() []Model {
	out := make([]Model, 0, len(cnStaticModels))
	for _, id := range cnStaticModels {
		out = append(out, Model{
			ID:              id,
			Name:            id,
			Description:     "AutoClaw 国内版沙箱模型",
			ContextWindow:   core.DefaultContextWindow(id),
			MaxOutputTokens: core.DefaultMaxOutputTokens,
			SupportsTools:   true,
		})
	}
	return out
}

// RefreshTokenResult 是一次 token 刷新的结果。
type RefreshTokenResult struct {
	AccessToken  string
	RefreshToken string
}

// RefreshToken 用 refresh token 换新 access token
// （POST {base}/userapi/v1/refresh，body {source_id, device_id, refresh_token}）。
//
// refresh_token 为空表示上游未轮换，沿用旧值。
func RefreshToken(ctx context.Context, baseURL string, cred *Credential) (*RefreshTokenResult, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is not configured")
	}
	payload := map[string]any{
		"source_id":     protocolName,
		"device_id":     cred.DeviceIDValue(),
		"refresh_token": cred.RefreshTokenValue(),
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal refresh request: %w", errMarshal)
	}
	endpoint := baseURL + RefreshPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if errReq != nil {
		return nil, fmt.Errorf("build refresh request: %w", errReq)
	}
	ApplyUserAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, 30*time.Second).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "refresh: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read refresh response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	var envelope struct {
		Code *int64 `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, int64Value(envelope.Code), string(raw))
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return nil, Classify(resp.StatusCode, *envelope.Code, envelope.Msg)
	}
	if strings.TrimSpace(envelope.Data.AccessToken) == "" {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "refresh succeeded but no access_token", Kind: core.FailureKindSessionDead}
	}
	return &RefreshTokenResult{
		AccessToken:  envelope.Data.AccessToken,
		RefreshToken: envelope.Data.RefreshToken,
	}, nil
}

// WalletBalance 是积分钱包余额。
type WalletBalance struct {
	TotalBalance float64
}

// FetchBalance 查询积分余额（GET {base}/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw）。
func FetchBalance(ctx context.Context, baseURL string, token string) (*WalletBalance, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is not configured")
	}
	endpoint := baseURL + WalletsPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build wallets request: %w", errReq)
	}
	ApplyUserAPIHeaders(req, token)

	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "wallets: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read wallets response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	var envelope struct {
		Code *int64 `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			TotalBalance any `json:"total_balance"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	if envelope.Code != nil && *envelope.Code != 0 {
		return nil, Classify(resp.StatusCode, *envelope.Code, envelope.Msg)
	}
	return &WalletBalance{TotalBalance: float64(core.IntFromAny(envelope.Data.TotalBalance))}, nil
}

// int64Value 返回第一个非 nil 的 int64 指针值（全 nil 时 0）。
func int64Value(values ...*int64) int64 {
	for _, value := range values {
		if value != nil {
			return *value
		}
	}
	return 0
}
