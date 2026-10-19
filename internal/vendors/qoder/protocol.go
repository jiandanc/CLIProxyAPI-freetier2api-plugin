package qoder

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// 本文件是 Qoder 的协议层：区域与端点表、专有协议引擎（Bridge 建链、
// 模型列举、CallQoderWithOpts）、排队信号与请求模板。

// baseprompt.json 是 Qoder 上游 agent_chat_generation 的请求模板（移植自 qoder2api）。
// 模板里保留了 {UUIDn}/{TIME1} 占位符：每次建 Bridge 时替换为实际值再解析成 JSON。
//
//go:embed assets/baseprompt.json
var basePromptRaw []byte

// TemplateBase 构造一次请求模板。
//
// 与 qoder2api 的差异：这里对解析失败返回错误而不是静默忽略。模板解析失败意味着
// 所有上游请求都会失真，静默继续会让问题表现成"模型答非所问"这种难以定位的现象。
func TemplateBase() (map[string]interface{}, error) {
	template := string(basePromptRaw)
	for _, placeholder := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
		template = strings.ReplaceAll(template, placeholder, NewUUID())
	}
	template = strings.ReplaceAll(template, "{TIME1}", fmt.Sprintf("%d", UnixMs()))

	var templateBase map[string]interface{}
	if errUnmarshal := json.Unmarshal([]byte(template), &templateBase); errUnmarshal != nil {
		return nil, fmt.Errorf("baseprompt.json 模板解析失败（内嵌资源损坏或占位符未替换）: %w", errUnmarshal)
	}
	if templateBase == nil {
		return nil, fmt.Errorf("baseprompt.json 模板为空")
	}
	return templateBase, nil
}

// BridgeSecret 返回要交给 NewBridge 的凭证串。
//
// 上游 NewBridge 内部会自己调 ParseOAuthSecret：传 dt- 明文会丢掉
// refresh_token（会话无法续期），所以有 refresh token 时必须按 qoder2api
// 导出的 JSON 形态传，与上游桌面端行为保持一致。
func BridgeSecret(cred *Credential) string {
	if cred == nil {
		return ""
	}
	if strings.TrimSpace(cred.RefreshToken) == "" {
		return cred.Token
	}
	payload, errMarshal := json.Marshal(map[string]string{
		"device_token":  cred.Token,
		"refresh_token": cred.RefreshToken,
	})
	if errMarshal != nil {
		return cred.Token
	}
	return string(payload)
}

// CredentialFingerprint 生成凭证指纹，用于 Bridge 缓存键。
//
// 不直接用 token 明文做缓存键：缓存键会进日志与内存 dump。
func CredentialFingerprint(cred *Credential) string {
	if cred == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(cred.Token + "|" + cred.RefreshToken + "|" + string(cred.Region)))
	return hex.EncodeToString(sum[:16])
}

func qoderChatStreamURL(region Region) string {
	return GetEndpoints(region).ChatStreamURL
}

func qoderModelListURL(region Region) string {
	return GetEndpoints(region).ModelListURL
}

func ParseOAuthSecret(secret string) (deviceToken, refreshToken string) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", ""
	}
	if strings.HasPrefix(secret, "dt-") || strings.HasPrefix(secret, "drt-") {
		return secret, ""
	}
	var payload struct {
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(secret), &payload); err != nil {
		return secret, ""
	}
	return payload.DeviceToken, payload.RefreshToken
}

func ResolveOAuthUserID(userInfo map[string]interface{}) string {
	if id := StrVal(userInfo, "id"); id != "" {
		return id
	}
	if id := StrVal(userInfo, "userId"); id != "" {
		return id
	}
	return StrVal(userInfo, "uid")
}

