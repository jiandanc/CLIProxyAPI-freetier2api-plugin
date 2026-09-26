package main

// 本文件提供包内共享的小工具：JSON 编解码与时间格式化。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// decodeJSONMap 把字节解析成 map；失败返回 nil（调用方按「无法解析」处理）。
func decodeJSONMap(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil
	}
	return payload
}

// encodeJSONMap 把 map 序列化回字节；失败时返回 fallback（保持请求体不变）。
func encodeJSONMap(payload map[string]any, fallback []byte) []byte {
	encoded, errEncode := json.Marshal(payload)
	if errEncode != nil {
		return fallback
	}
	return encoded
}

// nowRFC3339 返回当前时间的 RFC3339 字符串。
func nowRFC3339() string { return nowFunc().Format(time.RFC3339) }

// nowUnixMs 返回当前毫秒时间戳。
func nowUnixMs() int64 { return nowFunc().UnixMilli() }

// newHexID 生成 32 位十六进制随机 ID（与官方客户端的 messageId 形态一致）。
func newHexID() string {
	buffer := make([]byte, 16)
	if _, errRand := rand.Read(buffer); errRand != nil {
		// crypto/rand 失败极罕见；退回时间派生值也比返回空串安全。
		sum := sha256.Sum256([]byte(fmt.Sprintf("fallback-%d", nowFunc().UnixNano())))
		buffer = sum[:16]
	}
	return hex.EncodeToString(buffer)
}
