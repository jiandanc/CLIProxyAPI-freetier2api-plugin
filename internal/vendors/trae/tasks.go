package trae

// 本文件实现 Trae 家族的每日签到与额度查询。
//
// 签到（仅国内版；国际版无此活动）：
//   - 先 POST checkin_credits/status 查今日状态（enable / checked_in / credits）；
//   - 未签到再 POST checkin_credits/claim 领取。claim 对「今日已签到」同样返回
//     code=0，因此「已签到」只能由 status 判定，status 预查是必要流程。
//   - 业务码 9074 是设备级限流（不是已签到），按限流错误上抛。
//
// 额度（仅国内版）：POST pay/ide_user_ent_usage 取权益包列表，
// 只统计 product_type=2 的积分包（credits_limit=-1 表示无限）。
//
// 活动福利（仅国内版）：签到之外还有 2 个无需输入的领奖入口（竞赛 / 抽奖），
// 详见文件末尾的 UGActions 与 ClaimUG。联合活动与两个分享裂变需要活动 ID 或
// 作品 ID，插件取不到，故不进批量入口（端点常量仍保留）。
//
// 兑换码需要用户输入，而手动触发路由只传任务族名、没有请求体通道，故不提供
// 入口——留着实现却调不到，只会是死代码。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

func SupportsCheckin(cfg Config) bool { return cfg.supportsCheckin() }

type CheckinResult struct {
	Already bool
	Credit  int64
	Message string
}

// Checkin 执行每日签到。
func Checkin(ctx context.Context, cred *Credential, cfg Config) (*CheckinResult, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential missing")
	}
	if !cfg.supportsCheckin() {
		return nil, fmt.Errorf("trae 国际版无签到活动")
	}

	headers, errHeaders := checkinHeaders(cred)
	if errHeaders != nil {
		return nil, errHeaders
	}

	// 状态预查失败不阻断领取：null 只表示「无法确认」，不是「不能领」。
	// 把瞬时故障当成签到失败会让账号整天漏签。
	status, errStatus := fetchCheckinStatus(ctx, cfg, headers)
	if errStatus == nil && status != nil {
		if !status.Enable {
			return nil, fmt.Errorf("签到活动未开启")
		}
		if status.CheckedIn {
			return &CheckinResult{Already: true, Credit: status.Credits, Message: "今日已完成签到"}, nil
		}
	}

	if errClaim := claimCheckinCredits(ctx, cfg, headers); errClaim != nil {
		return nil, errClaim
	}
	// claim 响应不含积分数；领取成功后补查一次状态，报告真实所得。
	credit := int64(0)
	if status != nil {
		credit = status.Credits
	}
	if fresh, errFresh := fetchCheckinStatus(ctx, cfg, headers); errFresh == nil && fresh != nil {
		credit = fresh.Credits
	}
	return &CheckinResult{Credit: credit, Message: "签到成功"}, nil
}

// flexInt64 宽容解析业务码/积分：接受 JSON 数字与数字字符串。
//
// 上游网关对同一字段可能下发 0 或 "0"；用 int 直接解码会因类型不符整条
// 响应解析失败，把一次成功签到报成 decode 错误。
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(data []byte) error {
	trimmed := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if trimmed == "" || trimmed == "null" {
		*f = 0
		return nil
	}
	parsed, errParse := strconv.ParseFloat(trimmed, 64)
	if errParse != nil {
		return errParse
	}
	*f = flexInt64(parsed)
	return nil
}

type checkinStatus struct {
	Enable    bool
	CheckedIn bool
	Credits   int64
}

func fetchCheckinStatus(ctx context.Context, cfg Config, headers map[string]string) (*checkinStatus, error) {
	raw, errPost := postCheckin(ctx, cfg.CheckinHost+EpCheckinStatus, headers)
	if errPost != nil {
		return nil, errPost
	}
	var result struct {
		Code      flexInt64 `json:"code"`
		Message   string    `json:"message"`
		Enable    bool      `json:"enable"`
		CheckedIn bool      `json:"checked_in"`
		Credits   flexInt64 `json:"credits"`
	}
	if errJSON := json.Unmarshal(raw, &result); errJSON != nil {
		return nil, fmt.Errorf("decode checkin status: %w", errJSON)
	}
	if result.Code != 0 {
		// 业务码错误的 HTTP 状态是 200，但传 0 才能让下游按业务类型处理
		// （传 200 会让 Failure.HTTPStatus() 返回 200，限流/凭证策略不生效）。
		return nil, Classify(0, string(raw))
	}
	return &checkinStatus{Enable: result.Enable, CheckedIn: result.CheckedIn, Credits: int64(result.Credits)}, nil
}

func claimCheckinCredits(ctx context.Context, cfg Config, headers map[string]string) error {
	// 只在传输层失败（没有 HTTP 响应）时重试；业务码错误重试无意义。
	var lastErr error
	for attempt := 0; attempt <= 3; attempt++ {
		raw, errPost := postCheckin(ctx, cfg.CheckinHost+EpCheckinClaim, headers)
		if errPost != nil {
			if _, isUpstream := errPost.(*core.Failure); isUpstream {
				return errPost
			}
			lastErr = errPost
			if attempt < 3 && !core.SleepContext(ctx, time.Second) {
				return ctx.Err()
			}
			continue
		}
		var result struct {
			Code    flexInt64 `json:"code"`
			Message string    `json:"message"`
		}
		if errJSON := json.Unmarshal(raw, &result); errJSON != nil {
			return fmt.Errorf("decode checkin claim: %w", errJSON)
		}
		if result.Code == 0 {
			return nil
		}
		// 9074 = 设备级限流；其余业务码原样分类上抛（状态传 0，见 fetchCheckinStatus）。
		return Classify(0, string(raw))
	}
	return lastErr
}

// perRequestTimeout 是单个上游请求的超时，签到 / 额度 / 活动三个端点共用。
//
// 活动福利的一轮总预算（ugRoundTimeout）按它推算：端点数 × perRequestTimeout
// 就是「每个端点都卡满」的耗时，总预算必须严格大于这个乘积。两边都读这一个
// 常量，改超时只改这里，不会出现「预算按 30s 算、实际单请求 60s」这种错配。
const perRequestTimeout = 30 * time.Second

// ugDynamicTimeout 是活动表 / 活动触发 / 分享裂变这几个动态端点的单次上限。
//
// 比固定端点小，是因为一轮里的请求数不再固定：2 个固定端点已经占掉
// 2×perRequestTimeout，剩下的还要装 1 次活动表 + 若干次活动触发 + 2 次裂变，
// 全部卡满时仍必须落在宿主 3 分钟的 task 上限内
// （internal/tasks/api.go）。实测这些接口都在 1s 内返回，15s 是十倍余量。
const ugDynamicTimeout = 15 * time.Second

// ugClaimReserve 是「还能再打一次活动触发」所需的剩余预算。
//
// 不按固定条数截断：上游随时可能同时放出多条可领活动，条数写死就会把后面的
// 留到下一轮。改成看预算——只要剩余时间还够打一次动态端点就继续领，既能领干净，
// 又不会把整轮拖过宿主上限（总耗时恒不超过 ugRoundTimeout）。
const ugClaimReserve = ugDynamicTimeout + 2*time.Second

