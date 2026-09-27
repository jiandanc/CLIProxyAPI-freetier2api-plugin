package main

// 本文件实现 executor 能力：非流式执行、流式执行、token 计数与出站 HTTP 透传。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"workbuddy2api-plugin/cpasdk/pluginabi"
	"workbuddy2api-plugin/cpasdk/pluginapi"
	"workbuddy2api-plugin/internal/cb"
	"workbuddy2api-plugin/internal/httpx"
	"workbuddy2api-plugin/internal/logger"
)

const (
	// executorRPCTimeout 是非流式执行的内部上限（流式另有空闲控制）。
	executorRPCTimeout = 10 * time.Minute
	// streamEmitTimeout 是单次流分片投递的上限。
	streamEmitTimeout = 30 * time.Second
)

// executorRPCRequest 与宿主的 rpcExecutorRequest 对齐。
//
// 注意：内嵌字段按 Go 字段名（无 json tag）传输，stream_id / host_callback_id 用小写键。
type executorRPCRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// rpcExecutorStreamResponse 与宿主的 rpcExecutorStreamResponse 对齐。
//
// 本插件走「同步返回 headers + 异步 emit 分片」，因此只返回 headers。
type rpcExecutorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

// preparedExecution 汇集一次执行的同步校验结果。
type preparedExecution struct {
	rpc        executorRPCRequest
	credential *cb.Credential
	region     cb.Region
	model      string
}

// handleExecutorExecute 执行一次非流式对话。
func handleExecutorExecute(request []byte) ([]byte, error) {
	prepared, errPrepare := prepareExecution(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	ctx, cancel := context.WithTimeout(
		httpx.WithCallbackID(context.Background(), prepared.rpc.HostCallbackID), executorRPCTimeout)
	defer cancel()

	client := newUpstreamClient(ctx)
	result, errChat := client.Chat(cb.ChatRequest{
		Credential: prepared.credential,
		Body:       prepared.rpc.Payload,
		Model:      prepared.model,
		ClientIP:   cb.ExtractClientIP(prepared.rpc.Headers),
		Meta:       chatMetaFor(prepared.rpc),
	})
	if errChat != nil {
		return nil, errorToPluginError(errChat)
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: result.Response,
		Headers: http.Header{"Content-Type": []string{jsonContentType}},
	})
}

