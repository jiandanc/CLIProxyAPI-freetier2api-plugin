// Package httpx 把插件内的出站 HTTP 请求接到 CPA 宿主的 HTTP 桥上。
//
// 为什么必须走宿主桥而不是自己发请求：
//   - 宿主的全局代理、账号级代理、出站请求日志、API 统计都挂在这条链路上；
//   - 插件直接 net/http 出站会绕过以上全部，表现为"代理配了没用""请求日志里看不到"。
//
// 本包提供两个 *http.Client：
//   - Client：经 host.http.do，一次性拿到完整响应（模型清单、额度、签到等短请求）；
//   - StreamClient：经 host.http.do_stream，响应体按需从 host.http.stream_read 拉取（SSE 对话）。
//
// 对 internal 包的依赖方向：本包不知道宿存在，宿主调用能力由 package main 在
// init 里通过 Configure 注入（避免 internal 反向 import main）。
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api-plugin/cpasdk/pluginabi"
	"workbuddy2api-plugin/cpasdk/pluginapi"
)

// HostCaller 是宿主回调实现，由 package main 注入。
type HostCaller func(callbackID, method string, payload any) (json.RawMessage, error)

var (
	hostCallerMu sync.RWMutex
	hostCaller   HostCaller
)

// Configure 注入宿主回调实现。由 package main 的 init 调用一次。
func Configure(caller HostCaller) {
	hostCallerMu.Lock()
	hostCaller = caller
	hostCallerMu.Unlock()
}

func currentHostCaller() (HostCaller, error) {
	hostCallerMu.RLock()
	caller := hostCaller
	hostCallerMu.RUnlock()
	if caller == nil {
		return nil, errors.New("host caller is not configured")
	}
	return caller, nil
}

type callbackIDKey struct{}

// WithCallbackID 把宿主回调 ID 绑定到 context，后续出站请求会带上它。
//
// 绑定的意义：宿主用它把上游请求/响应关联回原始请求，并在请求结束时回收流资源。
// 后台任务（签到、任务上报）没有请求作用域，传空串即可。
func WithCallbackID(ctx context.Context, callbackID string) context.Context {
	trimmed := strings.TrimSpace(callbackID)
	if trimmed == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callbackIDKey{}, trimmed)
}

func callbackIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(callbackIDKey{}).(string)
	return id
}

// emptyReadBackoff 是宿主返回空分片时的让出间隔，避免桥接异常时读取循环空转。
const emptyReadBackoff = time.Millisecond

// Client 返回经宿主 host.http.do 出站的客户端。
func Client(ctx context.Context, timeout time.Duration) *http.Client {
	return &http.Client{Transport: &transport{ctx: ctx}, Timeout: timeout}
}

// StreamClient 返回经宿主 host.http.do_stream 出站的客户端（用于 SSE）。
//
// 超时设为 0：流式响应的总时长不可预期，首字节与空闲超时分别由上游
// ResponseHeaderTimeout 与 idle 监控负责（见 cb 包的 idle 处理）。
func StreamClient(ctx context.Context, timeout time.Duration) *http.Client {
	return &http.Client{Transport: &transport{ctx: ctx, stream: true}, Timeout: timeout}
}

type transport struct {
	ctx    context.Context
	stream bool
}