// ugHasBudget 判断 ctx 剩余时间是否还够再打一次动态端点。
func ugHasBudget(ctx context.Context, need time.Duration) bool {
	deadline, okDeadline := ctx.Deadline()
	if !okDeadline {
		return true
	}
	return time.Until(deadline) >= need
}

// ugDynamicCtx 给单个动态端点套一次上限。
func ugDynamicCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, ugDynamicTimeout)
}

// postJSON 发一次 JSON POST 并返回状态码与响应体（读上限 1MB）。
//
// 签到、额度、活动三个端点用的是同一套官方客户端头、同一个超时，只是对非
// 200 的处理不同（签到/额度要分类上抛，活动要把 body 里的原因带出来）——
// 所以这里只负责传输，语义留给调用方。
func postJSON(ctx context.Context, url string, headers map[string]string, payload []byte) (int, []byte, error) {
	if payload == nil {
		payload = []byte("{}")
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if errReq != nil {
		return 0, nil, errReq
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, errDo := hostbridge.Client(ctx, perRequestTimeout).Do(req)
	if errDo != nil {
		return 0, nil, fmt.Errorf("trae request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// postCheckin 是签到端点的语义层：非 200 一律分类上抛。
func postCheckin(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	status, body, errPost := postJSON(ctx, url, headers, nil)
	if errPost != nil {
		return nil, errPost
	}
	if status != http.StatusOK {
		return nil, Classify(status, string(body))
	}
	return body, nil
}

// checkinHeaders 装配签到/额度共用的完整客户端请求头。
//
// 参考实现（trae-mate）实测：签到端点接受的是**约 20 个头的官方客户端形态**，
// 只发 Authorization + 设备头会被上游拒绝或归到不同设备配额。设备身份必须
// 基于账号 uid 确定性派生：同一账号稳定、不同账号互不共享（上游按设备限流）。
func checkinHeaders(cred *Credential) (map[string]string, error) {
	uid := traeUserID(cred)
	if uid == "" {
		return nil, fmt.Errorf("trae: 缺少账号 uid，无法派生签到设备身份")
	}
	return checkinHeadersForUID(cred, uid), nil
}

// quotaHeaders 是额度查询的头：没有 uid 时退回凭证设备号，不让额度查询失败。
//
// 签到必须拒绝共享默认设备身份（上游按设备限流）；额度只读接口没有这个
// 约束，用一个稳定的账号级标识即可。
func quotaHeaders(cred *Credential) map[string]string {
	if uid := traeUserID(cred); uid != "" {
		return checkinHeadersForUID(cred, uid)
	}
	return checkinHeadersForUID(cred, strings.TrimSpace(cred.DeviceID))
}

// checkinHeadersForUID 用给定身份装配官方客户端请求头。
func checkinHeadersForUID(cred *Credential, uid string) map[string]string {
	return map[string]string{
		"Content-Type":       "application/json",
		"Accept":             "*/*",
		"Accept-Language":    "zh-CN",
		"User-Agent":         traeCheckinUserAgent,
		"Authorization":      "Cloud-IDE-JWT " + cred.AccessToken,
		"X-Market-Client-Id": traeCheckinMarketClientID,
		"X-Market-User-Id":   deriveMarketUserID(uid),
		"X-User-Region":      cred.RegionHeader(),
		"X-Device-Id":        deriveCheckinDeviceID(uid),
		"X-Lgw-Req-Sdk-Type": "3",
		"Package-Type":       cred.PackageTypeHeader(),
		"X-Lscbd-Aid":        "787976",
		"X-Lscbd-Platform":   "windows",
		// 参考实现调用时产品信息为空，实际发送的就是字面量 undefined；
		// 客户端身份由 User-Agent 表达，App-Version 不参与校验。
		"App-Version":      "undefined",
		"X-Tt-Trace-Id":    "00-" + core.RandomHex(8) + "-01",
		"Vscode-Sessionid": deriveCheckinSessionID(uid),
		"X-Request-Id":     newUUID(),
		"Sec-Fetch-Dest":   "empty",
		"Sec-Fetch-Mode":   "no-cors",
		"Sec-Fetch-Site":   "none",
	}
}

const (
	// traeCheckinUserAgent / traeCheckinMarketClientID 是官方 TRAE SOLO CN
	// 客户端在签到请求里使用的固定 UA 与市场客户端标识。
	traeCheckinUserAgent      = "VSCode 1.107.1 (TRAE SOLO CN)"
	traeCheckinMarketClientID = "VSCode 1.107.1"
	// productTypeCredits 是积分包的产品类型；product_type=0 是功能开关包。
	productTypeCredits = 2
)

// traeUserID 返回签到设备身份所用的账号 id。
//
// 优先 JWT payload.data.id（token 刷新后稳定），退回凭证 uid。为空时调用方
// 必须拒绝发请求：共用默认设备号会让多账号互相抢「每设备每天一次」的配额。
func traeUserID(cred *Credential) string {
	if identity := jwtIdentity(cred.AccessToken); identity != "" {
		return identity
	}
	return strings.TrimSpace(cred.UID)
}

// CheckinDeviceID 派生签到用的 15 位设备号（基于账号 uid 的确定性 SHA-256 流）。
func CheckinDeviceID(cred *Credential) string {
	uid := traeUserID(cred)
	if uid == "" {
		return ""
	}
	return deriveCheckinDeviceID(uid)
}

func deriveCheckinDeviceID(uid string) string {
	stream := seededStream(uid, "devid", 15)
	out := make([]byte, len(stream))
	for index, value := range stream {
		out[index] = '0' + value%10
	}
	return string(out)
}

// deriveMarketUserID 派生 UUID v4 形态的 X-Market-User-Id。
func deriveMarketUserID(uid string) string {
	bs := seededStream(uid, "market", 16)
	bs[6] = (bs[6] & 0x0F) | 0x40
	bs[8] = (bs[8] & 0x3F) | 0x80
	hex := fmt.Sprintf("%x", bs)
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32]
}

// deriveCheckinSessionID 派生 64 位 hex 的 Vscode-Sessionid。
func deriveCheckinSessionID(uid string) string {
	return fmt.Sprintf("%x", seededStream(uid, "sess", 32))
}

// seededStream 是确定性伪随机流：SHA256(salt:seed || counterBE32) 串联。
//
// 与参考实现（trae-mate device_map.rs）逐字节一致，保证同一账号跨进程、
// 跨版本得到同一套设备标识。
func seededStream(seed, salt string, nbytes int) []byte {
	prefix := salt + ":" + seed
	out := make([]byte, 0, nbytes)
	for counter := uint32(0); len(out) < nbytes; counter++ {
		hasher := sha256.New()
		_, _ = hasher.Write([]byte(prefix))
		var counterBytes [4]byte
		binary.BigEndian.PutUint32(counterBytes[:], counter)
		_, _ = hasher.Write(counterBytes[:])
		out = append(out, hasher.Sum(nil)...)
	}
	return out[:nbytes]
}

// jwtIdentity 从 Cloud-IDE-JWT 解出账号 id（payload.data.id）。
func jwtIdentity(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return ""
	}
	var parsed struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errJSON := json.Unmarshal(payload, &parsed); errJSON != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Data.ID)
}

// ---- 额度 ----

// FetchQuota 查询账号积分：剩余 / 总额 / 已用。
func FetchQuota(ctx context.Context, cred *Credential, cfg Config) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential missing")
	}

	payload, errBody := json.Marshal(map[string]any{"require_usage": true, "req_source": 2})
	if errBody != nil {
		return nil, errBody
	}
	status, body, errPost := postJSON(ctx, cfg.QuotaHost+cfg.QuotaPath, quotaHeaders(cred), payload)
	if errPost != nil {
		return nil, errPost
	}
	if status >= 400 {
		return nil, Classify(status, string(body))
	}

	var result struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				ProductType int `json:"product_type"`
				Quota       struct {
					CreditsLimit any `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount any `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if errJSON := json.Unmarshal(body, &result); errJSON != nil {
		return nil, fmt.Errorf("decode quota response: %w", errJSON)
	}

	var total, used float64
	unlimited := false
	for _, pack := range result.UserEntitlementPackList {
		// product_type=0 是功能开关包，没有积分额度。
		if pack.EntitlementBaseInfo.ProductType != productTypeCredits {
			continue
		}
		limit := core.FloatFromAny(pack.EntitlementBaseInfo.Quota.CreditsLimit)
		// credits_limit=-1 是「无限额度」包，不能按 0 或负数跳过——那会让
		// 无限账号显示剩余 0（auth.Totals 认 -1 / unit=unlimited）。
		if limit < 0 {
			unlimited = true
			continue
		}
		total += limit
		if amount := core.FloatFromAny(pack.Usage.CreditsAmount); amount > 0 {
			used += amount
		}
	}

	// 无限额度用 -1 / unit=unlimited 表达（auth.Totals 认这一对），既不能写
	// 0（看起来像用光），也不能写 total-used。
	remain, remainUnit := total-used, "credits"
	if unlimited {
		remain, remainUnit = -1, "unlimited"
	}

	// 键名必须落在 auth.Totals 的词表内（credit_remain / credit_total /
	// credit_used），否则控制台的「剩余 / 总额」会静默显示 0 / 0。
	// 供应商包不能 import auth（分层约定），所以这里按约定手写同名键。
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "Trae 国内版权益",
			TierName: "Standard",
			TierID:   cfg.VendorID,
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "剩余积分", Value: remain, Unit: remainUnit},
			{Key: "credit_total", Label: "总额度", Value: total, Unit: "credits"},
			{Key: "credit_used", Label: "已用积分", Value: used, Unit: "credits"},
		},
	}, nil
}

