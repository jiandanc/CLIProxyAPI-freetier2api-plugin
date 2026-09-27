package main

// 本文件提供「假宿主」：把宿主回调替换成内存实现，
// 使插件的完整链路（凭证解析、模型注册、出站 HTTP、任务上报、流式转发）
// 都能在没有 CPA 进程的情况下被测试。
//
// 关键设计：
//   - installFakeHost 用 t.Cleanup 做完整还原（含把重试退避压到毫秒级）；
//   - resetPluginGlobals 集中清包级全局——白盒测试最大的坑是缓存跨用例泄漏；
//   - 非流式响应刻意按宿主的「无 tag」形态返回，覆盖 rpcHostHTTPResponse 的兼容逻辑。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api-plugin/cpasdk/pluginabi"
	"workbuddy2api-plugin/cpasdk/pluginapi"
	"workbuddy2api-plugin/internal/cb"
)

// testCallbackID 是测试用的宿主回调 ID。
const testCallbackID = "test-callback"

// fakeUpstreamResponse 是假宿主对一次出站请求的应答。
type fakeUpstreamResponse struct {
	Status int
	Header map[string][]string
	Body   string
}

// fakeStream 是一次被模拟的流式响应。
type fakeStream struct {
	chunks   []string
	position int
	closed   bool
}

// fakeHost 是宿主回调的内存实现。
type fakeHost struct {
	mu sync.Mutex

	// calls 记录被调用的宿主方法名（供断言）。
	calls []string
	// authFiles 是 host.auth.list 的返回内容。
	authFiles []hostAuthEntry
	// authJSON 是 auth_index → 凭证 JSON。
	authJSON map[string]string
	// upstream 是出站请求的应答函数（按 method + url 匹配）。
	upstream func(method, url, body string) fakeUpstreamResponse

	streams    map[string]*fakeStream
	nextStream int

	// 流式转发的记录。
	emits     []string
	streamErr []string
	closed    []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		authJSON: map[string]string{},
		streams:  map[string]*fakeStream{},
		upstream: func(string, string, string) fakeUpstreamResponse {
			return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404,"msg":"not stubbed"}`}
		},
	}
}

// call 实现宿主回调接口。
func (h *fakeHost) call(callbackID, method string, payload any) (json.RawMessage, error) {
	h.mu.Lock()
	h.calls = append(h.calls, method)
	h.mu.Unlock()

	switch method {
	case pluginabi.MethodHostHTTPDo:
		return h.handleHTTPDo(payload)
	case pluginabi.MethodHostHTTPDoStream:
		return h.handleHTTPDoStream(payload)
	case pluginabi.MethodHostHTTPStreamRead:
		return h.handleStreamRead(payload)
	case pluginabi.MethodHostHTTPStreamClose:
		return h.handleStreamClose(payload)
	case pluginabi.MethodHostAuthList:
		return marshalJSON(map[string]any{"auths": h.snapshotAuthFiles()})
	case pluginabi.MethodHostAuthGet:
		return h.handleAuthGet(payload)
	case pluginabi.MethodHostStreamEmit:
		return h.handleStreamEmit(payload)
	case pluginabi.MethodHostStreamClose:
		return h.handleStreamClosePayload(payload)
	}
	return nil, fmt.Errorf("fake host does not implement %s", method)
}

func (h *fakeHost) snapshotAuthFiles() []hostAuthEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]hostAuthEntry, len(h.authFiles))
	copy(out, h.authFiles)
	return out
}

// handleHTTPDo 处理非流式出站请求。
func (h *fakeHost) handleHTTPDo(payload any) (json.RawMessage, error) {
	req := decodeFakeRequest(payload)
	response := h.upstream(req.method, req.url, string(req.body))
	// 与真实宿主同形：host.http.do 回的是**无 json tag** 的 pluginapi.HTTPResponse。
	return marshalJSON(map[string]any{
		"StatusCode": response.Status,
		"Headers":    response.Header,
		"Body":       []byte(response.Body),
	})
}

// handleHTTPDoStream 处理流式出站请求。
func (h *fakeHost) handleHTTPDoStream(payload any) (json.RawMessage, error) {
	req := decodeFakeRequest(payload)
	response := h.upstream(req.method, req.url, string(req.body))

	h.mu.Lock()
	h.nextStream++
	streamID := fmt.Sprintf("stream-%d", h.nextStream)
	h.streams[streamID] = &fakeStream{chunks: splitChunks(response.Body)}
	h.mu.Unlock()

	return marshalJSON(map[string]any{
		"status_code": response.Status,
		"headers":     response.Header,
		"stream_id":   streamID,
	})
}

// handleStreamRead 返回流的下一个分片。
func (h *fakeHost) handleStreamRead(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	stream, okStream := h.streams[req.StreamID]
	if !okStream {
		h.mu.Unlock()
		return marshalJSON(map[string]any{"done": true})
	}
	if stream.position >= len(stream.chunks) {
		delete(h.streams, req.StreamID)
		h.mu.Unlock()
		return marshalJSON(map[string]any{"done": true})
	}
	chunk := stream.chunks[stream.position]
	stream.position++
	h.mu.Unlock()
	return marshalJSON(map[string]any{"payload": []byte(chunk)})
}

// handleStreamClose 关闭上游流。
func (h *fakeHost) handleStreamClose(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	if stream, okStream := h.streams[req.StreamID]; okStream {
		stream.closed = true
	}
	delete(h.streams, req.StreamID)
	h.mu.Unlock()
	return marshalJSON(map[string]any{})
}

// handleAuthGet 按 auth_index 返回凭证 JSON。
func (h *fakeHost) handleAuthGet(payload any) (json.RawMessage, error) {
	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	raw, okRaw := h.authJSON[req.AuthIndex]
	h.mu.Unlock()
	if !okRaw {
		return json.RawMessage(`{"auth_index":"` + req.AuthIndex + `","json":null}`), nil
	}
	// 真实宿主的 HostAuthGetResponse.JSON 是 json.RawMessage：原样内联，
	// 不能走 []byte 序列化（那会变成 base64，插件侧解不出来）。
	return json.RawMessage(`{"auth_index":"` + req.AuthIndex + `","json":` + raw + `}`), nil
}

// handleStreamEmit 记录一次流分片投递。
func (h *fakeHost) handleStreamEmit(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	h.emits = append(h.emits, string(req.Payload))
	h.mu.Unlock()
	return marshalJSON(map[string]any{})
}

// handleStreamClosePayload 记录一次宿主流关闭。
func (h *fakeHost) handleStreamClosePayload(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
		Error    string `json:"error"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	h.closed = append(h.closed, req.StreamID)
	if req.Error != "" {
		h.streamErr = append(h.streamErr, req.Error)
	}
	h.mu.Unlock()
	return marshalJSON(map[string]any{})
}