// handleExecutorExecuteStream 执行一次流式对话。
//
// 架构：**同步返回响应头 + goroutine 异步投递分片**。
// 宿主先拿到 headers 构造下游响应，之后通过 host.stream.emit 接收分片、
// 通过 host.stream.close 收到结束信号。
func handleExecutorExecuteStream(request []byte) ([]byte, error) {
	prepared, errPrepare := prepareExecution(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	if strings.TrimSpace(prepared.rpc.StreamID) == "" {
		return nil, newPluginError("invalid_request", "stream_id is required for streaming execution", http.StatusBadRequest)
	}

	// 头部必须同步返回：宿主用它们构造下游响应。
	streamHeaders := http.Header{
		"Content-Type":  []string{"text/event-stream"},
		"Cache-Control": []string{"no-cache"},
	}

	streamCtx, cancelStream, finishStream := beginPluginStream()
	go func() {
		defer finishStream()
		runStreamExecution(streamCtx, cancelStream, prepared)
	}()

	return okEnvelope(rpcExecutorStreamResponse{Headers: streamHeaders})
}

// runStreamExecution 在后台把上游 SSE 转发给宿主流。
func runStreamExecution(streamCtx context.Context, cancelStream context.CancelFunc, prepared preparedExecution) {
	ctx := httpx.WithCallbackID(streamCtx, prepared.rpc.HostCallbackID)
	client := newUpstreamClient(ctx)

	reader, errStream := client.ChatStream(cb.ChatRequest{
		Credential: prepared.credential,
		Body:       prepared.rpc.Payload,
		Model:      prepared.model,
		ClientIP:   cb.ExtractClientIP(prepared.rpc.Headers),
		Meta:       chatMetaFor(prepared.rpc),
	})
	if errStream != nil {
		// 交给 closeStream 统一收尾：先 emit 再 close 会给同一条流发两次关闭。
		closeStream(prepared.rpc.HostCallbackID, prepared.rpc.StreamID, streamFailureMessage(errStream))
		return
	}
	defer func() {
		if errClose := reader.Close(); errClose != nil {
			logger.Debug("close upstream stream failed: %v", errClose)
		}
	}()

	streamID := prepared.rpc.StreamID
	callbackID := prepared.rpc.HostCallbackID
	failure := ""
	_, errForward := cb.Stream(reader, cb.StreamOptions{
		Emit: func(payload []byte) error {
			if errEmit := emitStreamChunk(callbackID, streamID, payload); errEmit != nil {
				// 下游断开：取消上游读取，静默收尾（这不是错误）。
				cancelStream()
				return errEmit
			}
			return nil
		},
		Hint: func(code string) string { return gatewayHint(code, prepared) },
	})
	if errForward != nil && !cb.IsEmptyStreamError(errForward) && streamCtx.Err() == nil {
		// 只有非取消的真实失败才报给下游；用户主动断开不该产生错误帧。
		failure = streamFailureMessage(errForward)
	} else if cb.IsEmptyStreamError(errForward) {
		failure = "upstream returned an empty stream (no valid data events)"
	}
	closeStream(callbackID, streamID, failure)
}

// prepareExecution 完成同步校验：请求解码、凭证解析、模型与域解析。
func prepareExecution(request []byte) (preparedExecution, error) {
	var prepared preparedExecution
	if errDecode := decodeRequest(request, &prepared.rpc); errDecode != nil {
		return prepared, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}

	// 本插件只声明 chat-completions。宿主若送来其它格式，说明双方声明不一致，
	// 明确报错比静默按错误格式处理更容易定位。
	if format := strings.TrimSpace(prepared.rpc.Format); format != "" && !isChatFormat(format) {
		return prepared, newPluginError("unsupported_format",
			fmt.Sprintf("unsupported protocol format %q (this plugin only handles %s)", format, formatChatCompletions),
			http.StatusBadRequest)
	}
	if format := strings.TrimSpace(prepared.rpc.SourceFormat); format != "" && !isChatFormat(format) {
		return prepared, newPluginError("unsupported_format",
			fmt.Sprintf("unsupported source format %q (this plugin only handles %s)", format, formatChatCompletions),
			http.StatusBadRequest)
	}

	ctx := httpx.WithCallbackID(context.Background(), prepared.rpc.HostCallbackID)
	credential, errCredential := credentialForAuth(ctx, prepared.rpc.HostCallbackID,
		prepared.rpc.StorageJSON, prepared.rpc.AuthID, prepared.rpc.AuthAttributes)
	if errCredential != nil {
		return prepared, newPluginError("workbuddy_credential_missing", errCredential.Error(), http.StatusUnauthorized)
	}
	prepared.credential = credential

	// 模型带的 realm 前缀优先；无前缀时由凭证决定域。
	region, bareModel := RealmForRequest(prepared.rpc.Model, credential)
	prepared.region = region
	prepared.model = bareModel

	// 检查模型是否已被插件配置禁用。
	if isModelDisabled(region, bareModel) {
		return prepared, newPluginError("model_disabled",
			fmt.Sprintf("model %q is disabled by plugin configuration", prepared.rpc.Model),
			http.StatusBadRequest)
	}

	// 域被配置关闭时直接拒绝（而不是让请求打到错误的上游域名）。
	if !realmEnabled(loadedConfig(), string(region)) {
		return prepared, newPluginError("workbuddy_realm_disabled",
			fmt.Sprintf("realm %s is disabled by plugin configuration", region), http.StatusBadRequest)
	}
	if strings.TrimSpace(bareModel) == "" {
		return prepared, newPluginError("invalid_request", "model is required", http.StatusBadRequest)
	}
	return prepared, nil
}

// isChatFormat 判断格式名是否属于 chat-completions 家族。
func isChatFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai", "chat-completions", "chat_completions", "openai-chat-completions":
		return true
	}
	return false
}

// chatMetaFor 从执行请求派生会话头族。
//
// 会话键来自请求体（客户端传的会话标识，或由 system+首条 user 派生），
// 同一次请求内所有重试共用同一组头——上游后台按 X-Conversation-Request-ID 聚合。
func chatMetaFor(rpc executorRPCRequest) cb.ChatMeta {
	body := rpc.Payload
	conversationKey := cb.ConversationKey(body)
	turnKey := cb.TurnKey(body)

	// 入站透传的链路 ID 优先（调用方可能已经建好了链路）。
	traceID := ""
	if rpc.Headers != nil {
		traceID = strings.TrimSpace(rpc.Headers.Get("X-Trace-ID"))
	}

	conversationRequestID := ""
	switch {
	case conversationKey != "" && turnKey != "":
		conversationRequestID = cb.TurnRequestID(conversationKey + ":" + turnKey)
	case turnKey != "":
		conversationRequestID = cb.TurnRequestID(turnKey)
	case conversationKey != "":
		conversationRequestID = cb.RequestIDForKey(conversationKey)
	default:
		conversationRequestID = cb.TurnRequestID("")
	}

	return cb.ChatMeta{
		ConversationID:        conversationKey,
		ConversationRequestID: conversationRequestID,
		TraceID:               traceID,
	}
}