const (
	// EpUGCompetitionClaim 领取竞赛奖励。
	EpUGCompetitionClaim = "/trae/api/v2/ug/competition/claim_package"
	// EpUGWorkLotteryDraw 抽奖（作品活动）。
	EpUGWorkLotteryDraw = "/trae/api/v2/ug/work_lottery/draw"
)

// UGAction 描述一个福利端点。
type UGAction struct {
	// Key 是稳定标识（落日志/前端过滤用），不随文案变。
	Key string
	// Label 是给人看的中文名。
	Label string
	// Path 是相对签到主机的路径。
	Path string
}

// UGActions 返回签到之外可批量尝试的活动入口。
//
// 只收「不带任何入参就能 POST」的端点。另有三个端点必须带活动 ID 或作品 ID
// （实测上游回 4001 / 9080「param is invalid」），而插件既没有取活动列表的
// 渠道、也拿不到用户的作品 ID，因此不进清单：
//   - POST /trae/api/v2/ug/cobrand/claim（需 activity_id）
//   - POST /trae/api/v2/ug/work_fission/share_link（需作品 ID）
//   - POST /trae/api/v2/ug/code_fission/share_link（需作品 ID）
//
// 放进批量入口只会每轮白打 3 次请求（各自最多 30s），再把注定失败的结果
// 包装成「暂不可领」，既拖慢整轮又掩盖真实原因。等能取到 ID 再接。
func UGActions() []UGAction {
	return []UGAction{
		{Key: "competition_claim", Label: "竞赛奖励", Path: EpUGCompetitionClaim},
		{Key: "work_lottery_draw", Label: "作品抽奖", Path: EpUGWorkLotteryDraw},
	}
}

// UGStatus 是一次领取的归类。
type UGStatus int

const (
	// UGClaimed 表示上游接受了本次调用（含「已领过」，上游对幂等端点同样回 0）。
	UGClaimed UGStatus = iota
	// UGAlready 表示上游明确说之前领过。
	UGAlready
	// UGUnavailable 表示「这次请求注定拿不到」：活动未开始 / 已结束 / 当前
	// 账号不满足条件 / 上游要求插件取不到的入参。都不该重试。
	UGUnavailable
	// UGFailed 表示调用失败或上游报错，需要人看一眼。
	UGFailed
	// UGNeedsPrereq 表示上游收下了调用但没有发放：该活动要等前置动作
	// （学生认证 / 装桌面端 / 发过消息 / 有被邀请人）满足后由上游自动发。
	// 与 UGFailed 分开，是因为这不是故障，重试也拿不到，只能等条件满足。
	UGNeedsPrereq
)

// UGResult 是单个端点的领取结果。
type UGResult struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Status  UGStatus `json:"status"`
	Code    int      `json:"code"`
	Message string   `json:"message,omitempty"`
}

// Describe 返回一行中文文案，供控制台展示。
func (r UGResult) Describe() string {
	switch r.Status {
	case UGClaimed:
		if detail := strings.TrimSpace(r.Message); detail != "" {
			return fmt.Sprintf("%s：已领取（%s）", r.Label, detail)
		}
		return fmt.Sprintf("%s：已领取", r.Label)
	case UGAlready:
		return fmt.Sprintf("%s：此前已领取", r.Label)
	case UGUnavailable:
		if detail := strings.TrimSpace(r.Message); detail != "" {
			return fmt.Sprintf("%s：暂不可领（%s）", r.Label, ugDetailText(detail))
		}
		return fmt.Sprintf("%s：暂不可领", r.Label)
	case UGNeedsPrereq:
		if detail := strings.TrimSpace(r.Message); detail != "" {
			return fmt.Sprintf("%s：待满足前置条件（%s）", r.Label, detail)
		}
		return fmt.Sprintf("%s：待满足前置条件", r.Label)
	default:
		if detail := strings.TrimSpace(r.Message); detail != "" {
			return fmt.Sprintf("%s：失败（%s）", r.Label, ugDetailText(detail))
		}
		return fmt.Sprintf("%s：失败", r.Label)
	}
}

// ugDetailText 把上游的机器文案翻成人话，认不出的原样返回（不吞原始信息）。
func ugDetailText(message string) string {
	// 缺参类文案（param is invalid / 9080）已随「需要入参的端点」一起退出
	// 批量入口，不会再出现在结果里，所以这里只剩「活动整体还没开」要翻译。
	if core.ContainsAny(strings.ToLower(message), ugSoftUnavailableMarkers) {
		return "活动暂不可用"
	}
	return message
}

