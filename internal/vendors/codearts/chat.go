package codearts

// 本文件实现 CodeArts 对话请求转发与华为云 SDK-HMAC-SHA256 签名。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/upstream/signer.go 与 client.go

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"freetier2api-plugin/internal/httpx"
)

type ChatRequest struct {
	Credential *Credential
	Body       []byte
	Stream     bool
}

// Chat 执行一次非流式对话。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	resp, errDo := doCodeArtsRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read codearts response: %w", errRead)
	}

	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}

	return body, nil
}

// ChatStream 执行一次流式对话。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	resp, errDo := doCodeArtsRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, Classify(resp.StatusCode, string(body))
	}

	return resp, nil
}

func doCodeArtsRequest(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	if req.Credential == nil {
		return nil, fmt.Errorf("codearts credential is nil")
	}

	target := strings.TrimRight(baseURL, "/")
	if target == "" {
		target = SnapEngineApiHost
	}

	endpoint := target + EpChatV2

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("create codearts http request: %w", errReq)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	httpReq.Header.Set("Agent-Type", "PromptCenter")
	httpReq.Header.Set("User-Agent", "CodeArtsAgent/5.2.0")

	// 签名
	signHuaweiRequest(httpReq, req.Body, req.Credential)

	timeout := 120 * time.Second
	if req.Stream {
		timeout = 300 * time.Second
	}

	client := httpx.Client(ctx, timeout)
	resp, errCall := client.Do(httpReq)
	if errCall != nil {
		return nil, fmt.Errorf("codearts request failed: %w", errCall)
	}

	return resp, nil
}

// signHuaweiRequest 为请求注入华为云 SDK-HMAC-SHA256 签名。
func signHuaweiRequest(req *http.Request, body []byte, cred *Credential) {
	if cred.AccessKeyID == "" || cred.SecretAccessKey == "" {
		return
	}

	xDate := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Sdk-Date", xDate)
	req.Header.Set("Host", req.URL.Host)
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
		req.Header.Set("X-Auth-Token", cred.SecurityToken)
	}

	payloadHash := sha256Hex(body)
	req.Header.Set("X-Sdk-Content-Sha256", payloadHash)

	canonicalHeadersStr, signedHeadersStr := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		canonicalQuery(req.URL.RawQuery),
		canonicalHeadersStr,
		signedHeadersStr,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"SDK-HMAC-SHA256",
		xDate,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signature := hmacHex(cred.SecretAccessKey, stringToSign)
	req.Header.Set("Authorization", "SDK-HMAC-SHA256 Access="+cred.AccessKeyID+
		", SignedHeaders="+signedHeadersStr+
		", Signature="+signature)
}

func canonicalHeaders(req *http.Request) (headers, signed string) {
	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		v := req.Header.Get(k)
		sb.WriteString(k + ":" + strings.TrimSpace(v) + "\n")
	}
	return sb.String(), strings.Join(keys, ";")
}

func canonicalURI(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	out := strings.Join(segments, "/")
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := vals[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacHex(key, msg string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}
