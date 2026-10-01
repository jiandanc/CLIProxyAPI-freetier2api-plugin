package main

// 本文件实现 auth.login 的 ABI 分发：把宿主的登录请求路由到对应的供应商。
//
// 登录协议本身**由供应商实现**（各自的 login.go）：
//   - WorkBuddy 是自有 state/token 三接口（internal/vendors/workbuddy/login.go）；
//   - Qoder 是标准 PKCE 设备码流（internal/vendors/qoder/login.go）；
//   - Cline 是 WorkOS OAuth 2.0 设备码（internal/vendors/cline/login.go）；
//   - OpenCode ZEN 没有登录（纯 API key）。
//
// 根层只负责：从 Metadata 解析目标供应商、校验区域开关、把会话路由回去。
// 会话是供应商自己建的，id 只存在于它的状态表里，根层不参与。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/logger"
)

const (
	// loginHTTPTimeout 是单次登录相关请求的上限。
	loginHTTPTimeout = 30 * time.Second
)

// loginStartRPCRequest 与宿主的 auth.login.start 请求对齐。
type loginStartRPCRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// loginPollRPCRequest 与宿主的 auth.login.poll 请求对齐。
type loginPollRPCRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthLoginStart 开始一次登录，返回用户需要打开的授权链接。
//
// 登录协议由供应商实现，根层只做供应商路由与区域开关校验。
func handleAuthLoginStart(request []byte) ([]byte, error) {
	var rpc loginStartRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	vendor, errVendor := vendorForLogin(rpc.Metadata)
	if errVendor != nil {
		return nil, errVendor
	}

	ctx, cancel := context.WithTimeout(httpx.WithCallbackID(context.Background(), rpc.HostCallbackID), loginHTTPTimeout)
	defer cancel()

	response, errStart := vendor.LoginStart(ctx, rpc.Metadata)
	if errStart != nil {
		return nil, errorToPluginError(errStart)
	}
	// 供应商标识由根层统一注入，不交给各供应商自己记着。
	//
	// 宿主靠 metadata.vendor 把登录会话绑定回具体供应商；缺了它宿主拿不到
	// 授权链接，页面只会看到「获取授权链接失败」。把这一步放在分发点而不是
	// 各供应商的 LoginStart 里：新增供应商时没人会记得补，且漏掉时没有任何
	// 编译期或运行期提示——本插件曾因此让 codearts / trae / traesolo 三个
	// 供应商长期无法添加账号。
	if response != nil {
		if response.Metadata == nil {
			response.Metadata = map[string]any{}
		}
		response.Metadata[core.VendorKey] = vendor.ID()
		response.Metadata["region"] = vendor.Region()
	}
	logger.Info("started login (vendor=%s)", vendor.ID())
	return okEnvelope(response)
}

// handleAuthLoginPoll 轮询一次登录状态。
//
// 会话状态由供应商自己持有（各自的 state 结构不同），根层不参与。
func handleAuthLoginPoll(request []byte) ([]byte, error) {
	var rpc loginPollRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, newPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	sessionID := strings.TrimSpace(rpc.State)
	if sessionID == "" {
		return nil, newPluginError("invalid_request", "login state is required", http.StatusBadRequest)
	}
	vendor, errVendor := vendorForLoginSession(sessionID)
	if errVendor != nil {
		return nil, errVendor
	}

	ctx, cancel := context.WithTimeout(httpx.WithCallbackID(context.Background(), rpc.HostCallbackID), loginHTTPTimeout)
	defer cancel()

	response, errPoll := vendor.LoginPoll(ctx, sessionID)
	if errPoll != nil {
		return nil, errorToPluginError(errPoll)
	}
	return okEnvelope(response)
}

// loginSessionOwner 由持有登录会话状态的供应商实现。
//
// 会话是供应商自己建的，id 只存在于它的状态表里；根层要判断「这个会话
// 属于谁」只能问供应商。做成可选接口而不是塞进 core.Vendor：只有需要
// 多步登录的供应商才关心它，其它实现不必被迫实现一个空方法。
type loginSessionOwner interface {
	// OwnsLoginSession 报告该会话是否由本供应商创建且仍有效。
	OwnsLoginSession(sessionID string) bool
}

