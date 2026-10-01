package main

// 本文件实现 executor 能力：非流式执行、流式执行、token 计数与出站 HTTP 透传。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/workbuddy"
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
	vendor     core.Vendor
	credential *core.Credential
	model      string
	// modelAliased 报告请求命中了别名：出站前需要把请求体里的模型名一并还原。
	modelAliased bool
}

// executeRequest 把宿主的执行请求转成供应商无关的形态。
func (p preparedExecution) executeRequest() *core.ExecuteRequest {
	payload := p.rpc.Payload
	if p.modelAliased {
		// 供应商的对话实现多从请求体读 model（Qoder / Trae 等），别名必须在这里
		// 一并还原成官方 ID，否则上游收到的是别名。
		payload = rewritePayloadModel(payload, p.model)
	}
	return &core.ExecuteRequest{
		Model:    p.model,
		Payload:  payload,
		Headers:  p.rpc.Headers,
		ClientIP: workbuddy.ExtractClientIP(p.rpc.Headers),
		Stream:   p.rpc.Stream,
	}
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

	response, errExecute := prepared.vendor.Execute(ctx, prepared.credential, prepared.executeRequest())
	if errExecute != nil {
		return nil, errorToPluginError(errExecute)
	}
	return okEnvelope(response)
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

// rewritePayloadModel 把请求体里的 model 字段改写成官方 ID。
//
// 解析失败时原样返回：宁可不改写，也不要让一次对话因为本地 JSON 解析失败而中断——
// 上游的真实报错比本地解析错误更有诊断价值（与 workbuddy.PrepareBody 同口径）。
func rewritePayloadModel(payload []byte, model string) []byte {
	if len(payload) == 0 || strings.TrimSpace(model) == "" {
		return payload
	}
	var obj map[string]any
	if errUnmarshal := json.Unmarshal(payload, &obj); errUnmarshal != nil || obj == nil {
		return payload
	}
	obj["model"] = model
	encoded, errMarshal := json.Marshal(obj)
	if errMarshal != nil {
		return payload
	}
	return encoded
}

// runStreamExecution 在后台把上游 SSE 转发给宿主流。
func runStreamExecution(streamCtx context.Context, cancelStream context.CancelFunc, prepared preparedExecution) {
	ctx := httpx.WithCallbackID(streamCtx, prepared.rpc.HostCallbackID)
	streamID := prepared.rpc.StreamID
	callbackID := prepared.rpc.HostCallbackID

	sink := &hostStreamSink{
		callbackID:   callbackID,
		streamID:     streamID,
		cancelStream: cancelStream,
	}
	errForward := prepared.vendor.ExecuteStream(ctx, prepared.credential, prepared.executeRequest(), sink)
	// 下游主动断开会取消上下文，那不是错误，不该发错误帧给客户端。
	if errForward != nil && streamCtx.Err() == nil {
		closeStream(callbackID, streamID, streamFailureMessage(errForward))
		return
	}
	closeStream(callbackID, streamID, "")
}

// hostStreamSink 把宿主的流式投递能力包装成 core.StreamSink。
//
// 供应商只负责产出分片内容，投递（host.stream.emit）与收尾由根层统一做——
// 这段逻辑宿主契约相关，不该让每个供应商各写一遍。
type hostStreamSink struct {
	callbackID   string
	streamID     string
	cancelStream context.CancelFunc
}

// Emit 投递一个分片（确保符合 SSE 传输协议：以 data: 开头，以 \n\n 结尾）。
func (s *hostStreamSink) Emit(payload []byte) error {
	frame := formatSSEChunk(payload)
	if len(frame) == 0 {
		return nil
	}
	if errEmit := emitStreamChunk(s.callbackID, s.streamID, frame); errEmit != nil {
		// 下游断开：取消上游读取，静默收尾（这不是错误）。
		s.cancelStream()
		return errEmit
	}
	return nil
}

// formatSSEChunk 确保分片符合标准 SSE 格式（data: <json>\n\n），
// 满足宿主转换器（如 ConvertOpenAIResponseToClaude）对 data: 前缀的校验要求。
func formatSSEChunk(payload []byte) []byte {
	trimmed := bytes.TrimRight(payload, "\r\n ")
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) ||
		bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte(":")) {
		return append(trimmed, '\n', '\n')
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return []byte("data: [DONE]\n\n")
	}
	out := make([]byte, 0, len(trimmed)+8)
	out = append(out, []byte("data: ")...)
	out = append(out, trimmed...)
	out = append(out, '\n', '\n')
	return out
}