func FetchUserInfoWithToken(ctx context.Context, token string, region Region) (map[string]interface{}, error) {
	ep := GetEndpoints(region)
	req, err := http.NewRequestWithContext(ctx, "GET", ep.UserinfoBase, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	// DEBUG 用途排查原始 userinfo 响应结构：降为 Debug 级并截断至 500 字节，
	// 默认日志级别不再落盘用户名/组织等 PII，保留低级别排查能力
	logger.Debug("userinfo raw (%d bytes): %s", len(raw), core.Truncate(string(raw), 500))
	return result, nil
}

type Bridge struct {
	sess         *SessionContext
	client       *BearerClient
	region       Region
	templateBase map[string]interface{}
	// chatStreamURL 测试注入用：非空时覆盖 region 对应的上游 chat 流地址
	chatStreamURL string
}

// Model 是返回给前端的精简模型条目（仅保留下拉选择必要字段）
type Model struct {
	Key             string  `json:"key"`
	DisplayName     string  `json:"display_name"`
	Enable          bool    `json:"enable"`
	IsDefault       bool    `json:"is_default"`
	IsReasoning     bool    `json:"is_reasoning,omitempty"`
	ContextWindow   int     `json:"context_window,omitempty"`
	MaxOutputTokens int     `json:"max_output_tokens,omitempty"`
	MaxInputTokens  int     `json:"max_input_tokens,omitempty"`
	PriceFactor     float64 `json:"price_factor,omitempty"`
}

// NewBridge 创建 API 转换桥接。
// 支持两种认证方式：
//  1. OAuth device token (dt-xxx): 直接使用，调用 /api/v1/userinfo 获取用户信息
//  2. Personal Access Token (PAT): 调用 ExchangeJobToken 转换为 session token
func NewBridge(ctx context.Context, pat string, region Region, templateBase map[string]interface{}) (*Bridge, error) {
	// 注意：不要在这里打印凭证（哪怕是前缀）。插件日志会进入 CPA 进程日志，
	// 账号标识只允许出现在成功建立会话后的非敏感字段里。
	var identity AuthIdentity
	var name, id string

	deviceToken, refreshToken := ParseOAuthSecret(pat)
	if strings.HasPrefix(deviceToken, "dt-") {
		userInfo, err := FetchUserInfoWithToken(ctx, deviceToken, region)
		if err != nil {
			return nil, fmt.Errorf("fetch user info: %w", err)
		}
		name = StrVal(userInfo, "name")
		id = ResolveOAuthUserID(userInfo)
		identity = AuthIdentity{
			Name:               name,
			Aid:                id,
			Uid:                id,
			OrganizationId:     StrVal(userInfo, "organization_id"),
			OrganizationName:   StrVal(userInfo, "organization_name"),
			UserType:           StrValDefault(userInfo, "userType", "personal_standard"),
			SecurityOauthToken: deviceToken,
			RefreshToken:       refreshToken,
		}
	} else {
		// jobToken 交换发生在拿到 uid 之前：机器头用凭证种子稳定派生
		// （hub 的交换请求不带机器头，此处保留 QCCG 原有行为但消除随机漂移）
		seed := FingerprintSeed("", pat)
		jt, err := ExchangeJobToken(ctx, pat, DeriveMachineID(seed), DeriveMachineToken(seed), DeriveMachineType(seed), GetEndpoints(region).JobTokenURL)
		if err != nil {
			return nil, fmt.Errorf("exchangeJobToken: %w", err)
		}
		name = StrVal(jt, "name")
		id = StrVal(jt, "id")
		identity = AuthIdentity{
			Name:               name,
			Aid:                id,
			Uid:                id,
			UserType:           StrValDefault(jt, "userType", "personal_standard"),
			SecurityOauthToken: StrVal(jt, "securityOauthToken"),
			RefreshToken:       StrVal(jt, "refreshToken"),
		}
	}

	logger.Info("Bridge session for %s (%s)", name, id)
	// 稳定设备指纹：优先 uid 派生（双区统一、重启不变），uid 缺失退回凭证种子
	seed := FingerprintSeed(id, pat)
	mid := DeriveMachineID(seed)
	mtoken := DeriveMachineToken(seed)
	mtype := DeriveMachineType(seed)
	logger.Info("Bridge fingerprint machineid=%s (stable derive)", mid)
	sess, err := NewSession(identity, mid, mtoken, mtype)
	if err != nil {
		return nil, err
	}
	client := NewBearerClient(sess)

	return &Bridge{
		sess:         sess,
		client:       client,
		region:       region,
		templateBase: templateBase,
	}, nil
}

// ListAvailableModels 通过 cosy 签名调用 /algo/api/v2/model/list 拉取上游模型清单。
// 返回顶层 assistant 数组中 enable=true 的模型，按 is_default desc + display_name asc 排序。
func (b *Bridge) ListAvailableModels(ctx context.Context) ([]Model, error) {
	modelListURL := qoderModelListURL(b.region)
	resp, err := b.client.callGet(ctx, modelListURL)
	if err != nil {
		return nil, err
	}
	// debug: 打印响应顶层 key
	keys := make([]string, 0, len(resp))
	for k := range resp {
		keys = append(keys, k)
	}
	logger.Info("model list response keys: %v (region=%s)", keys, b.region)
	models := parseModels(resp)
	if len(models) == 0 {
		return nil, fmt.Errorf("%s -> empty model list, keys=%v", modelListURL, keys)
	}
	return models, nil
}

func parseModels(resp map[string]interface{}) []Model {
	// 按优先级尝试多个分类
	categories := []string{"assistant", "developer", "chat"}
	for _, cat := range categories {
		rawList, _ := resp[cat].([]interface{})
		out := extractModels(rawList)
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func extractModels(rawList []interface{}) []Model {
	out := make([]Model, 0, len(rawList))
	for _, it := range rawList {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		enable, _ := m["enable"].(bool)
		model := Model{
			Key:            StrVal(m, "key"),
			DisplayName:    StrVal(m, "display_name"),
			Enable:         enable,
			IsDefault:      func() bool { v, _ := m["is_default"].(bool); return v }(),
			IsReasoning:    func() bool { v, _ := m["is_reasoning"].(bool); return v }(),
			MaxInputTokens: int(FloatVal(m, "max_input_tokens")),
			PriceFactor:    FloatVal(m, "price_factor"),
		}
		if cc, ok := m["context_config"].(map[string]interface{}); ok {
			var defaultCfg map[string]interface{}
			for _, cfg := range cc {
				cfgMap, _ := cfg.(map[string]interface{})
				if cfgMap == nil {
					continue
				}
				isDefault, _ := cfgMap["is_default"].(bool)
				if isDefault {
					defaultCfg = cfgMap
					break
				}
			}
			if defaultCfg == nil {
				for _, cfg := range cc {
					cfgMap, _ := cfg.(map[string]interface{})
					if cfgMap != nil {
						defaultCfg = cfgMap
						break
					}
				}
			}
			if defaultCfg != nil {
				model.ContextWindow = int(FloatVal(defaultCfg, "token_count"))
			}
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = model.MaxInputTokens
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = model.MaxInputTokens
		}
		if model.IsReasoning {
			model.MaxOutputTokens = 32768
		} else {
			model.MaxOutputTokens = 16384
		}
		out = append(out, model)
	}
	return out
}

// DeepCopyMap does a JSON round-trip deep copy.
func DeepCopyMap(m map[string]interface{}) map[string]interface{} {
	data, _ := json.Marshal(m)
	var out map[string]interface{}
	json.Unmarshal(data, &out)
	return out
}

// CallOpts 控制单次 CallQoder 的可选行为。
type CallOpts struct {
	IsReasoning bool // 是否启用推理模式（对应上游 model_config.is_reasoning）
	MaxTokens   int  // 客户端请求的 max_tokens，0 表示使用模板默认值
}

func (b *Bridge) CallQoder(ctx context.Context, agent string, messages []interface{}, model string, tools interface{}, onDelta func(Delta)) error {
	return b.CallQoderWithOpts(ctx, agent, messages, model, tools, CallOpts{}, onDelta)
}

// deltaDispatcher 把上游 SSE 行分发为 Delta 回调，返回值供 openStreamLines
// 的 onLine 使用：false 表示停止读取上游流。
// 约束（review P1）：一旦捕获到流内业务错误（*upstreamErr 非 nil），
// 立即停止向 onDelta 分发任何后续 delta，且从置位错误的那一行起返回 false，
// 让 openStreamLines 及时退出——上游发出错误帧后若不关流，否则重开闸门
// 永不触发、请求挂起；错误由 CallQoder 在流结束后统一返回。
func deltaDispatcher(onDelta func(Delta), upstreamErr *error) func(string) bool {
	return func(line string) bool {
		if *upstreamErr != nil {
			return false
		}
		if !strings.HasPrefix(line, "data:") {
			return true
		}
		dataPayload := strings.TrimSpace(line[5:])
		if dataPayload == "[DONE]" {
			return true
		}
		delta := ExtractDelta(dataPayload)
		if delta.Err != nil {
			*upstreamErr = delta.Err
			return false
		}
		if !delta.isEmpty() {
			onDelta(delta)
		}
		return true
	}
}

func (b *Bridge) CallQoderWithOpts(ctx context.Context, agent string, messages []interface{}, model string, tools interface{}, opts CallOpts, onDelta func(Delta)) error {
	// 将客户端模型名（claude-sonnet-4-6 等）映射成 Qoder 上游内部 model.key（auto/qmodel_38max/gmodel/dmodel 等）。
	// 上游对未知 key 会走兜底返回内容，但不会把这次调用计入 quota，这是「请求成功但 dashboard 无用量」的根因。
	originalModel := model
	model = MapModel(agent, model)
	if model != originalModel {
		logger.Debug("mapModel %s/%s -> %s", agent, originalModel, model)
	}
	body := DeepCopyMap(b.templateBase)

	nid := NewUUID()
	body["request_id"] = nid
	body["chat_record_id"] = nid
	body["request_set_id"] = NewUUID()
	body["session_id"] = NewUUID()
	body["stream"] = true
	body["aliyun_user_type"] = b.sess.Identity.UserType

	if mc, ok := body["model_config"].(map[string]interface{}); ok {
		mc["key"] = model
		if opts.IsReasoning {
			mc["is_reasoning"] = true
		}
	}

	if opts.MaxTokens > 0 {
		if params, ok := body["parameters"].(map[string]interface{}); ok {
			params["max_tokens"] = opts.MaxTokens
		}
	}

	// Extract summary prompt from last user message for chat_context and business.name
	prompt := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if mm, ok := messages[i].(map[string]interface{}); ok && mm["role"] == "user" {
			prompt = NormalizeMessageContent(mm)
			if prompt == "" {
				if contents, ok := mm["contents"].([]interface{}); ok {
					for _, block := range contents {
						if b, ok := block.(map[string]interface{}); ok {
							if t, ok := b["text"].(string); ok {
								prompt = t
								break
							}
						}
					}
				}
			}
			break
		}
	}

	if biz, ok := body["business"].(map[string]interface{}); ok {
		biz["id"] = NewUUID()
		biz["begin_at"] = UnixMs()
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}

	if cc, ok := body["chat_context"].(map[string]interface{}); ok {
		if txt, ok := cc["text"].(map[string]interface{}); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]interface{}); ok {
			if oc, ok := extra["originalContent"].(map[string]interface{}); ok {
				oc["text"] = prompt
			}
		}
	}

	body["messages"] = messages
	if tools != nil {
		body["tools"] = tools
	}

	mcSource := "system"
	if mc, ok := body["model_config"].(map[string]interface{}); ok {
		if s, ok := mc["source"].(string); ok {
			mcSource = s
		}
	}

	qurl := qoderChatStreamURL(b.region)
	if b.chatStreamURL != "" {
		qurl = b.chatStreamURL
	}
	extra := map[string]string{
		"x-model-key":    model,
		"x-model-source": mcSource,
	}

	preview := prompt
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}
	logger.Info("callQoder model=%s prompt=%s", model, preview)
	logger.Debug("callQoder request body: %s", func() string { d, _ := json.Marshal(body); return string(d) }())

	// ---- SSE 信封重试闸门（第 3 步，参照 hub should_retry_envelope /
	// aggregate_with_envelope_retry）------------------------------------------
	// 关键形态：上游 HTTP200 建流后才在信封里投 418/5xx，连接层重试覆盖不到。
	// 闸门规则：只要「尚未向 onDelta 发出任何非空 delta」且错误属瞬时类
	// （信封 418/5xx/provider_error、传输层 TLS EOF 等），就重开上游重试，
	// 对调用方无感；已发出内容则直接上抛，避免客户端收到重复内容。
	// 客户端参数错 / 内容审核 / 401/429 经 IsTransientUpstream 判定为
	// 不可重试，快速失败。闸门做在此处，chat/claude/codex 三协议的
	// 流式与非流式路径全部自动覆盖。
	// 说明：连接阶段错误已在 openStreamLines 内部重试过；此处重开是
	// 第二层防护（有界：最多 TransientMaxRetries 次），主要覆盖流内信封错误。
	var lastErr error
	// 排队等待单独计数：免费模型的上游排队信号（isQueued/serviceAvailable=false）
	// 不受瞬时重试次数限制，但要受“等几次”的上限约束，且必须等上游建议的时长
	// （而不是 1s/2s 的瞬时退避——等不到位等于没等）。
	queueWaits := 0
	maxQueueWaits := QueueMaxWaits()
	for attempt := 0; attempt <= TransientMaxRetries+maxQueueWaits; attempt++ {
		if attempt > 0 {
			wait := RetryBackoff(attempt)
			if signal, queued := QueueSignalFromError(lastErr); queued {
				if queueWaits >= maxQueueWaits {
					return QueueSignalError(signal)
				}
				queueWaits++
				wait = queueWaitFor(signal)
				logger.Info("upstream queues model %s (%s); waiting %s before reopening (queue wait %d/%d)",
					signal.ModelKey, signal.QueueType, wait, queueWaits, maxQueueWaits)
			}
			if !core.SleepContext(ctx, wait) {
				return lastErr
			}
			logger.Info("reopen upstream after in-stream error (try %d/%d): %v",
				attempt+1, TransientMaxRetries+maxQueueWaits+1, lastErr)
		}
		emitted := false
		var upstreamErr error
		onDeltaWrapped := func(d Delta) {
			if !d.isEmpty() {
				emitted = true
			}
			onDelta(d)
		}
		streamErr := b.client.openStreamLines(ctx, qurl, body, extra,
			deltaDispatcher(onDeltaWrapped, &upstreamErr))
		if upstreamErr == nil && streamErr == nil {
			// 上游正常关流但未发出任何有效 delta/usage 帧：对齐 hub
			// "empty upstream stream" 显式报错（不纳入重开闸门——
			// isRetryableStreamError 对哨兵错误自然返回 false，不重试）
			if !emitted {
				return ErrEmptyStream
			}
			return nil
		}
		if upstreamErr != nil {
			lastErr = upstreamErr
		} else {
			lastErr = streamErr
		}
		if emitted || attempt >= TransientMaxRetries+maxQueueWaits || !isRetryableStreamError(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// isRetryableStreamError 判定流内/连接错误是否值得重开上游：
// 结构化上游错误按瞬时分类判定；裸传输错误（TLS EOF 等）按传输层判定；
// ctx 取消、业务错误、内容审核等一律不可重试。
func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		// 排队信号只有在我们会“等”的时候才值得重试：queue_max_waits=0（快速失败）时
		// 不能退化成 1s/2s 的瞬时重试（那样只会白打上游几次，仍拿不到结果）。
		// 判定按 ErrType 与信号两路走：友好化后的错误文本里可能已无排队标记。
		if ue.ErrType == ErrTypeModelBusy {
			return QueueMaxWaits() > 0
		}
		if _, queued := QueueSignalFromError(err); queued {
			return QueueMaxWaits() > 0
		}
		return IsTransientUpstream(ue.Status, ue.Detail)
	}
	return IsTransientTransport(err)
}