// RoundTrip 实现 http.RoundTripper：把请求转成宿主 HTTP 桥调用。
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if t.ctx != nil {
		ctx = t.ctx
	}
	caller, errCaller := currentHostCaller()
	if errCaller != nil {
		return nil, errCaller
	}

	body, errBody := readRequestBody(req)
	if errBody != nil {
		return nil, errBody
	}

	payload := rpcHostHTTPRequest{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: req.Header,
		Body:    body,
	}
	method := pluginabi.MethodHostHTTPDo
	if t.stream {
		method = pluginabi.MethodHostHTTPDoStream
	}
	raw, errCall := caller(callbackIDFrom(ctx), method, payload)
	if errCall != nil {
		return nil, errCall
	}

	if !t.stream {
		var rpc rpcHostHTTPResponse
		if errUnmarshal := json.Unmarshal(raw, &rpc); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host http response: %w", errUnmarshal)
		}
		if !rpc.statusSeen {
			// 不能静默当 0：那会让上游 200 也被判成失败，而错误体里又带着真实响应，
			// 看起来像"凭证被拒"，极难排查。宁可直接报协议不匹配。
			return nil, fmt.Errorf("host http response has no status field (host ABI mismatch): %s", truncateForError(raw))
		}
		return buildResponse(req, rpc.StatusCode, rpc.Headers, io.NopCloser(bytes.NewReader(rpc.Body))), nil
	}

	var rpc rpcHostHTTPStreamResponse
	if errUnmarshal := json.Unmarshal(raw, &rpc); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http stream response: %w", errUnmarshal)
	}
	if rpc.StatusCode == 0 {
		return nil, fmt.Errorf("host http stream response has no status field (host ABI mismatch): %s", truncateForError(raw))
	}
	streamBody := &streamBody{
		ctx:        ctx,
		caller:     caller,
		callbackID: callbackIDFrom(ctx),
		streamID:   rpc.StreamID,
	}
	return buildResponse(req, rpc.StatusCode, rpc.Headers, streamBody), nil
}

func readRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	body, errRead := io.ReadAll(req.Body)
	if errClose := req.Body.Close(); errClose != nil && errRead == nil {
		return nil, fmt.Errorf("close request body: %w", errClose)
	}
	if errRead != nil {
		return nil, fmt.Errorf("read request body: %w", errRead)
	}
	return body, nil
}

func buildResponse(req *http.Request, status int, header http.Header, body io.ReadCloser) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          body,
		ContentLength: -1,
		Request:       req,
	}
}