// Close 结束流。错误帧由调用方在拿到 ExecuteStream 的返回值后统一发，
// 这里不重复发——同一条流发两次关闭会让客户端收到重复的结束事件。
func (s *hostStreamSink) Close(errMsg string) error { return nil }

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
	credential, vendor, errResolve := resolveVendorCredential(ctx, prepared.rpc.HostCallbackID,
		prepared.rpc.StorageJSON, prepared.rpc.AuthID, prepared.rpc.AuthAttributes)
	if errResolve != nil {
		return prepared, errResolve
	}
	prepared.vendor = vendor
	prepared.credential = credential

	// 模型带的供应商前缀优先；无前缀时就用裸名（宿主按 EqualFold 合并同名模型）。
	_, bareModel := core.SplitModelID(prepared.rpc.Model)
	if strings.TrimSpace(bareModel) == "" {
		return prepared, newPluginError("invalid_request", "model is required", http.StatusBadRequest)
	}

	// 客户端请求的是别名时还原成上游官方 ID：禁用校验与出站都用官方 ID，
	// 因此还原必须发生在两者之前。
	prepared.model = resolveOutboundModel(vendor.ID(), bareModel)
	prepared.modelAliased = prepared.model != bareModel

	// 检查模型是否已被插件配置禁用（禁用键是 vendor:model，跨供应商互不影响）。
	// 用官方 ID 判定：管理页展示与写入的禁用键都是官方 ID，别名只是对外名字。
	if isModelDisabled(vendor.ID(), prepared.model) {
		return prepared, newPluginError("model_disabled",
			fmt.Sprintf("model %q is disabled by plugin configuration", prepared.rpc.Model),
			http.StatusBadRequest)
	}

	// 供应商所在的区域被配置关闭时直接拒绝（而不是让请求打到错误的上游域名）。
	if !realmEnabled(loadedConfig(), vendor.Region()) {
		return prepared, newPluginError("vendor_disabled",
			fmt.Sprintf("vendor %s is disabled by plugin configuration", vendor.ID()), http.StatusBadRequest)
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
func chatMetaFor(rpc executorRPCRequest) workbuddy.ChatMeta {
	return chatMetaFromBody(rpc.Payload, rpc.Headers)
}

// chatMetaForExecuteRequest 从 vendor 适配层的执行请求派生会话头族。
//
// 与 chatMetaFor 同源，只是入参形态不同：适配层拿到的是已解好的
// core.ExecuteRequest，不需要再经 executorRPCRequest。
func chatMetaForExecuteRequest(req *core.ExecuteRequest) workbuddy.ChatMeta {
	if req == nil {
		return workbuddy.ChatMeta{}
	}
	return chatMetaFromBody(req.Payload, http.Header(req.Headers))
}

// chatMetaFromBody 从请求体与请求头派生会话头族。
//
// 会话键从请求体里提取（客户端可能指定了 conversation/message id），
// 缺失时由 turnKey 兜底——上游要求 ConversationRequestID 必发。
func chatMetaFromBody(body []byte, headers http.Header) workbuddy.ChatMeta {
	conversationKey := workbuddy.ConversationKey(body)
	turnKey := workbuddy.TurnKey(body)

	// 入站透传的链路 ID 优先（调用方可能已经建好了链路）。
	traceID := ""
	if headers != nil {
		traceID = strings.TrimSpace(headers.Get("X-Trace-ID"))
	}

	conversationRequestID := ""
	switch {
	case conversationKey != "" && turnKey != "":
		conversationRequestID = workbuddy.TurnRequestID(conversationKey + ":" + turnKey)
	case turnKey != "":
		conversationRequestID = workbuddy.TurnRequestID(turnKey)
	case conversationKey != "":
		conversationRequestID = workbuddy.RequestIDForKey(conversationKey)
	default:
		conversationRequestID = workbuddy.TurnRequestID("")
	}

	return workbuddy.ChatMeta{
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
	var upstreamErr *workbuddy.Error
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
		if workbuddy.HasImagePart(prepared.rpc.Payload) && !preparedSupportsImages(prepared) {
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

// preparedSupportsImages 报告本次执行的模型是否支持图片输入。
//
// 查上游模型缓存；查不到时保守返回 true（放行总比误拒好——上游会对
// 不支持的模型给出明确错误，而误拒会让正常请求直接失败）。
func preparedSupportsImages(prepared preparedExecution) bool {
	for _, model := range workbuddy.CachedModels()[workbuddy.NormalizeRegion(prepared.vendor.Region())] {
		if model.ID == prepared.model {
			return model.SupportsImages
		}
	}
	return true
}
