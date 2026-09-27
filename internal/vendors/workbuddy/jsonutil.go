package workbuddy

// 本文件提供请求体解码的小工具。
//
// 请求体在本包里被反复解析（提取会话键、改写管线、脱敏、判定图片），
// 每次都解一遍完整 JSON 会很浪费。但缓存整个解析结果又会带来一致性问题
// （改写管线会修改 map）。折中：各阶段各解一次，保持每步输入都是原始字节，
// 语义清晰且不会有「谁改了谁」的隐式耦合。

import "encoding/json"

// decodeBodyMap 把请求体解析成 map。解析失败返回 nil（调用方按"无法解析"处理）。
func decodeBodyMap(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil
	}
	return payload
}

// encodeBodyMap 把 map 序列化回请求体。
func encodeBodyMap(payload map[string]any) ([]byte, error) {
	return json.Marshal(payload)
}
