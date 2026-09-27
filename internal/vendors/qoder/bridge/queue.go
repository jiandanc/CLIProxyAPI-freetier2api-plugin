package bridge

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

// 排队策略由插件配置驱动（宿主热重载插件配置时会更新），所以不能是编译期常量。
var (
	queuePolicyMu    sync.RWMutex
	queueMaxWaits    = DefaultQueueMaxWaits
	queueWaitSeconds int // > 0 时覆盖上游建议的等待时长
)

// SetQueuePolicy 应用配置里的排队策略：maxWaits 为 0 表示完全不等待（立即返回排队错误）。
func SetQueuePolicy(maxWaits, waitSeconds int) {
	if maxWaits < 0 {
		maxWaits = 0
	}
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	queuePolicyMu.Lock()
	queueMaxWaits = maxWaits
	queueWaitSeconds = waitSeconds
	queuePolicyMu.Unlock()
}

// QueueMaxWaits 返回当前允许的排队等待次数。
func QueueMaxWaits() int {
	queuePolicyMu.RLock()
	defer queuePolicyMu.RUnlock()
	return queueMaxWaits
}

// queueWaitFor 计算这次排队该等多久：配置覆盖优先，否则用上游建议值（受 MaxQueueWait 约束）。
func queueWaitFor(signal QueueSignal) time.Duration {
	queuePolicyMu.RLock()
	override := queueWaitSeconds
	queuePolicyMu.RUnlock()
	if override > 0 {
		return time.Duration(override) * time.Second
	}
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