// rpcHostHTTPRequest 是 host.http.do / do_stream 的请求体。
//
// 只填扁平字段（method/url/headers/body）：宿主对扁平与嵌套两种形态都接受，
// 扁平形态更简单且不容易漏字段。
type rpcHostHTTPRequest struct {
	Method  string      `json:"method,omitempty"`
	URL     string      `json:"url,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

// rpcHostHTTPResponse 是 host.http.do 的响应体。
//
// 关键：宿主的 host.http.do 直接把 pluginapi.HTTPResponse 丢进信封，而该结构体
// **没有 json tag**，因此线上键名是 Go 字段名（StatusCode / Headers / Body）。
// 但宿主少数地方会走带 tag 的中间结构体（status_code / headers / body）。
// 只认一种的话状态码会静默变成 0，后果是所有非流式上游请求全部被判成失败。
// 因此这里手写 UnmarshalJSON 同时接受两种形态。
type rpcHostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	// statusSeen 标记是否真的解到了状态码。
	// 用「见过」而不是「缺失」是为了让零值语义正确：结构体零值即「没见过」。
	statusSeen bool
}

// UnmarshalJSON 兼容宿主两种序列化形态（Go 字段名 / snake_case）。
func (r *rpcHostHTTPResponse) UnmarshalJSON(data []byte) error {
	if r == nil {
		return errors.New("nil host http response")
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &fields); errUnmarshal != nil {
		return errUnmarshal
	}
	for key, value := range fields {
		switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
		case "statuscode":
			r.statusSeen = true
			if errDecode := json.Unmarshal(value, &r.StatusCode); errDecode != nil {
				return fmt.Errorf("decode host http status: %w", errDecode)
			}
		case "headers":
			if errDecode := json.Unmarshal(value, &r.Headers); errDecode != nil {
				return fmt.Errorf("decode host http headers: %w", errDecode)
			}
		case "body":
			if errDecode := json.Unmarshal(value, &r.Body); errDecode != nil {
				return fmt.Errorf("decode host http body: %w", errDecode)
			}
		}
	}
	return nil
}

// rpcHostHTTPStreamResponse 是 host.http.do_stream 的响应体。
// 这个结构体在宿主侧**有** json tag，键名是 snake_case。
type rpcHostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type rpcHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type rpcHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type rpcHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

// streamBody 实现 io.ReadCloser，按需从 host.http.stream_read 拉取上游分片。
//
// 与宿主的约定：宿主返回 done=true 表示流结束；error 非空表示流错误。
// 本类型负责把这两者翻译成 io.EOF 与 error。
type streamBody struct {
	ctx        context.Context
	caller     HostCaller
	callbackID string
	streamID   string

	mu       sync.Mutex
	closed   bool
	closeErr error
	pending  []byte
}

// Read 从上游流读取数据。每次调用向宿主索要一个分片。
func (b *streamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		b.mu.Unlock()
		return n, nil
	}
	if b.closed {
		errClosed := b.closeErr
		b.mu.Unlock()
		if errClosed != nil {
			return 0, errClosed
		}
		return 0, io.EOF
	}
	b.mu.Unlock()

	chunk, errRead := b.readChunk()
	if errRead != nil {
		// 读失败即终结：标记关闭并关闭宿主侧流，避免泄漏。
		_ = b.Close()
		return 0, errRead
	}
	if len(chunk) == 0 {
		return 0, nil
	}
	n := copy(p, chunk)
	if n < len(chunk) {
		b.mu.Lock()
		b.pending = append(b.pending, chunk[n:]...)
		b.mu.Unlock()
	}
	return n, nil
}

// readChunk 向宿主索要一个分片。空分片时短暂让出，避免空转。
func (b *streamBody) readChunk() ([]byte, error) {
	if strings.TrimSpace(b.streamID) == "" {
		return nil, errors.New("host http stream id is missing")
	}
	for {
		select {
		case <-b.ctx.Done():
			return nil, b.ctx.Err()
		default:
		}
		raw, errCall := b.caller(b.callbackID, pluginabi.MethodHostHTTPStreamRead, rpcHostHTTPStreamReadRequest{StreamID: b.streamID})
		if errCall != nil {
			return nil, fmt.Errorf("host http stream read: %w", errCall)
		}
		var chunk rpcHostHTTPStreamReadResponse
		if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host http stream chunk: %w", errUnmarshal)
		}
		if chunk.Error != "" {
			return nil, errors.New(chunk.Error)
		}
		if chunk.Done {
			return nil, io.EOF
		}
		if len(chunk.Payload) > 0 {
			return chunk.Payload, nil
		}
		// 空分片：让出 CPU 后重试，直到有数据、结束或 context 取消。
		select {
		case <-b.ctx.Done():
			return nil, b.ctx.Err()
		case <-time.After(emptyReadBackoff):
		}
	}
}

// Close 关闭宿主侧的上游流。幂等：只会真正调用一次 host.http.stream_close。
func (b *streamBody) Close() error {
	b.mu.Lock()
	if b.closed {
		errClosed := b.closeErr
		b.mu.Unlock()
		return errClosed
	}
	b.closed = true
	b.mu.Unlock()

	if strings.TrimSpace(b.streamID) == "" {
		return nil
	}
	_, errCall := b.caller(b.callbackID, pluginabi.MethodHostHTTPStreamClose, rpcHostHTTPStreamCloseRequest{StreamID: b.streamID})
	if errCall != nil {
		errWrapped := fmt.Errorf("host http stream close: %w", errCall)
		b.mu.Lock()
		b.closeErr = errWrapped
		b.mu.Unlock()
		return errWrapped
	}
	return nil
}

// truncateForError 截断用于错误信息的响应体，避免把整页 HTML 塞进日志。
func truncateForError(raw []byte) string {
	const limit = 256
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "..."
}

// compile-time 断言：确保宿主 SDK 的类型仍存在（ABI 漂移时编译期即可发现）。
var _ = pluginapi.HTTPRequest{}
