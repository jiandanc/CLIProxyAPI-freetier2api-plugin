package workbuddy

// 本文件实现对话调用：请求构造、出站、错误分类与流式读取。

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// ChatRequest 是一次对话调用的输入。
type ChatRequest struct {
	// Credential 是被选中的账号凭证。
	Credential *Credential
	// Body 是客户端原始请求体（OpenAI 形态）。
	Body []byte
	// Model 是上游模型名（已剥离 realm 前缀）。
	Model string
	// ClientIP 是客户端 IP（仅在 PassthroughIP 开启时才会上送）。
	ClientIP string
	// Meta 是会话头族。
	Meta ChatMeta
}

// ChatResult 是一次对话调用的结果。
type ChatResult struct {
	// Response 是非流式聚合后的响应体（流式调用时为空）。
	Response []byte
	// Stats 是流式统计（非流式调用时为零值）。
	Stats StreamStats
	// Credential 是实际使用的凭证（可能被刷新过）。
	Credential *Credential
}

// effortTables 是模型名 → 档位的映射，由模型目录在刷新时注入。
//
// 用包级注入而不是逐调用传参：档位表是全局派生数据（来自模型探测结果），
// 而对话路径的调用点很多，逐个透传只会让签名变长。
var (
	effortMu         sync.RWMutex
	defaultEffortMap = map[string]string{}
	supportEffortMap = map[string][]string{}
)

// SetEffortTables 注入档位表并合并到全局（支持多域合并，不互相覆盖）。
func SetEffortTables(defaults map[string]string, supported map[string][]string) {
	effortMu.Lock()
	defer effortMu.Unlock()
	for k, v := range defaults {
		defaultEffortMap[k] = v
	}
	for k, v := range supported {
		supportEffortMap[k] = v
	}
}

// SetEffortTablesForRegion 注入指定域的档位表并合并到全局。
func SetEffortTablesForRegion(region Region, defaults map[string]string, supported map[string][]string) {
	SetEffortTables(defaults, supported)
}

// effortTables 读取档位表。
func effortTables() (map[string]string, map[string][]string) {
	effortMu.RLock()
	defer effortMu.RUnlock()
	return defaultEffortMap, supportEffortMap
}

// ChatStream 发起一次流式对话，返回可供读取的响应体。
//
// 调用方负责关闭返回的 ReadCloser。上游强制流式（请求体里的 stream 恒为 true），
// 因此这是唯一的对话出口：非流式需求由 Aggregate 在本侧聚合。
func (c *Client) ChatStream(req ChatRequest) (io.ReadCloser, error) {
	if req.Credential == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := req.Credential.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	// 改写管线：加 stream、归一字段、注入思维链、脱敏。
	conversationKey := ConversationKey(req.Body)
	cacheKey := buildCacheKey(req.Credential.UIDValue(), firstNonEmpty(conversationKey, req.Meta.ConversationRequestID))
	defaultEfforts, supportedEfforts := effortTables()
	prepared := PrepareBody(req.Body, PrepareOptions{
		Model:            model,
		PromptMode:       c.opts.PromptMode,
		PromptText:       c.opts.PromptText,
		Sanitize:         c.opts.SanitizeFingerprints,
		DefaultEfforts:   defaultEfforts,
		SupportedEfforts: supportedEfforts,
		CacheKey:         cacheKey,
		Global:           region.IsGlobal(),
	})

	endpoints := GetEndpoints(region)
	httpReq, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost,
		endpoints.ChatBase+chatCompletionsPath, strings.NewReader(string(prepared)))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	c.applyChatHeaders(httpReq, req.Credential, req.Meta, req.ClientIP)

	resp, errDo := c.chatClient(chatHeaderTimeout).Do(httpReq)
	if errDo != nil {
		// 传输层错误：不是账号问题（不产生分类副作用），但换号重试有意义。
		return nil, transientError(errDo)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
		closeBody(resp.Body)
		errClassified := enrichClassifyError(resp.StatusCode, string(body), resp.Header)
		if errClassified != nil && errClassified.Kind == KindContentBlocked && c.opts.OnContentBlocked != nil {
			c.opts.OnContentBlocked()
		}
		return nil, errClassified
	}
	return resp.Body, nil
}

// transientError 把传输层错误包装成一个「可重试但不罚号」的错误。
//
// 与分类错误的区别：Status 为 0，宿主按 502 处理并换号重试，
// 但不会因它把凭证标记为坏（半截响应不该让账号背锅）。
func transientError(err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{Kind: KindServer, Status: 0, Msg: err.Error()}
}

// Chat 发起一次对话并聚合为非流式响应。
func (c *Client) Chat(req ChatRequest) (*ChatResult, error) {
	reader, errStream := c.ChatStream(req)
	if errStream != nil {
		return nil, errStream
	}
	defer closeBody(reader)

	aggregated, errAggregate := Aggregate(reader)
	if errAggregate != nil {
		if IsEmptyStreamError(errAggregate) {
			return nil, &Error{Kind: KindServer, Status: http.StatusBadGateway,
				Msg: "upstream stream contained no valid data events (upstream_parse)"}
		}
		return nil, errAggregate
	}
	return &ChatResult{Response: aggregated, Credential: req.Credential}, nil
}