// sensitiveFieldNames 命中后其字符串子值会被脱敏（递归进数组/对象继续处理）
var sensitiveFieldNames = map[string]bool{
	"content":      true,
	"text":         true,
	"system":       true,
	"input":        true,
	"instructions": true,
	"prompt":       true,
	"description":  true,
}

func redactValue(v interface{}, parentField string) {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, child := range x {
			if s, ok := child.(string); ok {
				if (sensitiveFieldNames[k] || sensitiveFieldNames[parentField]) && len(s) > 32 {
					x[k] = fmt.Sprintf("<redacted len=%d>", len(s))
				}
				continue
			}
			redactValue(child, k)
		}
	case []interface{}:
		for i, item := range x {
			if s, ok := item.(string); ok {
				if sensitiveFieldNames[parentField] && len(s) > 32 {
					x[i] = fmt.Sprintf("<redacted len=%d>", len(s))
				}
				continue
			}
			redactValue(item, parentField)
		}
	}
}

func StrVal(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func StrValDefault(m map[string]interface{}, key, def string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return def
}

// InferAgent 根据模型名启发式推断 agent 类型，用于 chat/completions 这种多 agent 共用 endpoint
// 时选择正确的映射桶。模型名命中关键字按 gemini > claude > 默认 codex。
func InferAgent(model string) string {
	low := strings.ToLower(model)
	switch {
	case strings.Contains(low, "gemini"):
		return "gemini"
	case strings.Contains(low, "claude"), strings.Contains(low, "sonnet"), strings.Contains(low, "opus"), strings.Contains(low, "haiku"):
		return "claude"
	default:
		return "codex"
	}
}

// defaultModelMapping 是 bridge 的内置静态映射表。
// 采用「家族关键字 → Qoder model.key」形式，依赖下面 lookupMapping 的双向 substring 模糊匹配，
// 一条 "sonnet" 即可覆盖 claude-sonnet-4-6 / claude-sonnet-4-20250514 等所有变体。
//
// 设计原则：默认表只负责让请求落到合法 SKU 不出错。
// Qoder 上游合法 key 见 /v1/models 返回，随上游变化动态调整。
// GPT / Gemini 没有专属 SKU，统一映射到主力档 performance —— 既不浪费 ultimate 高 price_factor，
// 也不被 lite 限频；想分档（如 gpt-5→ultimate / gpt-5-mini→efficient）请在 UI 配置。
var defaultModelMapping = map[string]string{
	// Claude 三档
	"opus":   "qmodel_38max",
	"sonnet": "gmodel",
	"haiku":  "qfmodel",
	// 非 Claude 家族兜底
	"gpt":    "dmodel",
	"gemini": "gmodel",
}

// CanonicalSlug converts a display name into a standard lowercase slug (e.g. "Qwen3.8-Flash" -> "qwen3.8-flash").
func CanonicalSlug(displayName, rawKey string) string {
	trimmed := strings.TrimSpace(displayName)
	if trimmed == "" {
		return strings.ToLower(strings.TrimSpace(rawKey))
	}
	low := strings.ToLower(trimmed)
	switch low {
	case "auto", "performance", "ultimate", "efficient":
		return low
	}
	return strings.ReplaceAll(low, " ", "-")
}

var (
	slugMu       sync.RWMutex
	slugToRawKey = map[string]string{
		"qwen3.8-flash":     "qfmodel",
		"qwen3.8-max":       "qmodel_38max",
		"qwen3.7-max":       "qmodel_latest",
		"qwen3.7-plus":      "qmodel",
		"qwen3.7-flash":     "q37fmodel",
		"deepseek-flash":    "dfmodel",
		"deepseek-v4-pro":   "dmodel",
		"glm-5.3-flash":     "gfmodel",
		"glm-5.3":           "gmodel",
		"glm-5.1":           "gm51model",
		"kimi-k3":           "kmodel_latest",
		"kimi-k2.8-preview": "kmodel",
		"minimax-m3":        "mmodel",
		"minimax-m2.7":      "mmodel",
		"sonus":             "smodel",
		"cantus":            "cmodel",
	}
)

func RegisterModelSlug(slug, rawKey string) {
	slugMu.Lock()
	defer slugMu.Unlock()
	slugToRawKey[strings.ToLower(strings.TrimSpace(slug))] = strings.TrimSpace(rawKey)
}

// MapModel 解析顺序（当前仅使用静态映射）：
//  1. slugToRawKey 精确命中（如客户端传规范化 ID "qwen3.8-flash" -> "qfmodel"）
//  2. defaultModelMapping 精确命中
//  3. defaultModelMapping 双向 substring 模糊匹配（按 source 长度倒序，最长优先）
//  4. ToLower 兜底（如客户端直接传 Qoder 原始 key 如 "Performance"/"dmodel"）
//
// agent 由 handler 入口传入并保留用于未来按 agent 区分的扩展；当前不参与映射。
//
// 双向 substring 含义：source 包含 model（短关键字匹配长模型名，如 "sonnet" → "claude-sonnet-4-6"）
// 或 model 包含 source（长别名匹配短模型名，反向也能命中）。
func MapModel(agent, model string) string {
	if model == "" {
		return model
	}

	mLow := strings.ToLower(strings.TrimSpace(model))
	slugMu.RLock()
	if rawKey, ok := slugToRawKey[mLow]; ok {
		slugMu.RUnlock()
		return rawKey
	}
	slugMu.RUnlock()

	// 内置默认映射兜底
	if mapped := lookupMapping(model, defaultModelMapping); mapped != "" {
		return mapped
	}

	// 大小写归一化兜底（客户端可能直接传 Qoder key 如 "Performance"/"dmodel"）
	return strings.ToLower(model)
}

// lookupMapping 在单张映射表内执行：精确匹配 → 长度倒序 + 双向 substring 模糊匹配。
// 未命中返回空串。
func lookupMapping(model string, table map[string]string) string {
	if len(table) == 0 {
		return ""
	}
	// 1. 精确命中
	if v, ok := table[model]; ok && v != "" {
		return v
	}
	// 2. 长度倒序，确保 "claude-sonnet-4-6" 优先于 "sonnet" 命中
	type kv struct{ src, dst string }
	pairs := make([]kv, 0, len(table))
	for k, v := range table {
		if k == "" || v == "" {
			continue
		}
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].src) > len(pairs[j].src) })
	mLow := strings.ToLower(model)
	for _, p := range pairs {
		sLow := strings.ToLower(p.src)
		if strings.Contains(mLow, sLow) || strings.Contains(sLow, mLow) {
			return p.dst
		}
	}
	return ""
}

