package main

// 本文件提供「上游 SSE 与宿主契约同形」时的原样转发。
//
// 为什么需要它：WorkBuddy 的 SSE 需要白名单重建、工具名裁剪、id 续传
// （见 internal/vendors/workbuddy/sse.go），Qoder 需要协议转换
// （internal/vendors/qoder/bridge）。而 OpenCode ZEN 与 Cline 的上游
// **本来就是标准 chat-completions SSE**，任何改写都只会引入偏差。
//
// 因此这里只做一件事：把上游的 `data: {...}` 逐帧剥出载荷交给 sink。
// 宿主按插件声明的格式补回 `data: ` 前缀与 `[DONE]`。

import (
	"bufio"
	"io"
	"strings"

	"freetier2api-plugin/internal/core"
)

// sseScannerBuffer 是单帧的上限。
//
// 默认的 64KB 会截断长帧（工具参数、长文本），这里放到 8MB——
// 与 workbuddy 的 sse.go 取同一个值，两处的截断行为应当一致。
const sseScannerBuffer = 8 * 1024 * 1024

// forwardRawSSE 逐帧转发上游 SSE 载荷。
//
// 行为约定（与宿主契约对齐）：
//   - `data: ` 前缀由宿主补，这里只交载荷；
//   - `[DONE]` 是流结束信号，**不转发**（宿主自己会补）；
//   - 非 data 行（注释、event:）原样转发，保持上游帧结构；
//   - 空载荷跳过。
//
// 全程零有效帧不算错误：上游有时返回一条没有任何 delta 的流，
// 把它当错误会让客户端收到无意义的错误帧。
func forwardRawSSE(body io.Reader, sink core.StreamSink) error {
	if body == nil {
		return nil
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), sseScannerBuffer)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			// 注释与 event: 行原样下发，保持上游的帧结构。
			if errEmit := sink.Emit([]byte(line)); errEmit != nil {
				return errEmit
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			return nil
		}
		if payload == "" {
			continue
		}
		if errEmit := sink.Emit([]byte(payload)); errEmit != nil {
			return errEmit
		}
	}
	return scanner.Err()
}
