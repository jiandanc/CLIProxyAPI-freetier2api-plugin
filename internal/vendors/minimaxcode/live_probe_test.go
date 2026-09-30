package minimaxcode

// 本文件是手动探针测试：MINIMAX_LIVE=1 时才运行，用于观察上游真实行为。
// 绝不打印凭证内容，只输出截断后的 SSE 帧与模型回复。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/httpx"
)

func liveCredential(t *testing.T) *Credential {
	t.Helper()
	if os.Getenv("MINIMAX_LIVE") == "" {
		t.Skip("MINIMAX_LIVE 未设置，跳过实况探针")
	}
	path := os.Getenv("MINIMAX_LIVE_CREDS")
	if path == "" {
		t.Skip("MINIMAX_LIVE_CREDS 未设置")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read creds: %v", errRead)
	}
	cred, errParse := ParseCredential(raw, RegionCN)
	if errParse != nil {
		t.Fatalf("parse creds: %v", errParse)
	}
	configureDirectHost(t)
	return cred
}

// configureDirectHost 注入直连网络的宿主回调（探针专用，绕过 CPA 宿主桥）。
func configureDirectHost(t *testing.T) {
	t.Helper()
	httpx.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, fmt.Errorf("probe host: unsupported method %s", method)
		}
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		rawPayload, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		if errDecode := json.Unmarshal(rawPayload, &req); errDecode != nil {
			return nil, errDecode
		}
		var bodyReader io.Reader
		if len(req.Body) > 0 {
			bodyReader = strings.NewReader(string(req.Body))
		}
		httpReq, errNew := http.NewRequest(req.Method, req.URL, bodyReader)
		if errNew != nil {
			return nil, errNew
		}
		for k, vs := range req.Headers {
			for _, v := range vs {
				httpReq.Header.Add(k, v)
			}
		}
		client := &http.Client{Timeout: 110 * time.Second}
		resp, errDo := client.Do(httpReq)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, errRead := io.ReadAll(resp.Body)
		if errRead != nil {
			return nil, errRead
		}
		return json.Marshal(map[string]any{
			"StatusCode": resp.StatusCode,
			"Headers":    resp.Header,
			"Body":       respBody,
		})
	})
	t.Cleanup(func() { httpx.Configure(nil) })
}

// dumpRawStream 直接读取 doMessageRequest 返回的上游原始流，逐帧截断打印。
func dumpRawStream(t *testing.T, cred *Credential, payload []byte) (text string, frames int) {
	t.Helper()
	body, modelName, promptTokens, errDo := doMessageRequest(context.Background(), cred, &core.ExecuteRequest{Payload: payload}, "")
	if errDo != nil {
		t.Fatalf("doMessageRequest: %v", errDo)
	}
	defer func() { _ = body.Close() }()
	t.Logf("model=%s promptTokens=%d", modelName, promptTokens)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var textSB strings.Builder
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			if trimmed != "" {
				t.Logf("RAW| %s", trunc(trimmed, 300))
			}
			continue
		}
		frames++
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" || data == "[DONE]" {
			t.Logf("FRAME[%d] %s", frames, data)
			continue
		}
		var payloadMap map[string]any
		if errJSON := json.Unmarshal([]byte(data), &payloadMap); errJSON != nil {
			t.Logf("FRAME[%d] (non-json) %s", frames, trunc(data, 400))
			continue
		}
		if th := findDeepString(payloadMap, thinkingKeys...); th != "" {
			textSB.WriteString(th)
		}
		if tx := findDeepString(payloadMap, contentKeys...); tx != "" {
			textSB.WriteString(tx)
		}
		// 打印完整帧（截断到 1500 字符），观察 role / finish_reason / 内容来源。
		dataStr := string(data)
		t.Logf("FRAME[%d] %s", frames, trunc(dataStr, 1500))
	}
	if errScan := scanner.Err(); errScan != nil && errScan != io.EOF {
		t.Logf("scan err: %v", errScan)
	}
	return textSB.String(), frames
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("...(len=%d)", len(s))
}

// skeleton 输出 JSON 的键结构骨架。
func skeleton(v any) string {
	var b strings.Builder
	writeSkeleton(&b, v, 0)
	return b.String()
}

func writeSkeleton(b *strings.Builder, v any, depth int) {
	if depth > 4 {
		b.WriteString("…")
		return
	}
	switch typed := v.(type) {
	case map[string]any:
		b.WriteString("{")
		first := true
		for k, val := range typed {
			if !first {
				b.WriteString(", ")
			}
			first = false
			b.WriteString(k)
			switch nested := val.(type) {
			case string:
				if len(nested) > 60 {
					b.WriteString(fmt.Sprintf(":%q…", nested[:60]))
				} else if isContentKey(k) {
					b.WriteString(fmt.Sprintf(":%q", nested))
				} else {
					b.WriteString(":s")
				}
			case float64:
				b.WriteString(":n")
			case bool:
				b.WriteString(":b")
			case nil:
				b.WriteString(":null")
			default:
				b.WriteString(":")
				writeSkeleton(b, val, depth+1)
			}
		}
		b.WriteString("}")
	case []any:
		b.WriteString(fmt.Sprintf("[%d]", len(typed)))
		if len(typed) > 0 && depth < 3 {
			b.WriteString("«")
			writeSkeleton(b, typed[0], depth+1)
			b.WriteString("»")
		}
	default:
		b.WriteString("?")
	}
}