// 上游的排队/服务未就绪信号。
//
// 免费模型（如 Qwen3.8-Flash / qfmodel）会被上游放进排队队列，返回 HTTP 403 +
// 形如下面的载荷（message 里套了两层 JSON 字符串，且是转义后的）：
//
//	{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,
//	 \\\"modelKey\\\":\\\"qfmodel\\\",\\\"queueCount\\\":0,\\\"queueType\\\":\\\"p3\\\",
//	 \\\"retryAfterSeconds\\\":30,\\\"serviceAvailable\\\":false,\\\"waitTime\\\":30}\"}"}
//
// 这不是额度/凭证错误：等一会儿重试就能成功。旧实现把它当成 403 权限错误，
// 于是免费模型永远用不了（宿主还会显示成 insufficient_quota，误导用户去充值）。
type QueueSignal struct {
	Queued            bool
	ServiceAvailable  bool
	ModelKey          string
	QueueType         string
	QueueCount        int
	RetryAfterSeconds int
	WaitTimeSeconds   int
}

// WaitDuration 返回按上游建议应等待的时长（配置覆盖的值不经过这里）。
func (s QueueSignal) WaitDuration() time.Duration {
	if s.retryAfter() > 0 {
		return s.cappedWait()
	}
	return time.Duration(DefaultQueueWaitSeconds) * time.Second
}