// emittedChunks 返回已投递的流分片。
func (h *fakeHost) emittedChunks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.emits))
	copy(out, h.emits)
	return out
}

// streamErrors 返回流关闭时上报的错误。
func (h *fakeHost) streamErrors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.streamErr))
	copy(out, h.streamErr)
	return out
}

// callCount 统计某个宿主方法被调用的次数。
func (h *fakeHost) callCount(method string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, name := range h.calls {
		if name == method {
			count++
		}
	}
	return count
}

// addAccount 注册一个假账号。
func (h *fakeHost) addAccount(authIndex, name, credentialJSON string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authJSON[authIndex] = credentialJSON
	h.authFiles = append(h.authFiles, hostAuthEntry{
		ID: name, AuthIndex: authIndex, Name: name, Provider: providerKey, Label: name,
	})
}

// setUpstream 设置出站请求的应答。
func (h *fakeHost) setUpstream(fn func(method, url, body string) fakeUpstreamResponse) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.upstream = fn
}

// fakeRequest 是解码后的出站请求。
type fakeRequest struct {
	method string
	url    string
	body   []byte
}

// decodeFakeRequest 从载荷里取出请求要素。
func decodeFakeRequest(payload any) fakeRequest {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(mustMarshal(payload), &fields)

	out := fakeRequest{}
	if raw, okRaw := fields["method"]; okRaw {
		_ = json.Unmarshal(raw, &out.method)
	}
	if raw, okRaw := fields["url"]; okRaw {
		_ = json.Unmarshal(raw, &out.url)
	}
	if raw, okRaw := fields["body"]; okRaw {
		_ = json.Unmarshal(raw, &out.body)
	}
	return out
}

// installFakeHost 把假宿主接到插件的宿主调用入口上。
func installFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	host := newFakeHost()
	previous := hostCallScopedImpl
	hostCallScopedImpl = host.call
	resetPluginGlobals(t)
	t.Cleanup(func() {
		hostCallScopedImpl = previous
		resetPluginGlobals(t)
	})
	return host
}

// resetPluginGlobals 清掉跨用例共享的进程级状态。
//
// 白盒测试最容易踩的坑：某个用例留下的缓存/单例让后续用例看到过期数据。
// 因此把所有包级全局集中在这里清一遍。
func resetPluginGlobals(t *testing.T) {
	t.Helper()

	pluginLifecycleMu.Lock()
	pluginRegistered = false
	pluginLifecycleMu.Unlock()

	hostCallMu.Lock()
	hostCallShuttingDown = false
	hostCallMu.Unlock()

	stateMu.Lock()
	stateCache = nil
	stateMu.Unlock()
	reloadDisabledModelCache(nil)

	resetCredentialCache()
	// 模型目录缓存是包级的：不重置会让上一个用例的探测结果泄漏到下一个。
	cb.ResetModelCacheForTest()

	loginStoreMu.Lock()
	loginStore = map[string]*pendingLogin{}
	loginStoreMu.Unlock()
	loginPollMinGap = loginPollInterval

	currentQueue.mu.Lock()
	currentQueue.running = false
	currentQueue.items = nil
	currentQueue.seq = 0
	currentQueue.mu.Unlock()

	taskLockMu.Lock()
	taskLocks = map[string]*sync.Mutex{}
	taskLockMu.Unlock()

	// 时间函数复位（个别用例会注入固定时间）。
	nowFunc = time.Now
}

