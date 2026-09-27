package workbuddy

// 本文件构造四类出站请求头。
//
// 为什么值得单独成文件：上游风控对请求头极其敏感——UA 的平台段写错、
// 少了 X-CodeBuddy-Request、会话头族不全会得到 403 code=11140 "request illegal"，
// 且报错信息完全不指向真正的原因。集中管理是唯一可靠的做法。

import (
	"net/http"
	"strings"
)

// ChatMeta 是与单次对话关联的会话头族。
//
// 由调用方在**轮转循环之外**生成一次，循环内复用：上游后台按
// X-Conversation-Request-ID 聚合同一轮请求，每次重试都换 ID 会被拆成多次会话。
type ChatMeta struct {
	// ConversationID 是多轮会话标识（可为空，为空时不发该头）。
	ConversationID string
	// ConversationRequestID 是轮级聚合主键（必发）。
	ConversationRequestID string
	// MessageID 是单条消息标识（每条独立）。
	MessageID string
	// TraceID 是入站透传的链路 ID（为空时回落 ConversationRequestID）。
	TraceID string
}

// applyCommonHeaders 写入所有 API 共享的头。
func applyCommonHeaders(req *http.Request, c *Client, cred *Credential) {
	endpoints := GetEndpoints(cred.Realm())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", endpoints.Origin)
	req.Header.Set("Referer", endpoints.Origin+"/")
	req.Header.Set("User-Agent", c.userAgent(cred.Realm()))
	req.Header.Set("Accept-Language", endpoints.AcceptLanguage)
	// 这个头是上游识别"来自官方客户端"的基础判据之一，缺失会被风控拦。
	req.Header.Set("X-CodeBuddy-Request", "1")

	if uid := cred.UIDValue(); uid != "" {
		req.Header.Set("X-Machine-ID", deriveID(uid, "machine"))
		req.Header.Set("X-Session-ID", deriveID(uid, "session"))
	}
}

// applyAuthHeaders 写入凭证相关的头。
//
// 凭证缺失时不发空 Bearer（那会被当成"提供了无效凭证"），
// 而是发上游约定的 X-No-* 标记，让它给出更准确的错误。
func applyAuthHeaders(req *http.Request, cred *Credential) {
	if token := cred.AccessTokenValue(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if uid := cred.UIDValue(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
}

// userAgent 构造三段式 UA。
//
// 平台段在 global 域**必须**是 "WorkBuddy AI"：送成 "WorkBuddy" 会触发
// 上游 403 code=11140 "request illegal"。官方客户端不做任何 UA 随机化，
// 因此这里也保持确定性（随机化反而更容易被风控标记）。
func (c *Client) userAgent(region Region) string {
	if override := strings.TrimSpace(c.opts.UserAgent); override != "" {
		return override
	}
	clientVersion := c.opts.ClientVersion
	if clientVersion == "" {
		clientVersion = defaultClientVersion
	}
	cliVersion := c.opts.CLIVersion
	if cliVersion == "" {
		cliVersion = defaultCLIVersion
	}
	platform := GetEndpoints(region).PlatformLabel
	return "WorkBuddy/" + clientVersion + " " + platform + "/" + clientVersion + " CLI/" + cliVersion
}

// applyChatHeaders 写入对话请求专有的头。
func (c *Client) applyChatHeaders(req *http.Request, cred *Credential, meta ChatMeta, clientIP string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/event-stream")

	// 企业与域信息：CN 与 global 的形态不同，写错会被拒。
	if cred.Realm().IsGlobal() {
		req.Header.Set("X-No-Enterprise-Id", "1")
		req.Header.Set("X-Domain", strings.TrimSuffix(GetEndpoints(RegionGlobal).ChatBase, "/"))
	} else {
		if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", enterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if domain := cred.DomainValue(); domain != "" {
			req.Header.Set("X-Domain", domain)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	}

	applyAttributionHeaders(req, c)
	applyConversationHeaders(req, meta)

	if token := c.DeviceToken(cred); token != "" {
		req.Header.Set("X-Device-Token", token)
	}
	if c.opts.PassthroughIP && strings.TrimSpace(clientIP) != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
		req.Header.Set("X-Real-IP", clientIP)
		req.Header.Set("X-Client-IP", clientIP)
	}
}

// applyAttributionHeaders 写入客户端归属头。
//
// 默认四个头一起给，把请求标识成 WorkBuddy 客户端。
// client_name 显式配成 "SaaS" 时还原旧行为（只发 X-Product），保留这个开关
// 是为了在归属头引起风控变化时能快速回退。
func applyAttributionHeaders(req *http.Request, c *Client) {
	clientVersion := c.opts.ClientVersion
	if clientVersion == "" {
		clientVersion = defaultClientVersion
	}
	name := strings.TrimSpace(c.opts.ClientName)
	if name == "" {
		name = defaultClientName
	}
	if strings.EqualFold(name, "SaaS") {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", clientVersion)
	req.Header.Set("X-Product", name)
}

// applyConversationHeaders 写入会话头族。
//
// 这一族头看似冗余（多个头取同一个值），但上游后台按它们做请求聚合与链路追踪，
// 缺任意一个都会让会话在后台被拆散。
func applyConversationHeaders(req *http.Request, meta ChatMeta) {
	conversationRequestID := strings.TrimSpace(meta.ConversationRequestID)
	messageID := strings.TrimSpace(meta.MessageID)
	if messageID == "" {
		messageID = newHexID()
	}
	if conversationRequestID == "" {
		conversationRequestID = messageID
	}
	if conversationID := strings.TrimSpace(meta.ConversationID); conversationID != "" {
		req.Header.Set("X-Conversation-ID", conversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", conversationRequestID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", conversationRequestID)

	traceID := strings.TrimSpace(meta.TraceID)
	if !validTraceID(traceID) {
		traceID = conversationRequestID
	}
	req.Header.Set("X-Trace-ID", traceID)

	b3TraceID := conversationRequestID
	if !validTraceID(b3TraceID) {
		b3TraceID = messageID
	}
	req.Header.Set("X-B3-TraceId", b3TraceID)
	req.Header.Set("X-B3-SpanId", spanFromMessageID(messageID))
	req.Header.Set("X-B3-Sampled", "1")
}

// spanFromMessageID 从消息 ID 截出 16 位作为 B3 span id。
func spanFromMessageID(messageID string) string {
	if len(messageID) >= 16 {
		return messageID[:16]
	}
	return messageID
}

// validTraceID 校验 trace id：必须是 16 或 32 位 hex（大小写均可）。
//
// 上游对非法 trace id 会直接拒绝，因此入站透传的值必须先校验再使用。
func validTraceID(value string) bool {
	if len(value) != 16 && len(value) != 32 {
		return false
	}
	for _, char := range value {
		isDigit := char >= '0' && char <= '9'
		isLower := char >= 'a' && char <= 'f'
		isUpper := char >= 'A' && char <= 'F'
		if !isDigit && !isLower && !isUpper {
			return false
		}
	}
	return true
}

// applyBillingHeaders 写入计费域与成长域请求的头。
//
// 与对话头的差别：没有会话头族与归属四头，UA 是单段 "WorkBuddy/<version>"。
func (c *Client) applyBillingHeaders(req *http.Request, cred *Credential) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	// 计费域用单段 UA；显式配 SaaS 时不设 UA（旧行为）。
	if name := strings.TrimSpace(c.opts.ClientName); !strings.EqualFold(name, "SaaS") && strings.TrimSpace(c.opts.UserAgent) == "" {
		clientVersion := c.opts.ClientVersion
		if clientVersion == "" {
			clientVersion = defaultClientVersion
		}
		req.Header.Set("User-Agent", "WorkBuddy/"+clientVersion)
	}

	if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	}
	if domain := cred.DomainValue(); domain != "" {
		req.Header.Set("X-Domain", domain)
	}
	if token := c.DeviceToken(cred); token != "" {
		req.Header.Set("X-Device-Token", token)
	}
}

// applyMPHeaders 在计费头之上叠加小程序指纹头。
//
// 小程序口径的任务列表 / accept / claim 全链路都必须带这组头，
// 缺头时上游会把请求当成默认口径：列表不下发 mp 任务、accept 报 task not found。
func applyMPHeaders(req *http.Request) {
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
}

// applyDesktopHeaders 写入桌面指纹族请求的头。
//
// 桌面族用于成长任务的埋点上报：上游按 UA 与 X-Domain 把事件归属到
// "桌面客户端"，与 CLI 族的事件分开计分。
func (c *Client) applyDesktopHeaders(req *http.Request, cred *Credential) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUA())
	req.Header.Set("X-Domain", strings.TrimSuffix(GetEndpoints(cred.Realm()).ChatBase, "/"))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", requestIDFor(cred))
}