// retryAfter 是上游给的等待秒数（retryAfterSeconds 优先，退化到 waitTime）。
func (s QueueSignal) retryAfter() int {
	if s.RetryAfterSeconds > 0 {
		return s.RetryAfterSeconds
	}
	if s.WaitTimeSeconds > 0 {
		return s.WaitTimeSeconds
	}
	return 0
}

func (s QueueSignal) cappedWait() time.Duration {
	d := time.Duration(s.retryAfter()) * time.Second
	if d > MaxQueueWait {
		return MaxQueueWait
	}
	return d
}

const (
	// DefaultQueueWaitSeconds 是上游没给建议等待时长时的兜底（秒）。
	DefaultQueueWaitSeconds = 15
	// DefaultQueueMaxWaits 是一次请求内默认最多为排队等待几次。
	DefaultQueueMaxWaits = 2
	// MaxQueueWait 是单次排队等待的上限，避免上游给个离谱数字把请求挂死。
	MaxQueueWait = 90 * time.Second
)

// QueueMaxWaits 返回当前允许的排队等待次数。
func QueueMaxWaits() int { return DefaultQueueMaxWaits }

// queueWaitFor 计算这次排队该等多久：配置覆盖优先，否则用上游建议值（受 MaxQueueWait 约束）。
func queueWaitFor(signal QueueSignal) time.Duration {
	if signal.retryAfter() > 0 {
		return signal.cappedWait()
	}
	return time.Duration(DefaultQueueWaitSeconds) * time.Second
}

