package main

// 本文件把 WorkBuddy（CodeBuddy）协议适配成 core.Vendor。
//
// 分界：
//   - internal/vendors/workbuddy 是**纯协议层**（端点、请求头、指纹、SSE、任务闭环），
//     不知道宿存在，出站 HTTP 一律经 internal/httpx 走宿主桥；
//   - 本文件是**适配层**，把根层的配置/状态/凭证解析接到协议层上，
//     并实现 core.Vendor 供根层的 ABI 处理器统一调用。
//
// 供应商实例 = 协议 × 区域。CN 与 GLOBAL 是两套独立部署：域名、模型清单、
// reasoning 档位表、签到活动都不同，且凭证不通用（用 cn 的 token 打 global
// 域名会得到 401 TOKEN_EXPIRE）。区域在构造期固定，实例方法内不再分支。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/workbuddy"
)

// workbuddyVendor 是 core.Vendor 在 WorkBuddy 上的实现。
type workbuddyVendor struct {
	region workbuddy.Region
}

// newWorkBuddyVendors 构造 WorkBuddy 的两个区域实例。
func newWorkBuddyVendors() []core.Vendor {
	return []core.Vendor{
		&workbuddyVendor{region: workbuddy.RegionCN},
		&workbuddyVendor{region: workbuddy.RegionGlobal},
	}
}

func (v *workbuddyVendor) ID() string     { return workbuddy.VendorIDFor(v.region) }
func (v *workbuddyVendor) Name() string   { return workbuddy.VendorNameFor(v.region) }
func (v *workbuddyVendor) Region() string { return string(v.region) }

// ModelPrefix 返回空：本供应商用裸模型名注册。
//
// 跨供应商的同名模型（如 qoder 的 GLM-5.3 与这里的 glm-5.3）由宿主按
// strings.EqualFold 合并为一个条目，宿主再从各家凭证里选号。
func (v *workbuddyVendor) ModelPrefix() string { return "" }

// ---- 凭证 ----

// Match 判断一份凭证是否属于本区域。
//
// 严格要求区域一致：这是 CN 与 GLOBAL 两个实例之间唯一的区分依据，
// 缺了它两个实例会互抢同一份凭证。
func (v *workbuddyVendor) Match(fileName, provider string, raw map[string]any) bool {
	if !workbuddy.LooksLikeCredential(raw, fileName, provider) {
		return false
	}
	// 优先看文件内的 vendor 字段（插件自己写的，最可靠）。
	if vendorID, okVendor := raw[core.VendorKey].(string); okVendor {
		return strings.EqualFold(strings.TrimSpace(vendorID), v.ID())
	}
	// 其次按凭证自带的区域声明。
	return workbuddy.RegionForCredential(raw) == v.region
}

// Parse 把凭证 JSON 解析成 core.Credential。
func (v *workbuddyVendor) Parse(raw []byte, fileName string) (*core.Credential, error) {
	native, errParse := workbuddy.ParseCredential(raw, v.region)
	if errParse != nil {
		return nil, errParse
	}
	if native.Realm() != v.region {
		return nil, fmt.Errorf("credential realm %q does not match vendor %q", native.Realm(), v.ID())
	}
	return newCoreCredential(native, v.ID(), fileName), nil
}

// newCoreCredential 把协议层凭证包装成 core.Credential。
func newCoreCredential(native *workbuddy.Credential, vendorID, fileName string) *core.Credential {
	token := native.AccessTokenValue()
	authMode := "oauth"
	if token == "" {
		// 设备令牌形态的凭证没有 access/refresh token，靠 device token 出站。
		token = native.DeviceTokenValue()
		authMode = "device"
	}
	return &core.Credential{
		VendorID:     vendorID,
		Region:       string(native.Realm()),
		Label:        firstNonEmptyString(native.NicknameValue(), native.UIDValue(), fileName),
		UID:          native.UIDValue(),
		Token:        token,
		RefreshToken: native.RefreshTokenValue(),
		ExpiresAt:    native.ExpiresAt,
		AuthMode:     authMode,
		Native:       native,
	}
}