// handleExecutorCountTokens 估算 token 数。
//
// 上游没有 token 计数接口，因此这是**保守估算**：宁可高估也不能低估，
// 因为客户端用它做上下文管理——低估会导致客户端把超长上下文发上来被上游拒绝。
// 响应里显式标注 estimated，避免被当成上游精确值。
func handleExecutorCountTokens(request []byte) ([]byte, error) {
	var rpc executorRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	estimate := estimateTokens(rpc.OriginalRequest, rpc.Payload)
	payload, errMarshal := json.Marshal(map[string]any{
		"total_tokens":  estimate,
		"input_tokens":  estimate,
		"output_tokens": 0,
		"estimated":     true,
	})
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal token estimate: %w", errMarshal)
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{jsonContentType}},
	})
}

// estimateTokens 按字节数估算 token。
//
// 经验值：英文约 4 字节/token，中文约 3 字节/token。
// 统一按 3 估算（偏保守，倾向高估）。
func estimateTokens(payloads ...[]byte) int {
	total := 0
	for _, payload := range payloads {
		total += len(payload)
	}
	if total == 0 {
		return 0
	}
	tokens := total / 3
	if tokens < 1 {
		return 1
	}
	return tokens
}

// handleExecutorHTTPRequest 让宿主把一次出站 HTTP 交给插件执行。
//
// 本插件不需要这个能力（所有出站都由插件主动发起并经宿主的 HTTP 桥），
// 但既然声明了 executor 就必须实现。返回"不支持"比返回错误的成功更诚实。
func handleExecutorHTTPRequest(request []byte) ([]byte, error) {
	return nil, newPluginError("unsupported_operation",
		"this plugin does not proxy executor HTTP requests; outbound calls go through the host HTTP bridge",
		http.StatusNotImplemented)
}

// emitStreamChunk 把一段载荷推给宿主流。
func emitStreamChunk(callbackID, streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("stream id is missing")
	}
	_, errCall := callHostScoped(callbackID, pluginabi.MethodHostStreamEmit, map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	if errCall != nil {
		logger.Debug("stream emit failed: %v", errCall)
	}
	return errCall
}

// closeStream 关闭宿主流；failure 非空时由宿主转为错误帧。
func closeStream(callbackID, streamID, failure string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	payload := map[string]any{"stream_id": streamID}
	if failure != "" {
		payload["error"] = failure
	}
	if _, errCall := callHostScoped(callbackID, pluginabi.MethodHostStreamClose, payload); errCall != nil {
		logger.Debug("stream close failed: %v", errCall)
	}
}

// streamFailureMessage 把错误转成给下游看的消息。
//
// 上游的原始错误信息比我们重写的更有诊断价值，因此优先透传。
func streamFailureMessage(err error) string {
	if err == nil {
		return ""
	}
	var upstreamErr *cb.Error
	if asUpstreamError(err, &upstreamErr) && strings.TrimSpace(upstreamErr.Msg) != "" {
		return upstreamErr.Msg
	}
	return err.Error()
}

// gatewayHint 返回给某个错误码附加的提示文案（没有则返回空串）。
func gatewayHint(code string, prepared preparedExecution) string {
	trimmed := strings.TrimSpace(code)
	switch trimmed {
	case "11133":
		if cb.HasImagePart(prepared.rpc.Payload) && !modelSupportsImages(prepared) {
			return "model " + prepared.model + " does not support images; pick one with image support from /v1/models"
		}
		return "request parameters were rejected by the model provider; check message format and model capabilities"
	case "11135":
		return "image data rejected by upstream; use a real/valid image, may need a new conversation"
	case "11115":
		return "request context exceeds the model limit; reduce history or message size"
	case "6004":
		return "this model is temporarily rate-limited upstream; retry later or switch model"
	case "11102":
		return "upstream has no such model on this backend; switch model or retry on another account"
	}
	return ""
}

func modelSupportsImages(prepared preparedExecution) bool {
	cached := cb.CachedModels()[prepared.region]
	for _, model := range cached {
		if model.ID == prepared.model {
			return model.SupportsImages
		}
	}
	return true
}