// quoted 匹配可能被转义的 JSON 引号：正文里常见 \"isQueued\" 这种形态。
const quoted = `(?:\\?")?`

var (
	queueIntPattern    = regexp.MustCompile(quoted + `(?:retryAfterSeconds|waitTime)` + quoted + `\s*:\s*\\?"?\s*(\d+)`)
	queueStringPattern = regexp.MustCompile(quoted + `(?:modelKey|queueType)` + quoted + `\s*:\s*\\?"?\s*([A-Za-z0-9_.\-]+)`)
	queueBoolPattern   = regexp.MustCompile(quoted + `(?:isQueued|serviceAvailable)` + quoted + `\s*:\s*(true|false)`)
	queueQueuedPattern = regexp.MustCompile(quoted + `isQueued` + quoted + `\s*:\s*(true|false)`)
	queueAvailPattern  = regexp.MustCompile(quoted + `serviceAvailable` + quoted + `\s*:\s*(true|false)`)
)

// ParseQueueSignal 从上游响应正文里识别排队信号。
//
// 正文可能是多层 JSON 字符串（宿主/上游习惯把错误体再包一层），所以先逐层解包，
// 再做一次正则兜底扫描——只依赖少数字段存在，不假设整体结构。
func ParseQueueSignal(detail string) (QueueSignal, bool) {
	trimmed := strings.TrimSpace(detail)
	if trimmed == "" {
		return QueueSignal{}, false
	}
	if !strings.Contains(trimmed, "isQueued") && !strings.Contains(trimmed, "serviceAvailable") &&
		!strings.Contains(trimmed, "queueType") {
		return QueueSignal{}, false
	}

	signal := QueueSignal{ServiceAvailable: true}
	// seenBool 记录是否从正文里真的读到了排队布尔字段（避免把无关错误误判成排队）。
	seenBool := false
	applyQueueFields := func(segment string) {
		// 逐层解包：当前段本身是 JSON 就取 message/msg/detail 继续，同时读本层字段。
		for depth := 0; depth < 6 && segment != ""; depth++ {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(segment), &parsed) != nil {
				return
			}
			if queued, ok := parsed["isQueued"].(bool); ok {
				signal.Queued = queued
				seenBool = true
			}
			if available, ok := parsed["serviceAvailable"].(bool); ok {
				signal.ServiceAvailable = available
				seenBool = true
			}
			if key, ok := parsed["modelKey"].(string); ok && strings.TrimSpace(key) != "" {
				signal.ModelKey = strings.TrimSpace(key)
			}
			if queueType, ok := parsed["queueType"].(string); ok && strings.TrimSpace(queueType) != "" {
				signal.QueueType = strings.TrimSpace(queueType)
			}
			signal.RetryAfterSeconds = firstPositiveInt(signal.RetryAfterSeconds, parsed["retryAfterSeconds"])
			signal.WaitTimeSeconds = firstPositiveInt(signal.WaitTimeSeconds, parsed["waitTime"])
			signal.QueueCount = firstPositiveInt(signal.QueueCount, parsed["queueCount"])

			next := ""
			for _, key := range []string{"message", "msg", "detail", "error"} {
				switch value := parsed[key].(type) {
				case string:
					if strings.TrimSpace(value) != "" {
						next = strings.TrimSpace(value)
					}
				case map[string]interface{}:
					if encoded, errMarshal := json.Marshal(value); errMarshal == nil {
						next = string(encoded)
					}
				}
				if next != "" {
					break
				}
			}
			segment = next
		}
	}
	applyQueueFields(trimmed)

	// 正则兜底：载荷可能被转义得连 json.Unmarshal 都吃不下（例如上游直接拼字符串）。
	if signal.RetryAfterSeconds == 0 {
		if match := queueIntPattern.FindStringSubmatch(trimmed); len(match) == 2 {
			signal.RetryAfterSeconds, _ = strconv.Atoi(match[1])
		}
	}
	if signal.ModelKey == "" {
		if match := queueStringPattern.FindStringSubmatch(trimmed); len(match) == 2 {
			signal.ModelKey = match[1]
		}
	}
	// 布尔字段的兜底：正文可能被转义到 json.Unmarshal 都吃不下（上游拼字符串的情形）。
	if match := queueQueuedPattern.FindStringSubmatch(trimmed); len(match) == 2 {
		if !signal.Queued {
			signal.Queued = match[1] == "true"
			seenBool = true
		}
	}
	if match := queueAvailPattern.FindStringSubmatch(trimmed); len(match) == 2 {
		if signal.ServiceAvailable {
			signal.ServiceAvailable = match[1] == "true"
			seenBool = true
		}
	}
	if !seenBool {
		// 一个布尔字段都没读到，也没有任何排队痕迹：不是排队信号。
		if !queueBoolPattern.MatchString(trimmed) {
			return QueueSignal{}, false
		}
	}
	if signal.ServiceAvailable && !signal.Queued {
		return QueueSignal{}, false
	}
	return signal, true
}