// ClaimUG 依次领取全部不需要输入的福利端点。
//
// 返回的 results 覆盖所有已尝试的端点；err 只在「这一轮不该继续」时非空
// （凭证失效 401、或整轮 ugRoundTimeout 超时/被取消），单个端点失败只体现在它的
// UGResult 上。
func ClaimUG(ctx context.Context, cred *Credential, cfg Config) ([]UGResult, error) {
	if cred == nil || strings.TrimSpace(cred.AccessToken) == "" {
		return nil, fmt.Errorf("trae: 缺少访问令牌，无法领取活动奖励")
	}
	if !cfg.supportsCheckin() {
		// UG 接口只在国内版存在；国际版没有这套活动。
		return nil, fmt.Errorf("trae: 当前区域不支持活动福利领取")
	}
	headers, errHeaders := checkinHeaders(cred)
	if errHeaders != nil {
		return nil, errHeaders
	}
	// 整轮上限见 ugRoundTimeout（2 个固定端点 + 动态入口的最坏耗时之和）。
	// 预算必须严格大于「端点个数 × 单端点超时」：两者相等时，第一个端点一旦
	// 卡满，第二个端点剩下的预算连握手都不够，结构上必然被砍掉。
	// 超时与取消一样：把已拿到的结果带出去。
	ctx, cancel := context.WithTimeout(ctx, ugRoundTimeout)
	defer cancel()

	results := make([]UGResult, 0, len(UGActions())+8)
	for _, action := range UGActions() {
		if errCtx := ctx.Err(); errCtx != nil {
			return results, errCtx
		}
		result, errStop := claimUGOne(ctx, cfg, headers, action)
		results = append(results, result)
		if errStop != nil {
			// 凭证失效：后面的端点只会得到同一个 401，停在这里并如实上报。
			return results, errStop
		}
	}
	// 活动中心：上游的活动是一张表（activity/info）+ 一个通用操作入口
	// （activity/action），不是固定端点。读表才知道当前有哪些活动、哪些能领，
	// 写死端点会漏掉上游新开的活动，也会在活动下线后白打请求。
	overview, claimed, errActivity := claimUGActivities(ctx, cfg, headers)
	if errActivity != nil {
		return results, errActivity
	}
	results = append(results, claimed...)
	results = append(results, overview...)
	// 分享裂变：当前这一期不在活动表里（表里那条已结束），单独探测。
	fissionResults, errFission := claimFissionRewards(ctx, cfg, headers)
	if errFission != nil {
		return results, errFission
	}
	results = append(results, fissionResults...)
	return results, nil
}

// ugRoundTimeout 是一轮活动福利领取的总上限。
//
// 必须严格大于最坏耗时：2 个固定端点各 perRequestTimeout（60s）+ 活动表与
// 裂变 status/grant 共 3 次各 ugDynamicTimeout（45s）+ 至少一次活动触发的
// 预留 ugClaimReserve（17s）= 122s，取 150s 留 28s 余量。活动触发条数不写死，
// 靠 claimUGActivities 里的 ugHasBudget 按剩余预算截断，总耗时恒不超过本值。
// 加端点时同步上调（ug_test.go 用同一组常量验算，改超时不会漏改预算）。
const ugRoundTimeout = 150 * time.Second

// claimUGOne 打一个端点并归类结果。
//
// 第二个返回值只在「这一轮不该继续」时非空（凭证失效），其余失败只体现在
// UGResult 上：单个活动不可领不影响后面的端点。
func claimUGOne(ctx context.Context, cfg Config, headers map[string]string, action UGAction) (UGResult, error) {
	result := UGResult{Key: action.Key, Label: action.Label}
	status, body, errDo := postJSON(ctx, cfg.CheckinHost+action.Path, headers, nil)
	if errDo != nil {
		result.Status = UGFailed
		result.Message = errDo.Error()
		return result, nil
	}
	if status == http.StatusUnauthorized {
		// 凭证失效：后面的端点只会得到同一个 401。用分类错误上抛，宿主据此
		// 把账号标成需要重新登录，而不是报一个无法归因的 502。
		result.Status = UGFailed
		result.Message = "登录态已失效（HTTP 401），请重新登录后再领取"
		return result, &core.Failure{Kind: core.FailureKindCredential, Status: status, Msg: result.Message}
	}
	// Code 用指针：网关回 HTML、空 body、或 {"data":{}} 时它会是 nil，此时
	// 绝不能把零值当成 code=0（那会把失败报成「已领取」）。
	var envelope struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if errJSON := json.Unmarshal(body, &envelope); errJSON != nil || envelope.Code == nil {
		// 少数端点（实测 claim_package）回的是纯状态对象：没有业务码，
		// 只有 eligible / newly_granted。按 code==nil 一律归失败会把「当前
		// 账号不满足条件」报成上游故障，所以先认状态字段再退回失败。
		if state, okState := readUGState(body); okState {
			result.Status, result.Message = state.describe()
			// 上游没给业务码，Code 保持零值占位——所以任何地方都不能按
			// Code==0 判「上游回 0 = 已领取」，只能按 Status 判。
			return result, nil
		}
		result.Status = UGFailed
		result.Message = truncateBody(string(body))
		if result.Message == "" {
			result.Message = fmt.Sprintf("HTTP %d：响应不含业务码", status)
		}
		return result, nil
	}
	result.Code = *envelope.Code
	result.Message = core.FirstNonEmpty(strings.TrimSpace(envelope.Message), strings.TrimSpace(envelope.Msg))
	switch {
	case status == http.StatusOK && *envelope.Code == 0:
		result.Status = UGClaimed
	case status == http.StatusOK:
		result.Status = classifyUGMessage(result.Message)
	default:
		// 非 200：**只有 4xx 才按文案归类**。
		//
		// 4xx 是「这次请求本身不对」：实测 cobrand/claim 缺 activity_id 回
		// 400 + 9080「param is invalid」（我们没带参数），409 + 1001「该奖励
		// 已领取」（此前领过）。这两种重试都不会变，归到对应状态是对的。
		//
		// 5xx 与 429 必须留给 Classify 走 Transient：上游 503 若因为文案里
		// 恰好带「暂不可用」就被折成「暂不可领」，调度和用户都不会重试，
		// 真实故障被静默吞掉。这正是本函数原先的缺陷。
		if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
			if classified := classifyUGMessage(result.Message); classified == UGUnavailable || classified == UGAlready {
				result.Status = classified
				return result, nil
			}
		}
		result.Status = UGFailed
		if result.Message == "" {
			if failure := Classify(status, string(body)); failure != nil {
				result.Message = failure.Msg
			}
		}
		if result.Message == "" {
			result.Message = fmt.Sprintf("HTTP %d", status)
		}
	}
	return result, nil
}

// truncateBody 把非 JSON 响应压成一行短提示，避免把整页 HTML 塞进横幅。
func truncateBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}
	if runes := []rune(trimmed); len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return trimmed
}

// ugState 是「无业务码」响应的状态字段。
//
// 实测 claim_package 回的是纯状态对象（eligible=false，没有 code）。用指针
// 区分「上游没给这个字段」与「给了 false」，否则缺字段会被当成不满足条件。
type ugState struct {
	Eligible     *bool
	NewlyGranted *bool
}