// desktopUA 返回桌面指纹族的 UA。
func desktopUA() string {
	return "WorkBuddy/" + DesktopClientVersion + " WorkBuddy/" + DesktopClientVersion + " CLI/" + defaultCLIVersion
}

// requestIDFor 生成一个带纳秒后缀的请求 ID（桌面族要求每次不同）。
func requestIDFor(cred *Credential) string {
	return deriveID(cred.UIDValue(), "req") + newHexID()[:6]
}

// applyWebHeaders 写入 web 指纹族请求的头（浏览器形态）。
func applyWebHeaders(req *http.Request, cred *Credential, pageURL string) {
	endpoints := GetEndpoints(RegionCN)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", endpoints.WebBase)
	req.Header.Set("Referer", pageURL)
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("x-client-platform", "web")
	applyAuthHeaders(req, cred)
	if enterpriseID := cred.EnterpriseIDValue(); enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	}
	if domain := cred.DomainValue(); domain != "" {
		req.Header.Set("X-Domain", domain)
	}
}

// applyV3ConfigHeaders 写入 /v3/config 专有的头。
//
// UA 是这里的风控闸门：过旧会拿到精简目录，或直接 400 code=12403。
func (c *Client) applyV3ConfigHeaders(req *http.Request, cred *Credential, ua string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Domain", v3ConfigDomain(cred))
}

// v3ConfigDomain 决定 /v3/config 的 X-Domain 取值。
func v3ConfigDomain(cred *Credential) string {
	domain := strings.TrimSpace(cred.DomainValue())
	domain = strings.TrimPrefix(domain, "https://")
	domain = strings.TrimPrefix(domain, "http://")
	domain = strings.TrimSuffix(domain, "/")
	if domain != "" {
		return domain
	}
	base := GetEndpoints(cred.Realm()).ChatBase
	base = strings.TrimPrefix(base, "https://")
	base = strings.TrimPrefix(base, "http://")
	return strings.TrimSuffix(base, "/")
}

// ExtractClientIP 从入站请求头里取客户端 IP（X-Forwarded-For 首段，回落 X-Real-IP）。
//
// 只在 PassthroughIP 开启时使用；透传客户端 IP 会给上游提供额外的关联信息，
// 因此默认关闭。
func ExtractClientIP(header http.Header) string {
	if header == nil {
		return ""
	}
	if forwarded := strings.TrimSpace(header.Get("X-Forwarded-For")); forwarded != "" {
		if idx := strings.IndexByte(forwarded, ','); idx >= 0 {
			forwarded = forwarded[:idx]
		}
		return strings.TrimSpace(forwarded)
	}
	return strings.TrimSpace(header.Get("X-Real-IP"))
}
