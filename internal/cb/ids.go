package cb

// 本文件生成请求标识与从请求体里提取会话键。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// NewHexID 生成 32 位十六进制随机 ID（与官方客户端的 messageId 形态一致）。
func NewHexID() string {
	return RandomHex(16)
}

func newHexID() string {
	return NewHexID()
}

// RandomHex 生成 n 字节的随机十六进制串。
func RandomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, errRand := rand.Read(buf); errRand != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("fallback-%d", nextFallbackSeed())))
		if n > len(sum) {
			n = len(sum)
		}
		return hex.EncodeToString(sum[:n])
	}
	return hex.EncodeToString(buf)
}

func oldHexIDInternal() string {
	buf := make([]byte, 16)
	if _, errRand := rand.Read(buf); errRand != nil {
		// crypto/rand 失败极罕见；退回时间派生值也比返回空串安全——
		// 空 messageID 会让上游的会话聚合失效。
		sum := sha256.Sum256([]byte(fmt.Sprintf("fallback-%d", nextFallbackSeed())))
		buf = sum[:16]
	}
	return hex.EncodeToString(buf)
}

var fallbackSeed int64

func nextFallbackSeed() int64 {
	value, errRand := rand.Int(rand.Reader, big.NewInt(1<<62))
	if errRand == nil {
		return value.Int64()
	}
	fallbackSeed++
	return fallbackSeed
}

// ConversationKey 从请求体里提取会话键，用于生成稳定的会话头族。
//
// 识别顺序照抄原实现（粒度由细到粗）：
//  1. metadata.conversation_id / conversationId
//  2. 顶层 conversation_id / conversationId
//  3. prompt_cache_key
//  4. 派生键：system 文本 + 首条 user 消息的哈希
//
// 刻意**不**把 user_id 当会话键：同一用户的所有会话会粘到同一个键上，
// 反而破坏会话隔离。
func ConversationKey(body []byte) string {
	payload := decodeBodyMap(body)
	if payload == nil {
		return ""
	}
	if metadata, okMeta := payload["metadata"].(map[string]any); okMeta {
		for _, key := range []string{"conversation_id", "conversationId"} {
			if value, okValue := metadata[key].(string); okValue && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	for _, key := range []string{"conversation_id", "conversationId"} {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if value, okValue := payload["prompt_cache_key"].(string); okValue && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return deriveConversationKey(payload)
}

// deriveConversationKey 用 system 与首条 user 消息派生会话键。
//
// 通用 OpenAI 客户端不传任何会话标识，派生键让它们也能享受会话聚合。
// 前缀 "d-" 标明这是派生值（便于与客户端显式传的值区分）。
func deriveConversationKey(payload map[string]any) string {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages || len(messages) == 0 {
		return ""
	}
	systemText := ""
	firstUser := ""
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer":
			if systemText == "" {
				systemText = contentSignature(message["content"])
			}
		case "user":
			if firstUser == "" {
				firstUser = contentSignature(message["content"])
			}
		}
	}
	if firstUser == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUser))
	return "d-" + hex.EncodeToString(sum[:])[:16]
}

// contentSignature 把消息内容压成一个稳定的短签名。
//
// 数组形态（多模态）只取文本部分并保留非文本部分的类型，这样
// 图片消息不会与纯文本消息撞成同一个签名。
func contentSignature(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			if text, okText := part["text"].(string); okText {
				parts = append(parts, text)
				continue
			}
			partType, _ := part["type"].(string)
			sum := sha256.Sum256([]byte(partType))
			parts = append(parts, "["+partType+":"+hex.EncodeToString(sum[:4])+"]")
		}
		return strings.Join(parts, "\x1f")
	}
	return ""
}

// TurnKey 返回当前轮次的键（最后一条 user 消息）。
//
// 序号入键是刻意的：连续两次内容相同的「继续」必须被识别为不同轮次，
// 否则第二轮会复用第一轮的请求 ID，后台聚合会把它们合并成一次。
func TurnKey(body []byte) string {
	payload := decodeBodyMap(body)
	if payload == nil {
		return ""
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return ""
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message, okMessage := messages[index].(map[string]any)
		if !okMessage {
			continue
		}
		role, _ := message["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "user") {
			continue
		}
		signature := contentSignature(message["content"])
		if signature == "" {
			continue
		}
		return fmt.Sprintf("u%d:%s", index, signature)
	}
	return ""
}

// RequestIDForKey 从会话键派生一个稳定的请求 ID。
func RequestIDForKey(key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("req|" + key))
	return hex.EncodeToString(sum[:16])
}

// TurnRequestID 从轮次键派生一个稳定的请求 ID。
func TurnRequestID(key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("turn|" + key))
	return hex.EncodeToString(sum[:16])
}

// HasImagePart 报告请求体里是否携带图片。
//
// 用于错误提示：上游对「模型不支持图片」与「图片请求本身有问题」
// 返回的码很近，区分它们才能给出有用的提示。
func HasImagePart(body []byte) bool {
	payload := decodeBodyMap(body)
	if payload == nil {
		return false
	}
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return false
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if !okMessage {
			continue
		}
		parts, okParts := message["content"].([]any)
		if !okParts {
			continue
		}
		for _, part := range parts {
			partMap, okPartMap := part.(map[string]any)
			if !okPartMap {
				continue
			}
			if partType, okType := partMap["type"].(string); okType && partType == "image_url" {
				return true
			}
		}
	}
	return false
}