// vendorForLogin 从登录请求的 Metadata 解析目标供应商。
//
// 控制台页把用户选的供应商标识放在 Metadata 的 vendor 字段里；
// 缺失时回落到配置里启用的第一个供应商（兼容旧版页面只传 realm 的情况）。
func vendorForLogin(metadata map[string]any) (core.Vendor, error) {
	if metadata != nil {
		if rawVendor, okVendor := metadata[core.VendorKey].(string); okVendor {
			vendor, okLookup := core.VendorByID(rawVendor)
			if !okLookup {
				return nil, newPluginError("invalid_request",
					fmt.Sprintf("unknown vendor %q", rawVendor), http.StatusBadRequest)
			}
			if !vendorEnabled(vendor) {
				return nil, newPluginError("vendor_disabled",
					fmt.Sprintf("vendor %s is disabled by plugin configuration", vendor.ID()), http.StatusBadRequest)
			}
			return vendor, nil
		}
		// 旧版页面只传 realm：按区域找第一个匹配的供应商。
		if rawRegion, okRegion := metadata["region"].(string); okRegion && strings.TrimSpace(rawRegion) != "" {
			if vendor, okFind := firstVendorForRegion(rawRegion); okFind {
				return vendor, nil
			}
		}
	}
	if vendor, okDefault := firstEnabledVendor(); okDefault {
		return vendor, nil
	}
	return nil, newPluginError("vendor_disabled", "no vendor is enabled by plugin configuration", http.StatusBadRequest)
}

// vendorForLoginSession 按登录会话找供应商。
//
// 会话是供应商自己建的（状态存在各自的包里），因此这里只能遍历询问。
// 会话数很少（同时最多一两个登录在进行），遍历代价可忽略。
func vendorForLoginSession(sessionID string) (core.Vendor, error) {
	// 会话存在哪个供应商里由对方的 LoginPoll 自证（会话 id 只在对方的状态表里）。
	// 首先按供应商主动认领匹配：
	for _, vendor := range core.Vendors() {
		if !vendorEnabled(vendor) {
			continue
		}
		if provider, okProvider := vendor.(loginSessionOwner); okProvider && provider.OwnsLoginSession(sessionID) {
			return vendor, nil
		}
	}
	// 备用机制：按 sessionID 的前缀特征路由到对应的供应商，
	// 防止由于状态清理竞争或时序差异把特定供应商的轮询发给无关的供应商。
	normSession := strings.ToLower(strings.TrimSpace(sessionID))
	for _, vendor := range core.Vendors() {
		if !vendorEnabled(vendor) {
			continue
		}
		vid := strings.ToLower(vendor.ID())
		vprefix1 := vid + "_"
		vprefix2 := strings.ReplaceAll(vid, "-", "_") + "_"
		if strings.HasPrefix(normSession, vprefix1) || strings.HasPrefix(normSession, vprefix2) {
			return vendor, nil
		}
	}
	// 特殊供应商别名前缀匹配
	if strings.HasPrefix(normSession, "traesolo_") || strings.HasPrefix(normSession, "trae_solo_") {
		if v, ok := core.VendorByID("trae-solo"); ok && vendorEnabled(v) {
			return v, nil
		}
	}
	if strings.HasPrefix(normSession, "workbuddy_") || strings.HasPrefix(normSession, "copilot_") {
		if v, ok := firstVendorForRegion("cn"); ok {
			return v, nil
		}
	}
	// 都不认领且无匹配前缀：直接返回会话过期错误，绝不乱调用其他供应商
	return nil, newPluginError("session_not_found", "登录会话不存在或已过期，请重新发起登录", http.StatusNotFound)
}

// vendorEnabled 报告供应商的区域是否被配置启用。
func vendorEnabled(vendor core.Vendor) bool {
	return realmEnabled(loadedConfig(), vendor.Region())
}

// firstEnabledVendor 返回配置里第一个启用的供应商。
func firstEnabledVendor() (core.Vendor, bool) {
	for _, vendor := range core.Vendors() {
		if vendorEnabled(vendor) {
			return vendor, true
		}
	}
	return nil, false
}

// firstVendorForRegion 返回某个区域第一个启用的供应商。
func firstVendorForRegion(region string) (core.Vendor, bool) {
	normalized := strings.ToLower(strings.TrimSpace(region))
	for _, vendor := range core.Vendors() {
		if vendor.Region() == normalized && vendorEnabled(vendor) {
			return vendor, true
		}
	}
	return nil, false
}

// newLoginSessionID 生成一个随机的登录会话标识。
//
// 登录会话是供应商各自持有的，但「随机会话 id」是所有登录链路的公共
// 基础设施——一个 32 位十六进制随机串，与官方客户端的 messageId 同形。
// 放在根层而不是某个供应商包里：Cline / WorkBuddy / Qoder 都用到它，
// 塞进任一家都会让其它家反向依赖那一家。
func newLoginSessionID() string {
	return newHexID(16)
}

// newHexID 生成 n 字节的随机十六进制串（2n 个字符）。
//
// 随机源不可用时退回基于时间与计数的确定性串：登录会话 id 只影响
// 会话查找的唯一性，退化的熵在这种场景下可接受——比返回空串让整个
// 登录链路崩掉好。
func newHexID(n int) string {
	buf := make([]byte, n)
	if _, errRand := rand.Read(buf); errRand != nil {
		return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&hexIDCounter, 1))
	}
	return hex.EncodeToString(buf)
}

var hexIDCounter int64