func firstPositiveInt(current int, value interface{}) int {
	if current > 0 {
		return current
	}
	switch typed := value.(type) {
	case float64:
		if typed > 0 {
			return int(typed)
		}
	case json.Number:
		if parsed, errParse := typed.Int64(); errParse == nil && parsed > 0 {
			return int(parsed)
		}
	case string:
		if parsed, errParse := strconv.Atoi(strings.TrimSpace(typed)); errParse == nil && parsed > 0 {
			return parsed
		}
	}
	return current
}

// QueueSignalError 是排队等待预算耗尽后的错误：等待一段时间后重试即可，
// 与额度/凭证无关。
func QueueSignalError(signal QueueSignal) *UpstreamError {
	detail := "上游排队中"
	if signal.ModelKey != "" {
		detail = "上游模型 " + signal.ModelKey + " 排队中"
	}
	if signal.QueueType != "" {
		detail += "（队列 " + signal.QueueType + "）"
	}
	detail += "，等待后重试即可"
	return &UpstreamError{
		Status:  503,
		Detail:  detail,
		ErrType: ErrTypeModelBusy,
		Message: detail + "（不是额度或凭证问题；插件已按上游建议等待过，仍不可用）",
		// 必须把信号本身带上：友好化后的文本里已经没有 isQueued 之类标记，
		// 上层（isRetryableStreamError / 执行器）要靠这里判断“这是排队，不是瞬时故障”。
		Queue: &signal,
	}
}