// readUGState 从响应体里读 eligible / newly_granted（顶层，或包一层 data）。
//
// 实测只有这两种形状，所以用定长结构一次解完，不做递归下钻：多包几层的
// 响应从没出现过，为它写通用遍历只会让「到底认哪些字段」变得不可读。
// 第二个返回值 false 表示两个字段都没出现，调用方应退回原有的失败归类。
func readUGState(body []byte) (ugState, bool) {
	type ugStateFields struct {
		Eligible     *bool `json:"eligible"`
		NewlyGranted *bool `json:"newly_granted"`
	}
	var probe struct {
		ugStateFields
		Data *ugStateFields `json:"data"`
	}
	if errJSON := json.Unmarshal(body, &probe); errJSON != nil {
		return ugState{}, false
	}
	state := ugState{Eligible: probe.Eligible, NewlyGranted: probe.NewlyGranted}
	// 逐字段回落，而不是「顶层有任一字段就整层信顶层」：上游出现过顶层只给
	// eligible、data 里另给 newly_granted 的混合形状。整层优先会把 data 里的
	// 新字段直接丢掉，把「本次已发放」读成「可参与但未发放」——判反了。
	if probe.Data != nil {
		if state.Eligible == nil {
			state.Eligible = probe.Data.Eligible
		}
		if state.NewlyGranted == nil {
			state.NewlyGranted = probe.Data.NewlyGranted
		}
	}
	if state.Eligible == nil && state.NewlyGranted == nil {
		return ugState{}, false
	}
	return state, true
}

// describe 把状态字段翻成 (状态, 文案)。
func (s ugState) describe() (UGStatus, string) {
	switch {
	case s.NewlyGranted != nil && *s.NewlyGranted:
		return UGClaimed, "上游本次发放成功"
	case s.Eligible != nil && !*s.Eligible:
		return UGUnavailable, "当前账号不满足活动条件"
	case s.Eligible != nil && *s.Eligible:
		return UGUnavailable, "活动可参与，但本次未发放"
	default:
		// 只给了 newly_granted=false（没给 eligible）：本次没发东西。
		return UGUnavailable, "本次未发放"
	}
}

// 上游不会给出稳定的码表，只能按文案粗分；标记表放在包级只读变量里，
// 避免每次调用都分配一次切片。
var (
	ugAlreadyMarkers = []string{"已领取", "已经领取", "重复领取", "已参与", "已经参与", "已兑换", "已经兑换", "already"}

	// ugParamMarkers 是「上游要求额外参数」的标记。实测两个 share_link 回
	// 4001、cobrand/claim 回 9080，文案都是「param is invalid」——这些端点
	// 已不在 UGActions() 里（插件拿不到活动/作品 ID），但归类表仍要认识这批
	// 文案：日后接上入参时，失败原因能直接读对，不用再猜一次。
	ugParamMarkers = []string{"param is invalid", "参数错误", "参数不合法", "参数无效", "缺少参数", "参数缺失"}

	// ugSoftUnavailableMarkers 是「活动整体还没开 / 没开完」的柔和说法。
	// 实测 work_lottery/draw 回 9069「活动暂不可用」。
	ugSoftUnavailableMarkers = []string{"暂不可用", "暂未开放"}

	// ugUnavailableMarkers 是完整的「暂不可领」表：活动状态类 + 柔和说法 +
	// 缺参类。后两组直接复用上面的表，避免同一批文案写两份、改一处漏一处。
	ugUnavailableMarkers = append(
		append([]string{
			"未开始", "已结束", "活动结束", "未参与", "不满足", "无资格", "不在活动",
			"暂不可领", "not started", "ended", "unavailable",
		}, ugSoftUnavailableMarkers...),
		ugParamMarkers...,
	)
)

// classifyUGMessage 按上游文案做粗归类。
//
// 不猜业务码：码表随活动变，猜错会把「已领过」显示成失败，反而误导。
func classifyUGMessage(message string) UGStatus {
	text := strings.ToLower(message)
	switch {
	case text == "":
		// 非 0 码却没给文案：当成失败，让人能看到原始码。
		return UGFailed
	case core.ContainsAny(text, ugAlreadyMarkers):
		return UGAlready
	case core.ContainsAny(text, ugUnavailableMarkers):
		return UGUnavailable
	default:
		return UGFailed
	}
}

// SummarizeUG 把一轮结果压成一段给人看的文案。
//
// 明细只铺「有结果」的（领到 / 已领 / 待前置 / 失败），暂不可领的只列名字：
// 它们每轮都一样，铺开只会把真正领到的东西挤没。
func SummarizeUG(results []UGResult) string {
	if len(results) == 0 {
		return "没有可领取的活动入口"
	}
	parts := make([]string, 0, len(results))
	names := make([]string, 0, len(results))
	var claimed, already, unavailable, pending, failed int
	for _, result := range results {
		switch result.Status {
		case UGClaimed:
			claimed++
			parts = append(parts, result.Describe())
		case UGAlready:
			already++
			parts = append(parts, result.Describe())
		case UGUnavailable:
			unavailable++
			names = append(names, result.Label)
		case UGNeedsPrereq:
			// 前置条件没满足不是故障，别混进失败数里吓人。
			pending++
			parts = append(parts, result.Describe())
		default:
			failed++
			parts = append(parts, result.Describe())
		}
	}
	if unavailable > 0 {
		parts = append(parts, fmt.Sprintf("暂不可领 %d 项（%s）", unavailable, strings.Join(names, "、")))
	}
	head := fmt.Sprintf("活动福利：领到 %d 项，此前已领 %d 项，待满足前置 %d 项，失败 %d 项",
		claimed, already, pending, failed)
	return head + "；" + strings.Join(parts, "；")
}

// ── 活动中心：activity/info 驱动的动态领取 ─────────────────────────────────
// EpUGActivityInfo 是活动表。
const EpUGActivityInfo = "/trae/api/v2/ug/activity/info"

// EpUGActivityAction 是通用活动操作入口。
const EpUGActivityAction = "/trae/api/v2/ug/activity/action"

// 活动类型码（实测 activity_type 取值）。
const (
	// ugTypeNewUser 新用户注册奖励：注册即发，无需客户端动作。
	ugTypeNewUser = 102
	// ugTypeFission 分享裂变：按被邀请人数量发积分。
	ugTypeFission = 103
	// ugTypeCheckin 签到：走 checkin_credits 专用接口，不进通用入口。
	ugTypeCheckin = 104
	// ugTypeSendMessage 发消息奖励：上游判定用户发过消息才发。
	ugTypeSendMessage = 105
	// ugTypeDesktopDownload 桌面端下载奖励：上游判定装过桌面端才发。
	ugTypeDesktopDownload = 106
	// ugTypeStudentAuth 学生认证奖励：上游判定完成学籍认证才发。
	ugTypeStudentAuth = 107
)

// ugActivity 是 activity/info 里的一条活动。
//
// 字段全用指针或零值可区分的形式：上游对不同类型的活动给不同子集，
// 缺字段与「字段为 false」必须分开（否则「没给 granted」会被读成「未发放」）。
type ugActivity struct {
	ID        string `json:"activity_id"`
	Type      int    `json:"activity_type"`
	Enabled   bool   `json:"Enabled"`
	StartMs   int64  `json:"start_time_ms"`
	EndMs     int64  `json:"end_time_ms"`
	WorkExtra struct {
		GeneralCredits *int64 `json:"general_credits"`
		WorkCredits    *int64 `json:"work_credits"`
		AutoGrant      *bool  `json:"auto_grant"`
		Eligible       *bool  `json:"eligible"`
		Grantable      *bool  `json:"grantable"`
		Granted        *bool  `json:"granted"`
	} `json:"work_extra"`
}