// nativeCredential 取回协议层凭证对象。
//
// 凭证经 core.Credential 传递时 Native 被保留，因此正常路径不重新解析。
func (v *workbuddyVendor) nativeCredential(cred *core.Credential) (*workbuddy.Credential, error) {
	if cred == nil {
		return nil, core.NewPluginError("credential_missing", "credential is nil", 401)
	}
	if native, okNative := cred.Native.(*workbuddy.Credential); okNative && native != nil {
		return native, nil
	}
	return nil, core.NewPluginError("credential_missing",
		"credential was not parsed by the workbuddy vendor", 401)
}

// ---- 模型 ----

// StaticModels 返回本区域的模型清单。
func (v *workbuddyVendor) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := modelsForRealm(ctx, v.region)
	if errModels != nil {
		return nil, errModels
	}
	return models, nil
}

// ModelsForAuth 返回该凭证可用的模型。
//
// 本供应商的清单按区域探测（不是按账号），因此与 StaticModels 同源。
// 宿主仍会用这份清单把「凭证 ↔ 模型」绑起来，从而淘汰跨区域凭证。
func (v *workbuddyVendor) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	if cred == nil || cred.RegionValue() != string(v.region) {
		// 区域不符时返回空清单而不是报错：宿主会因此跳过该凭证，
		// 这正是我们想要的结果（避免用错域的凭证打上游）。
		return nil, nil
	}
	return v.StaticModels(ctx)
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *workbuddyVendor) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	client := newUpstreamClient(ctx)
	result, errChat := client.Chat(workbuddy.ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Model:      req.Model,
		ClientIP:   req.ClientIP,
		Meta:       chatMetaFromBody(req.Payload, http.Header(req.Headers)),
	})
	if errChat != nil {
		return nil, errChat
	}
	return &pluginapi.ExecutorResponse{
		Payload: result.Response,
		Headers: map[string][]string{"Content-Type": {jsonContentType}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
//
// 只投递上游的原生 SSE 数据载荷，不做 `data: ` 包装与分帧——包装由宿主
// 按声明格式完成。插件侧重复分帧会与宿主产生不一致的行为。
func (v *workbuddyVendor) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return errNative
	}
	reader, errStream := newUpstreamClient(ctx).ChatStream(workbuddy.ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Model:      req.Model,
		ClientIP:   req.ClientIP,
		Meta:       chatMetaFromBody(req.Payload, http.Header(req.Headers)),
	})
	if errStream != nil {
		return errStream
	}
	defer func() { _ = reader.Close() }()

	_, errForward := workbuddy.Stream(reader, workbuddy.StreamOptions{Emit: sink.Emit})
	// 空流不算失败：上游有时返回一条没有任何 delta 的流，
	// 把它当错误会让客户端收到无意义的错误帧。
	if errForward != nil && !workbuddy.IsEmptyStreamError(errForward) && ctx.Err() == nil {
		return errForward
	}
	return nil
}

// CountTokens 估算 token 数（按字节数除以 3，不调上游）。
func (v *workbuddyVendor) CountTokens(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (int, error) {
	if len(req.Payload) == 0 {
		return 0, nil
	}
	return len(req.Payload) / 3, nil
}

// ---- 额度与签到 ----

// Quota 查询账号额度。
func (v *workbuddyVendor) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	balance, errBalance := newUpstreamClient(ctx).FetchBalance(native)
	if errBalance != nil {
		return nil, errBalance
	}
	response := buildQuotaResponse(native, balance)
	return &response, nil
}

// SupportsCheckin 报告本区域是否提供签到。
//
// 签到是 CodeBuddy 国内版独有的活动；国际版没有该活动，额度按周期自动重置。
// 在调用前就返回 false，UI 才能提前隐藏入口而不是让用户白点一次。
func (v *workbuddyVendor) SupportsCheckin() bool { return !v.region.IsGlobal() }

// Checkin 执行一次签到。国际版返回 nil（无此活动）。
func (v *workbuddyVendor) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	if !v.SupportsCheckin() {
		return nil, nil
	}
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, errNative
	}
	result, errCheckin := newUpstreamClient(ctx).DailyCheckin(native)
	if errCheckin != nil {
		return nil, errCheckin
	}
	message := "今天已经签到过了（幂等，无新增）"
	if !result.Already {
		message = fmt.Sprintf("签到成功：+%d 积分 +%d 能量", result.Credit, result.Energy)
	}
	return &core.CheckinResult{
		Already: result.Already,
		Credit:  result.Credit,
		Energy:  result.Energy,
		Message: message,
	}, nil
}