// QueueSignalFromError 从错误里取出排队信号：优先用结构化字段，
// 退化到在消息/详情文本里重新识别（错误可能被包装过）。
func QueueSignalFromError(err error) (QueueSignal, bool) {
	if err == nil {
		return QueueSignal{}, false
	}
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		if upstream.Queue != nil {
			return *upstream.Queue, true
		}
		if signal, queued := ParseQueueSignal(upstream.Detail); queued {
			return signal, true
		}
	}
	return ParseQueueSignal(err.Error())
}

// Region 标识 Qoder 站点区域
type Region string

const (
	RegionGlobal Region = "global"
	RegionCN     Region = "cn"
)

// Endpoints 包含某个 region 对应的所有 API 端点
type Endpoints struct {
	DeviceLoginBase string // OAuth 登录页
	PollEndpoint    string // deviceToken/poll
	UserinfoBase    string // /api/v1/userinfo
	PlanEndpoint    string // /api/v2/user/plan
	QuotaEndpoint   string // /api/v2/quota/usage
	ChatStreamURL   string // SSE chat
	ModelListURL    string // model list
	JobTokenURL     string // PAT exchange
	// SashBase 是签到/活动接口的域名（/sash/api/v1/...），仅域名无路径。
	// 必须按区域选：把国际版 token 打到国内域名会得到 401 TOKEN_EXPIRE。
	SashBase string
}

var endpointsGlobal = Endpoints{
	DeviceLoginBase: "https://qoder.com/device/selectAccounts",
	PollEndpoint:    "https://openapi.qoder.sh/api/v1/deviceToken/poll",
	UserinfoBase:    "https://openapi.qoder.sh/api/v1/userinfo",
	PlanEndpoint:    "https://openapi.qoder.sh/api/v2/user/plan",
	QuotaEndpoint:   "https://openapi.qoder.sh/api/v2/quota/usage",
	ChatStreamURL:   "https://api1.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
	ModelListURL:    "https://api2.qoder.sh/algo/api/v2/model/list?Encode=1",
	JobTokenURL:     "https://center.qoder.sh/algo/api/v3/user/jobToken?Encode=1",
	SashBase:        "https://openapi.qoder.sh",
}

var endpointsCN = Endpoints{
	DeviceLoginBase: "https://qoder.com.cn/device/selectAccounts",
	PollEndpoint:    "https://openapi.qoder.com.cn/api/v1/deviceToken/poll",
	UserinfoBase:    "https://openapi.qoder.com.cn/api/v1/userinfo",
	PlanEndpoint:    "https://openapi.qoder.com.cn/api/v2/user/plan",
	QuotaEndpoint:   "https://openapi.qoder.com.cn/api/v2/quota/usage",
	ChatStreamURL:   "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
	ModelListURL:    "https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1",
	JobTokenURL:     "https://gateway.qoder.com.cn/algo/api/v3/user/jobToken?Encode=1",
	SashBase:        "https://openapi.qoder.com.cn",
}

// GetEndpoints 根据 region 返回对应端点配置，空值或未知值视为 global
func GetEndpoints(region Region) Endpoints {
	if region == RegionCN {
		return endpointsCN
	}
	return endpointsGlobal
}

// NormalizeRegion 将字符串归一化为合法 Region 值
func NormalizeRegion(s string) Region {
	if s == "cn" || s == "CN" {
		return RegionCN
	}
	return RegionGlobal
}