// label 返回给人看的中文名。
func (a ugActivity) label() string {
	switch a.Type {
	case ugTypeNewUser:
		return "新用户奖励"
	case ugTypeFission:
		return "分享裂变"
	case ugTypeCheckin:
		return "每日签到"
	case ugTypeSendMessage:
		return "发消息奖励"
	case ugTypeDesktopDownload:
		return "桌面端下载奖励"
	case ugTypeStudentAuth:
		return "学生认证奖励"
	default:
		if a.ID != "" {
			return a.ID
		}
		return "未知活动"
	}
}

// rewardText 返回奖励额度文案，没给额度时返回空串。
func (a ugActivity) rewardText() string {
	var parts []string
	if a.WorkExtra.GeneralCredits != nil && *a.WorkExtra.GeneralCredits > 0 {
		parts = append(parts, fmt.Sprintf("%d 通用积分", *a.WorkExtra.GeneralCredits))
	}
	if a.WorkExtra.WorkCredits != nil && *a.WorkExtra.WorkCredits > 0 {
		parts = append(parts, fmt.Sprintf("%d 工作积分", *a.WorkExtra.WorkCredits))
	}
	return strings.Join(parts, " + ")
}

// expired 判断活动是否已过结束时间（end_time_ms 为 0 视为无期限）。
func (a ugActivity) expired(now time.Time) bool {
	if a.EndMs <= 0 {
		return false
	}
	return now.UnixMilli() > a.EndMs
}

// notStarted 判断活动是否还没开始（start_time_ms 为 0 视为早已开始）。
func (a ugActivity) notStarted(now time.Time) bool {
	if a.StartMs <= 0 {
		return false
	}
	return now.UnixMilli() < a.StartMs
}

// grantable 表示上游明确说「这个账号现在可以领」。
func (a ugActivity) grantable() bool {
	return a.WorkExtra.Grantable != nil && *a.WorkExtra.Grantable
}

// granted 表示上游明确说「已经发过了」。
func (a ugActivity) granted() bool {
	return a.WorkExtra.Granted != nil && *a.WorkExtra.Granted
}

// autoGrant 表示该活动由上游在条件满足时自动发放。
func (a ugActivity) autoGrant() bool {
	return a.WorkExtra.AutoGrant != nil && *a.WorkExtra.AutoGrant
}

// needsManualAction 判断该活动的发放前提是「用户自己动手」。
//
// 实测 student_auth_reward：activity/info 里 grantable=true、granted=false，
// 但 action 只回 200 `{}`，复查 granted 仍是 false —— 上游要的是先完成学籍
// 认证。桌面端下载（要先装客户端）、发消息（要先在 Trae 里聊天）同理。
// 这类前置插件拿不到，打 action 是纯浪费，也不该当成福利推送出去。
func (a ugActivity) needsManualAction() bool {
	switch a.Type {
	case ugTypeSendMessage, ugTypeDesktopDownload, ugTypeStudentAuth:
		return true
	default:
		return false
	}
}

// fetchUGActivities 拉取当前账号可见的活动表。
//
// body 必须是空对象 {}：实测带 req_source 会被上游回 400 空响应。
func fetchUGActivities(ctx context.Context, cfg Config, headers map[string]string) ([]ugActivity, error) {
	ctx, cancel := ugDynamicCtx(ctx)
	defer cancel()
	status, body, errDo := postJSON(ctx, cfg.CheckinHost+EpUGActivityInfo, headers, []byte("{}"))
	if errDo != nil {
		return nil, errDo
	}
	if status == http.StatusUnauthorized {
		return nil, &core.Failure{
			Kind:   core.FailureKindCredential,
			Status: http.StatusUnauthorized,
			Msg:    "登录态已失效，请重新登录后再领取活动福利",
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("activity/info HTTP %d：%s", status, truncateBody(string(body)))
	}
	var envelope struct {
		Activities []ugActivity `json:"commercial_activities"`
	}
	if errJSON := json.Unmarshal(body, &envelope); errJSON != nil {
		return nil, fmt.Errorf("activity/info 响应无法解析：%s", truncateBody(string(body)))
	}
	return envelope.Activities, nil
}

// claimableActivity 判断该活动本轮是否值得打一次 action。
//
// 只挑「上游明确说可领且还没发」的。其余活动一律不发请求：
//   - 签到走专用接口（checkin_credits/claim），重复打会打乱它自己的状态机
//   - 新用户奖励注册时已发，action 无意义
//   - 需要用户自己去客户端里做前置动作的（发消息 / 桌面端 / 学生认证）见
//     needsManualAction：action 只会回 200 空响应，纯浪费一次请求
func claimableActivity(a ugActivity, now time.Time) bool {
	if !a.Enabled || a.ID == "" {
		return false
	}
	if a.Type == ugTypeCheckin || a.Type == ugTypeNewUser || a.needsManualAction() {
		return false
	}
	if a.expired(now) || a.notStarted(now) {
		return false
	}
	return a.grantable() && !a.granted()
}

// claimUGActivity 对单个活动触发一次发放。
func claimUGActivity(ctx context.Context, cfg Config, headers map[string]string, a ugActivity) UGResult {
	ctx, cancel := ugDynamicCtx(ctx)
	defer cancel()
	result := UGResult{Key: "activity:" + a.ID, Label: a.label()}
	if reward := a.rewardText(); reward != "" {
		result.Label = fmt.Sprintf("%s（%s）", a.label(), reward)
	}
	payload, _ := json.Marshal(map[string]string{"activity_id": a.ID})
	status, body, errDo := postJSON(ctx, cfg.CheckinHost+EpUGActivityAction, headers, payload)
	if errDo != nil {
		result.Status = UGFailed
		result.Message = errDo.Error()
		return result
	}
	if status == http.StatusUnauthorized {
		result.Status = UGFailed
		result.Message = "登录态已失效（HTTP 401），请重新登录后再领取"
		return result
	}
	// 空响应体 / 空对象：上游接受了调用但没有给出任何发放内容。
	//
	// 实测 student_auth_reward 就是这种：action 回 200 `{}`，复查 activity/info
	// 的 granted 仍是 false。这不是「已领取」，而是「还差前置动作」——把它报成
	// 已领取会让用户以为积分到账了。
	var envelope struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if errJSON := json.Unmarshal(body, &envelope); errJSON != nil || envelope.Code == nil {
		if len(strings.TrimSpace(string(body))) <= 2 {
			result.Status = UGNeedsPrereq
			result.Message = a.prereqHint()
			return result
		}
		result.Status = UGFailed
		result.Message = truncateBody(string(body))
		return result
	}
	result.Code = *envelope.Code
	result.Message = core.FirstNonEmpty(strings.TrimSpace(envelope.Message), strings.TrimSpace(envelope.Msg))
	switch {
	case status == http.StatusOK && *envelope.Code == 0:
		if strings.TrimSpace(result.Message) == "" || result.Message == "success" {
			// 回了 0 但没有任何说明：与空对象同义，按需前置处理。
			result.Status = UGNeedsPrereq
			result.Message = a.prereqHint()
			return result
		}
		result.Status = UGClaimed
	case status == http.StatusOK:
		result.Status = classifyUGMessage(result.Message)
	default:
		if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
			if classified := classifyUGMessage(result.Message); classified == UGUnavailable || classified == UGAlready {
				result.Status = classified
				result.Message = a.unavailableHint(result.Message)
				return result
			}
		}
		result.Status = UGFailed
		if result.Message == "" {
			result.Message = fmt.Sprintf("HTTP %d", status)
		}
	}
	return result
}