func isContentKey(k string) bool {
	for _, key := range append(append([]string{}, contentKeys...), thinkingKeys...) {
		if k == key {
			return true
		}
	}
	return false
}

// TestLiveProbeA 复现：Claude Code 风格请求发给 m3.1-flash，观察原始帧。
func TestLiveProbeA(t *testing.T) {
	cred := liveCredential(t)
	content := "[系统指令] You are Claude Code, Anthropic's official CLI for Claude.\n\n" +
		" 用户：1. 测试子代理和tool"
	payload, _ := json.Marshal(map[string]any{
		"model": "minimax-m3.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": content},
		},
		"stream": true,
	})
	started := time.Now()
	text, frames := dumpRawStream(t, cred, payload)
	t.Logf("耗时 %s, frames=%d, 提取文本 len=%d", time.Since(started), frames, len(text))
	t.Logf("提取文本: %s", trunc(text, 2000))
}

// TestLiveProbeE 走完整 Chat() 路径（带 tools），验证错误透传与工具路径。
func TestLiveProbeE(t *testing.T) {
	cred := liveCredential(t)
	payload, _ := json.Marshal(map[string]any{
		"model": "minimax-m3.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "查一下北京天气"},
		},
		"stream": true,
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "查询指定城市的当前天气",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
					"required":   []any{"city"},
				},
			},
		}},
	})
	resp, errChat := Chat(context.Background(), cred, &core.ExecuteRequest{Payload: payload}, "")
	if errChat != nil {
		t.Logf("Chat 返回错误（余额为 0 时期待透传）: %v", errChat)
		return
	}
	t.Logf("Chat 响应: %s", trunc(string(resp), 3000))
}

// TestLiveProbeD 查询账号额度，确认 50110 是否为余额问题。
func TestLiveProbeD(t *testing.T) {
	cred := liveCredential(t)
	quota, errQuota := FetchQuota(context.Background(), cred, "")
	if errQuota != nil {
		t.Fatalf("FetchQuota: %v", errQuota)
	}
	encoded, _ := json.Marshal(quota)
	t.Logf("quota: %s", string(encoded))
}

// TestLiveProbeC 不带 model 选择（默认 agent 模型），观察与 m3.1-flash 的差异。
func TestLiveProbeC(t *testing.T) {
	cred := liveCredential(t)
	payload, _ := json.Marshal(map[string]any{
		"model": "minimax-agent",
		"messages": []any{
			map[string]any{"role": "user", "content": "用一句话回答：1+1等于几？"},
		},
		"stream": true,
	})
	started := time.Now()
	text, frames := dumpRawStream(t, cred, payload)
	t.Logf("耗时 %s, frames=%d, 提取文本 len=%d", time.Since(started), frames, len(text))
	t.Logf("提取文本: %s", trunc(text, 2000))
}

// TestLiveProbeB 文本工具协议实验：在 prompt 中注入工具与格式说明，看模型是否遵守。
func TestLiveProbeB(t *testing.T) {
	cred := liveCredential(t)
	content := strings.Join([]string{
		"[系统指令] 你是一个通过文本协议调用工具的助手。收到用户请求后，如果需要使用工具，按下面的格式输出工具调用。",
		"",
		"[工具]",
		`[{"name":"get_weather","description":"查询指定城市的当前天气","parameters":{"type":"object","properties":{"city":{"type":"string","description":"城市名"}},"required":["city"]}}]`,
		"",
		"[工具调用格式]",
		"严格按以下格式输出，标签独占一行，arguments 必须是单行 JSON：",
		"<tool_call>",
		`{"name":"get_weather","arguments":{"city":"北京"}}`,
		"</tool_call>",
		"输出工具调用后停止，等待工具结果。",
		"",
		" 用户：1. 北京今天天气怎么样？请调用天气工具查询，然后告诉我结果。",
	}, "\n")
	payload, _ := json.Marshal(map[string]any{
		"model": "minimax-m3.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": content},
		},
		"stream": true,
	})
	started := time.Now()
	text, frames := dumpRawStream(t, cred, payload)
	t.Logf("耗时 %s, frames=%d, 提取文本 len=%d", time.Since(started), frames, len(text))
	t.Logf("提取文本: %s", trunc(text, 3000))
}