// setupTestPlugin 准备一个使用临时状态目录的插件实例。
func setupTestPlugin(t *testing.T, mutate ...func(*pluginConfig)) pluginConfig {
	t.Helper()
	cfg := defaultPluginConfig()
	cfg.StateDir = t.TempDir()
	cfg.LogLevel = "error"
	cfg.LogToFile = false
	for _, apply := range mutate {
		apply(&cfg)
	}
	if errApply := applyConfig(cfg); errApply != nil {
		t.Fatalf("apply config: %v", errApply)
	}
	if _, errState := loadState(cfg); errState != nil {
		t.Fatalf("load state: %v", errState)
	}
	return cfg
}

// buildConfigYAML 生成宿主会传给 plugin.register 的配置 YAML。
//
// 包含宿主追加的 enabled / priority 两键，验证插件对未知字段的容忍。
func buildConfigYAML(cfg pluginConfig) []byte {
	var builder strings.Builder
	builder.WriteString("enabled: true\npriority: 1\n")
	fmt.Fprintf(&builder, "state_dir: %q\n", cfg.StateDir)
	fmt.Fprintf(&builder, "log_level: %s\n", cfg.LogLevel)
	fmt.Fprintf(&builder, "enabled_realms: %s\n", strings.Join(cfg.EnabledRealms, ","))
	if cfg.PromptMode != "" {
		fmt.Fprintf(&builder, "prompt_mode: %s\n", cfg.PromptMode)
	}
	fmt.Fprintf(&builder, "auto_checkin_at: %q\n", cfg.AutoCheckinAt)
	return []byte(builder.String())
}

// registerRequest 构造 plugin.register 的请求体。
func registerRequest(t *testing.T, cfg pluginConfig) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: buildConfigYAML(cfg)})
	if errMarshal != nil {
		t.Fatalf("marshal register request: %v", errMarshal)
	}
	return raw
}

// sampleCredentialJSON 构造一份凭证 JSON（嵌套形态）。
func sampleCredentialJSON(realm, uid string) string {
	region := cb.NormalizeRegion(realm)
	return fmt.Sprintf(`{
  "auth": {"accessToken": "at-%s", "refreshToken": "rt-%s", "expiresAt": %d, "domain": "%s", "realm": "%s"},
  "account": {"uid": "%s", "enterpriseId": "ent-1", "nickname": "账号%s"},
  "device_token": "dt-%s"
}`, uid, uid, time.Now().Add(24*time.Hour).Unix(), defaultDomainFor(region), region, uid, uid, uid)
}

// 小工具。

func mustMarshal(value any) []byte {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return raw
}

func marshalJSON(value any) (json.RawMessage, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return raw, nil
}

// splitChunks 把响应体切成小块，模拟宿主 32KB 分片读取的真实路径。
//
// 刻意不一次性喂完整正文：那会掩盖分片边界相关的 bug。
func splitChunks(body string) []string {
	if body == "" {
		return nil
	}
	const size = 17
	chunks := make([]string, 0, len(body)/size+1)
	for len(body) > 0 {
		take := size
		if take > len(body) {
			take = len(body)
		}
		chunks = append(chunks, body[:take])
		body = body[take:]
	}
	return chunks
}

// decodeEnvelopeResultForTest 解开插件响应信封（测试辅助）。
func decodeEnvelopeResultForTest(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	if !env.OK {
		message := "unknown"
		if env.Error != nil {
			message = env.Error.Code + ": " + env.Error.Message
		}
		t.Fatalf("envelope is not ok: %s", message)
	}
	return env.Result
}

// callMethod 调用一个 ABI 方法并解开结果。
func callMethod(t *testing.T, method string, payload any) json.RawMessage {
	t.Helper()
	var request []byte
	if payload != nil {
		request = mustMarshal(payload)
	}
	raw, errHandle := handleMethod(method, request)
	if errHandle != nil {
		t.Fatalf("%s: %v", method, errHandle)
	}
	return decodeEnvelopeResultForTest(t, raw)
}

// waitFor 轮询等待条件成立（避免用固定 sleep 造成偶发失败）。
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return condition()
}

// pluginAPI 的引用保证测试文件与生产代码用同一份契约类型。
var _ = pluginapi.Metadata{}