// ---- 登录与续期 ----

// LoginStart 发起一次设备授权登录。
func (v *workbuddyVendor) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	state, authURL, errStart := requestLoginState(ctx, v.region)
	if errStart != nil {
		return nil, errStart
	}
	sessionID := newLoginSessionID()
	storeLoginSession(sessionID, &pendingLogin{region: v.region, state: state, createdAt: time.Now()})
	return &pluginapi.AuthLoginStartResponse{
		Provider:  providerKey,
		URL:       authURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(loginSessionTTL),
		Metadata: map[string]any{
			core.VendorKey: v.ID(),
			"region":       string(v.region),
		},
	}, nil
}

// LoginPoll 轮询一次登录状态。
func (v *workbuddyVendor) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	session, okSession := takeLoginSession(state)
	if !okSession {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	accessToken, refreshToken, errToken := exchangeLoginToken(ctx, session.region, session.state)
	if errToken != nil {
		if isLoginPending(errToken) {
			// 用户还没在浏览器里完成授权，继续等待。
			return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
		}
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errToken.Error()}, nil
	}
	account, errAccount := fetchLoginAccount(ctx, session.region, session.state, accessToken)
	if errAccount != nil {
		// token 已拿到但账号信息失败：用最小信息落盘，让用户至少能用起来
		// （账号信息会在后续刷新时补齐）。
		logger.Error("fetch account info failed after token exchange: %v", errAccount)
	}
	credential := &workbuddy.Credential{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		UID:          account.uid,
		Nickname:     account.nickname,
		Domain:       account.domain,
		EnterpriseID: account.enterpriseID,
	}
	credential.SetRealm(session.region)
	if credential.Domain == "" {
		credential.Domain = defaultDomainFor(credential.Realm())
	}
	storageJSON, errStorage := credential.StorageJSON()
	if errStorage != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "构建凭证失败：" + errStorage.Error(),
		}, nil
	}
	fileName := core.FileNameFor(v.ID(), firstNonEmptyString(credential.UID, newLoginSessionID()))
	logger.Info("completed workbuddy login for %s (vendor=%s)",
		firstNonEmptyString(credential.NicknameValue(), credential.UIDValue()), v.ID())
	return &pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   buildAuthData(credential, fileName, storageJSON),
	}, nil
}

// Refresh 校验并刷新凭证。
//
// refreshed 报告上游是否真的下发了新令牌：false 表示仅校验通过，
// 调用方不应重写文件（否则 mtime 会无意义地变动，用户无法判断是否真的续期了）。
func (v *workbuddyVendor) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.nativeCredential(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	accessBefore, refreshBefore := native.Snapshot()
	if errRefresh := newUpstreamClient(ctx).RefreshToken(native); errRefresh != nil {
		return nil, false, errRefresh
	}
	refreshed := !native.UnchangedSince(accessBefore, refreshBefore)
	if refreshed {
		cred.ApplyTokenRefresh(native.AccessTokenValue(), native.RefreshTokenValue(), native.ExpiresAt)
	}
	return cred, refreshed, nil
}

// Tasks 返回本区域支持的任务动作。任务闭环是 CodeBuddy 国内版的活动。
func (v *workbuddyVendor) Tasks() []core.Task {
	if v.region.IsGlobal() {
		return nil
	}
	return workbuddyTaskList()
}

// workbuddyTaskList 把根层的任务动作表转成 core.Task 描述。
//
// 只暴露展示所需的元数据（标识、说明、是否耗额度）；真正的执行入口
// 仍在根层（任务中心按 Code 派发），因为任务执行需要账户遍历、并发锁与
// 状态记录，那些属于根层的编排职责而非供应商协议。
func workbuddyTaskList() []core.Task {
	out := make([]core.Task, 0, len(autoActions))
	for _, action := range autoActions {
		out = append(out, core.Task{
			Code:     action.Code,
			Desc:     action.Desc,
			UsesChat: action.UsesChat,
		})
	}
	return out
}