// prereqHint 说明该活动还差什么前置动作。
func (a ugActivity) prereqHint() string {
	switch a.Type {
	case ugTypeStudentAuth:
		return "需先在 Trae 完成学生认证，认证通过后由上游自动发放"
	case ugTypeDesktopDownload:
		return "需先下载并登录 Trae 桌面端，装好后由上游自动发放"
	case ugTypeSendMessage:
		return "需先在 Trae 内发过消息，之后由上游自动发放"
	case ugTypeFission:
		return "需先邀请新用户注册，被邀请人达标后自动发放"
	default:
		return "上游未说明发放条件，暂未发放"
	}
}

// unavailableHint 给「暂不可领」补上人话原因。
func (a ugActivity) unavailableHint(message string) string {
	if a.expired(time.Now()) {
		return "活动已结束"
	}
	if core.ContainsAny(strings.ToLower(message), ugSoftUnavailableMarkers) {
		return "活动暂不可用"
	}
	return message
}

// claimUGActivities 拉活动表并领取其中可领的部分。
//
// 返回的活动总览（overview）不截断，供面板展示全部活动及其状态；
// 领取结果（results）只覆盖本轮真正尝试过的活动。
func claimUGActivities(ctx context.Context, cfg Config, headers map[string]string) (overview []UGResult, results []UGResult, err error) {
	activities, errFetch := fetchUGActivities(ctx, cfg, headers)
	if errFetch != nil {
		if failure, okFailure := errFetch.(*core.Failure); okFailure && failure.Kind == core.FailureKindCredential {
			return nil, nil, errFetch
		}
		// 拿不到活动表不影响原有端点继续跑，如实记一条即可。
		return nil, nil, nil
	}
	now := time.Now()
	// 先领，再出总览：已在本轮触发过的活动不再重复铺一条「等待本轮触发」，
	// 否则同一条活动会出现两次、两种说法。
	claimedIDs := make(map[string]bool, len(activities))
	for _, a := range activities {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, results, errCtx
		}
		if !claimableActivity(a, now) {
			continue
		}
		// 能领的全领：不按条数截断，只按剩余预算截断。预算不够时剩下的活动
		// 会在总览里显示成「可领取，等待本轮触发」，下一轮接着领。
		if !ugHasBudget(ctx, ugClaimReserve) {
			break
		}
		results = append(results, claimUGActivity(ctx, cfg, headers, a))
		claimedIDs[a.ID] = true
	}
	for _, a := range activities {
		if !a.Enabled || claimedIDs[a.ID] || !worthReporting(a, now) {
			continue
		}
		overview = append(overview, describeActivity(a, now))
	}
	return overview, results, nil
}

// worthReporting 判断一条活动是否值得出现在结果里。
//
// 上游的活动表里常年挂着已结束或与客户端无关的条目（如已过期的
// work_fission_202607），全部铺出来只会淹没真正有用的那两条。只报三类：
// 上游说可领的、已经发过的、由上游自动发放的。
func worthReporting(a ugActivity, now time.Time) bool {
	if a.ID == "" || a.expired(now) {
		return false
	}
	// 要用户自己去认证 / 装客户端 / 发消息的活动不推：插件替他做不了，
	// 报出来只是一行永远不变的「需先完成…」。
	if a.needsManualAction() {
		return false
	}
	return a.grantable() || a.granted() || a.autoGrant()
}

// describeActivity 把一条活动压成总览行（不发请求）。
func describeActivity(a ugActivity, now time.Time) UGResult {
	result := UGResult{Key: "activity:" + a.ID, Label: a.label()}
	if reward := a.rewardText(); reward != "" {
		result.Label = fmt.Sprintf("%s（%s）", a.label(), reward)
	}
	switch {
	case a.expired(now):
		result.Status = UGUnavailable
		result.Message = "活动已结束"
	case a.notStarted(now):
		result.Status = UGUnavailable
		result.Message = "活动未开始"
	case a.granted():
		result.Status = UGAlready
		result.Message = "已发放"
	case a.Type == ugTypeCheckin:
		// 签到不在这条链路上触发，别把它说成「等待本轮触发」。
		result.Status = UGUnavailable
		result.Message = "走每日签到"
	case a.Type == ugTypeNewUser:
		result.Status = UGUnavailable
		result.Message = "注册时已发放，无需重复领取"
	case a.grantable():
		result.Status = UGUnavailable
		result.Message = "可领取，等待本轮触发"
	case a.autoGrant():
		result.Status = UGUnavailable
		result.Message = a.prereqHint()
	default:
		result.Status = UGUnavailable
		result.Message = "当前不可领"
	}
	return result
}

// ── 分享裂变：邀请码、邀请进度、裂变积分领取 ───────────────────────────────
// 裂变端点（三个一组，work_ 与 code_ 同构）。
const (
	EpCodeFissionShareStatus = "/trae/api/v2/ug/code_fission/share_status"
	EpCodeFissionShareLink   = "/trae/api/v2/ug/code_fission/share_link"
	EpCodeFissionGrant       = "/trae/api/v2/ug/code_fission/grant"
)

// fissionActivityCandidates 是待探测的裂变活动 ID。
//
// 只放实测有效的：code_fission_202609 是 2026-09 那期，endTime 为空表示
// 上游没设结束时间。同族的 202608 / 202610 / 202611 / 202612 实测全部回
// 9090「活动暂不可用」，所以不按月推算——按月猜会每个月白打一轮。
// 上游换期时把新 ID 放进来，旧的留着无害（会回 9090 被跳过）。
var fissionActivityCandidates = []string{"code_fission_202609"}

// fissionInvitee 是邀请记录里的一条。
type fissionInvitee struct {
	Name      string `json:"invitee_name"`
	Reward    int64  `json:"inviter_reward_credits"`
	Status    int    `json:"status"`
	CreatedAt int64  `json:"created_at"`
}

// fissionStatus 是一次 share_status 的结果。
type fissionStatus struct {
	Available bool             `json:"-"`
	Invitees  []fissionInvitee `json:"invitees"`
	HasMore   bool             `json:"has_more"`
}

// fetchFissionStatus 查一期的邀请进度。
//
// 第二个返回值表示「这一期活动是否存在」：9090（活动暂不可用）与 9080
// （活动不存在）都算不存在，其余错误按失败上报。
func fetchFissionStatus(ctx context.Context, cfg Config, headers map[string]string, activityID string) (fissionStatus, bool, error) {
	ctx, cancel := ugDynamicCtx(ctx)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"activity_id": activityID, "page_size": 20})
	status, body, errDo := postJSON(ctx, cfg.CheckinHost+EpCodeFissionShareStatus, headers, payload)
	if errDo != nil {
		return fissionStatus{}, false, errDo
	}
	if status == http.StatusUnauthorized {
		return fissionStatus{}, false, &core.Failure{
			Kind:   core.FailureKindCredential,
			Status: status,
			Msg:    "登录态已失效，请重新登录后再领取裂变积分",
		}
	}
	var envelope struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		fissionStatus
	}
	if errJSON := json.Unmarshal(body, &envelope); errJSON != nil {
		return fissionStatus{}, false, fmt.Errorf("share_status 响应无法解析：%s", truncateBody(string(body)))
	}
	message := core.FirstNonEmpty(strings.TrimSpace(envelope.Message), strings.TrimSpace(envelope.Msg))
	if envelope.Code == nil {
		// 200 但没有业务码：网关兜页、`{"data":{}}` 这类都长这样。裂变端点
		// 一律带 code 信封，缺 code 就不算「这一期存在」，否则会把一张空壳
		// 当成活动进行中报给用户。
		return fissionStatus{}, false, nil
	}
	if *envelope.Code != 0 {
		// 9080 / 9090 = 这期不存在或不可用，换下一个候选，不算故障。
		if *envelope.Code == 9080 || *envelope.Code == 9090 {
			return fissionStatus{}, false, nil
		}
		return fissionStatus{}, false, fmt.Errorf("share_status code=%d：%s", *envelope.Code, message)
	}
	result := envelope.fissionStatus
	result.Available = true
	return result, true, nil
}

// claimFissionRewards 探测当前有效的裂变期并领取其中待发的积分。
//
// 每期只发两次请求：先 status（判断这期在不在、有没有待领），
// 有待领才 grant。没待领时 grant 会回 9070「暂无可领取权益」，
// 那不是错误，只是还没有被邀请人达标。
func claimFissionRewards(ctx context.Context, cfg Config, headers map[string]string) ([]UGResult, error) {
	var results []UGResult
	for _, activityID := range fissionActivityCandidates {
		if errCtx := ctx.Err(); errCtx != nil {
			return results, errCtx
		}
		state, available, errStatus := fetchFissionStatus(ctx, cfg, headers, activityID)
		if errStatus != nil {
			if failure, okFailure := errStatus.(*core.Failure); okFailure && failure.Kind == core.FailureKindCredential {
				return results, errStatus
			}
			// 单期查询失败不影响别的期，也不该打断整轮领取。
			continue
		}
		if !available {
			continue
		}
		line := describeFission(state, activityID)
		if len(state.Invitees) == 0 {
			// 还没被邀请人时把邀请码带出来，用户才知道拿什么去分享。
			// 取不到就不提，一次辅助请求不该影响结论。
			if code, _, errCode := fetchFissionInviteCode(ctx, cfg, headers, activityID); errCode == nil && code != "" {
				line.Message = fmt.Sprintf("活动进行中，还没有被邀请人（邀请码 %s）", code)
			}
		}
		results = append(results, line)
		if !fissionHasPending(state) {
			continue
		}
		results = append(results, grantFission(ctx, cfg, headers, activityID))
	}
	return results, nil
}

// fissionHasPending 判断是否有已达标但未发放的邀请。
//
// status=2 是「已发放」（实测历史记录都是 2），其余值（0/1）表示还在
// 流程里，此时调 grant 才有意义。
func fissionHasPending(state fissionStatus) bool {
	for _, invitee := range state.Invitees {
		if invitee.Status != 2 {
			return true
		}
	}
	return false
}

// describeFission 把邀请进度压成一行。
func describeFission(state fissionStatus, activityID string) UGResult {
	result := UGResult{Key: "fission:" + activityID, Label: "分享裂变"}
	if len(state.Invitees) == 0 {
		result.Status = UGUnavailable
		result.Message = "活动进行中，还没有被邀请人"
		return result
	}
	var total int64
	pending := 0
	for _, invitee := range state.Invitees {
		total += invitee.Reward
		if invitee.Status != 2 {
			pending++
		}
	}
	result.Status = UGAlready
	result.Message = fmt.Sprintf("已邀请 %d 人，累计 %d 积分", len(state.Invitees), total)
	if pending > 0 {
		result.Status = UGUnavailable
		result.Message = fmt.Sprintf("已邀请 %d 人，其中 %d 人待发放", len(state.Invitees), pending)
	}
	return result
}

// grantFission 领取裂变积分。
func grantFission(ctx context.Context, cfg Config, headers map[string]string, activityID string) UGResult {
	ctx, cancel := ugDynamicCtx(ctx)
	defer cancel()
	result := UGResult{Key: "fission-grant:" + activityID, Label: "裂变积分"}
	payload, _ := json.Marshal(map[string]string{"activity_id": activityID})
	status, body, errDo := postJSON(ctx, cfg.CheckinHost+EpCodeFissionGrant, headers, payload)
	if errDo != nil {
		result.Status = UGFailed
		result.Message = errDo.Error()
		return result
	}
	var envelope struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if errJSON := json.Unmarshal(body, &envelope); errJSON != nil || envelope.Code == nil {
		result.Status = UGFailed
		result.Message = truncateBody(string(body))
		if result.Message == "" {
			result.Message = fmt.Sprintf("HTTP %d：响应不含业务码", status)
		}
		return result
	}
	result.Code = *envelope.Code
	result.Message = core.FirstNonEmpty(strings.TrimSpace(envelope.Message), strings.TrimSpace(envelope.Msg))
	switch {
	case status == http.StatusOK && *envelope.Code == 0:
		result.Status = UGClaimed
	case status == http.StatusOK && *envelope.Code == 9070:
		// 暂无可领取权益：不是错误，是「还没有达标的被邀请人」。
		result.Status = UGAlready
		result.Message = "暂无可领取权益"
	case status == http.StatusOK:
		result.Status = classifyUGMessage(result.Message)
	default:
		result.Status = UGFailed
		if result.Message == "" {
			result.Message = fmt.Sprintf("HTTP %d", status)
		}
	}
	return result
}

// fetchFissionInviteCode 取当前这期的邀请码（用于分享）。
//
// 邀请码是稳定值，取到一次就该缓存；这里只负责取，不负责缓存。
func fetchFissionInviteCode(ctx context.Context, cfg Config, headers map[string]string, activityID string) (string, string, error) {
	ctx, cancel := ugDynamicCtx(ctx)
	defer cancel()
	payload, _ := json.Marshal(map[string]string{"activity_id": activityID})
	status, body, errDo := postJSON(ctx, cfg.CheckinHost+EpCodeFissionShareLink, headers, payload)
	if errDo != nil {
		return "", "", errDo
	}
	if status == http.StatusUnauthorized {
		return "", "", &core.Failure{
			Kind:   core.FailureKindCredential,
			Status: status,
			Msg:    "登录态已失效，请重新登录后再生成邀请码",
		}
	}
	var envelope struct {
		Code       *int   `json:"code"`
		Message    string `json:"message"`
		Msg        string `json:"msg"`
		InviteCode string `json:"invite_code"`
		ShareURL   string `json:"share_url"`
	}
	if errJSON := json.Unmarshal(body, &envelope); errJSON != nil {
		return "", "", fmt.Errorf("share_link 响应无法解析：%s", truncateBody(string(body)))
	}
	message := core.FirstNonEmpty(strings.TrimSpace(envelope.Message), strings.TrimSpace(envelope.Msg))
	if envelope.Code == nil {
		return "", "", fmt.Errorf("share_link 响应不含业务码：%s", truncateBody(string(body)))
	}
	if *envelope.Code != 0 {
		return "", "", fmt.Errorf("share_link code=%d：%s", *envelope.Code, message)
	}
	return strings.TrimSpace(envelope.InviteCode), strings.TrimSpace(envelope.ShareURL), nil
}
