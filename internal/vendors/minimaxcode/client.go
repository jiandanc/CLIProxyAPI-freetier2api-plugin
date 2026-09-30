package minimaxcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/httpx"
)

// Target 是一次签名的 MiniMax 上游请求。
type Target struct {
	Method  string
	URL     string
	SignURL string
	UnixMS  int64
	Body    string
	Checkin bool
}

// AgentInfo 描述 MiniMax 账号下的一个 Agent。
type AgentInfo struct {
	ID            string `json:"name"`
	Role          string `json:"agent_role"`
	RootSessionID string `json:"root_session_id"`
}

// buildAgentQuery 构建 agent 族接口的查询字符串。
func buildAgentQuery(cred *Credential, unixMS int64) string {
	cred.EnsureIdentifiers()
	width := cred.ScreenWidth
	if width == 0 {
		width = 1920
	}
	height := cred.ScreenHeight
	if height == 0 {
		height = 1080
	}

	if cred.AuthMode == "oauth" {
		pairs := [][2]string{
			{"device_platform", "web"},
			{"biz_id", "3"},
			{"app_id", "3001"},
			{"version_code", "22201"},
			{"unix", strconv.FormatInt(unixMS, 10)},
			{"timezone_offset", "28800"},
			{"is_desktop", "1"},
			{"desktop_version", "3.0.73"},
			{"sys_language", "en"},
			{"lang", "en"},
			{"uuid", cred.UUID},
			{"device_id", cred.DeviceID},
			{"os_name", "macOS"},
			{"browser_name", "Chrome"},
			{"device_memory", "16"},
			{"cpu_core_num", "8"},
			{"browser_language", "zh-CN"},
			{"browser_platform", "MacIntel"},
			{"user_id", cred.UserID},
			{"screen_width", strconv.Itoa(width)},
			{"screen_height", strconv.Itoa(height)},
			{"client", "desktop"},
			{"region", "en"},
		}
		var parts []string
		for _, p := range pairs {
			// desktop_version 即使为空也保留
			if p[1] != "" || p[0] == "desktop_version" {
				parts = append(parts, p[0]+"="+EncodeURIComponent(p[1]))
			}
		}
		return strings.Join(parts, "&")
	}

	// Web JWT 模式
	pairs := [][2]string{
		{"device_platform", "web"},
		{"biz_id", "3"},
		{"app_id", "3001"},
		{"version_code", "22201"},
		{"unix", strconv.FormatInt(unixMS, 10)},
		{"timezone_offset", "28800"},
		{"sys_language", "en"},
		{"lang", "en"},
		{"uuid", cred.UUID},
		{"device_id", cred.DeviceID},
		{"os_name", "Windows"},
		{"browser_name", "Chrome"},
		{"device_memory", "16"},
		{"cpu_core_num", "8"},
		{"browser_language", "zh-CN"},
		{"browser_platform", "Win32"},
		{"user_id", cred.UserID},
		{"screen_width", strconv.Itoa(width)},
		{"screen_height", strconv.Itoa(height)},
		{"token", cred.AccessToken},
		{"client", "web"},
		{"region", "en"},
	}
	var parts []string
	for _, p := range pairs {
		if p[1] != "" {
			parts = append(parts, p[0]+"="+EncodeURIComponent(p[1]))
		}
	}
	return strings.Join(parts, "&")
}

// buildSigninQuery 构建签到、额度与账号信息接口的查询字符串。
func buildSigninQuery(cred *Credential, unixMS int64, forSignature bool) string {
	cred.EnsureIdentifiers()
	width := cred.ScreenWidth
	if width == 0 {
		width = 1920
	}
	height := cred.ScreenHeight
	if height == 0 {
		height = 1080
	}

	userID := cred.UserID
	if userID == "" {
		userID = "0"
	}

	tokenParam := ""
	if cred.AuthMode != "oauth" {
		tokenParam = cred.AccessToken
	}

	pairs := [][2]string{
		{"device_platform", "web"},
		{"biz_id", "3"},
		{"app_id", "3001"},
		{"version_code", "22201"},
		{"unix", strconv.FormatInt(unixMS, 10)},
		{"timezone_offset", "28800"},
		{"sys_language", "en"},
		{"lang", "en"},
		{"uuid", cred.UUID},
		{"device_id", cred.DeviceID},
		{"os_name", "Windows"},
		{"browser_name", "Chrome"},
		{"device_memory", "16"},
		{"cpu_core_num", "8"},
		{"browser_language", "en-US"},
		{"browser_platform", "Win32"},
		{"user_id", userID},
		{"op_ticket", "undefined"},
		{"screen_width", strconv.Itoa(width)},
		{"screen_height", strconv.Itoa(height)},
		{"token", tokenParam},
		{"client", "web"},
	}

	var parts []string
	for _, p := range pairs {
		if p[0] == "op_ticket" && !forSignature {
			continue
		}
		if p[1] == "" && p[0] != "op_ticket" {
			continue
		}
		parts = append(parts, p[0]+"="+FormEncode(p[1]))
	}
	return strings.Join(parts, "&")
}

// buildAgentTarget 构建对 Agent 族端点的签名目标。
func buildAgentTarget(cred *Credential, baseURL, path, method, body string, streamHost bool) Target {
	unixMS := time.Now().UnixMilli()
	query := buildAgentQuery(cred, unixMS)
	wire := baseURL + path
	if strings.Contains(wire, "?") {
		wire += "&" + query
	} else {
		wire += "?" + query
	}

	var signURL string
	if streamHost {
		signURL = wire
	} else {
		signURL = path
		if strings.Contains(signURL, "?") {
			signURL += "&" + query
		} else {
			signURL += "?" + query
		}
	}

	return Target{
		Method:  method,
		URL:     wire,
		SignURL: signURL,
		UnixMS:  unixMS,
		Body:    body,
		Checkin: false,
	}
}

// buildSigninTarget 构建对 Signin/Quota/UserInfo 端点的签名目标。
func buildSigninTarget(cred *Credential, baseURL, path, method, body string) Target {
	unixMS := time.Now().UnixMilli()
	signQuery := buildSigninQuery(cred, unixMS, true)
	wireQuery := buildSigninQuery(cred, unixMS, false)

	signURL := path + "?" + signQuery
	wireURL := baseURL + path + "?" + wireQuery

	return Target{
		Method:  method,
		URL:     wireURL,
		SignURL: signURL,
		UnixMS:  unixMS,
		Body:    body,
		Checkin: true,
	}
}

// buildHeaders 组装出站 HTTP 请求头。
func buildHeaders(cred *Credential, target Target, origin string) http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/")

	unixSec := target.UnixMS / 1000
	h.Set("X-Timestamp", strconv.FormatInt(unixSec, 10))
	h.Set("X-Signature", XSignature(unixSec, target.Body))

	yyBody := target.Body
	if target.Checkin {
		yyBody = "{}"
		h.Set("Accept", "*/*")
	} else if cred.AuthMode == "oauth" {
		h.Set("Accept", "application/json, text/plain, */*")
	} else {
		h.Set("Accept", "text/event-stream, application/json, */*")
	}

	h.Set("YY", YYSignature(target.SignURL, yyBody, target.UnixMS))

	if cred.AuthMode == "oauth" {
		h.Set("User-Agent", DesktopUA)
		h.Set("Authorization", "Bearer "+cred.AccessToken)
	} else {
		h.Set("User-Agent", WebUA)
		if cred.AccessToken != "" {
			h.Set("Token", cred.AccessToken)
		}
		h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	}
	return h
}

// doRequest 发起一次已签名的 HTTP 请求。
func doRequest(ctx context.Context, cred *Credential, target Target, origin string, timeout time.Duration) (*http.Response, error) {
	var bodyReader io.Reader
	if target.Body != "" {
		bodyReader = bytes.NewReader([]byte(target.Body))
	}
	req, errNew := http.NewRequestWithContext(ctx, target.Method, target.URL, bodyReader)
	if errNew != nil {
		return nil, fmt.Errorf("create request: %w", errNew)
	}
	req.Header = buildHeaders(cred, target, origin)
	return httpx.Client(ctx, timeout).Do(req)
}

// fetchUserInfo 获取当前账号 realUserID 和展示标识。
func fetchUserInfo(ctx context.Context, cred *Credential, baseURLOverride string) (userID, name, email string, err error) {
	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildSigninTarget(cred, base, EpUserInfo, http.MethodGet, "")
	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return "", "", "", errDo
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("fetch user info HTTP %d", resp.StatusCode)
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return "", "", "", errDec
	}

	core := raw
	if data, ok := raw["data"].(map[string]any); ok {
		core = data
	}
	if ui, ok := core["userInfo"].(map[string]any); ok {
		core = ui
	}

	realUserID := getString(core, "realUserID", "real_user_id", "userID", "user_id")
	accName := getString(core, "name", "username")
	accEmail := getString(core, "email", "phone")

	if realUserID != "" {
		cred.UserID = realUserID
	}
	if accEmail != "" && cred.Email == "" {
		cred.Email = accEmail
	}
	if accName != "" && cred.Label == "" {
		cred.Label = accName
	}

	return realUserID, accName, accEmail, nil
}

// fetchAgents 获取账号名下的 Agent 列表并挑出适合对话的 AgentID。
func fetchAgents(ctx context.Context, cred *Credential, baseURLOverride string) (string, error) {
	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildAgentTarget(cred, base, EpAgentList, http.MethodGet, "", false)
	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return "", errDo
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch agent list HTTP %d", resp.StatusCode)
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return "", errDec
	}

	var agents []AgentInfo
	core := raw
	if data, ok := raw["data"].(map[string]any); ok {
		core = data
	}
	if rawAgents, ok := core["agents"].([]any); ok {
		for _, a := range rawAgents {
			if am, ok := a.(map[string]any); ok {
				agents = append(agents, AgentInfo{
					ID:            getString(am, "name", "id"),
					Role:          getString(am, "agent_role", "role"),
					RootSessionID: getString(am, "root_session_id"),
				})
			}
		}
	}

	// 优选 mavis，其次 general，最后任意有 ID 的 agent
	for _, a := range agents {
		if a.Role == "mavis" && a.ID != "" {
			return a.ID, nil
		}
	}
	for _, a := range agents {
		if a.Role == "general" && a.ID != "" {
			return a.ID, nil
		}
	}
	for _, a := range agents {
		if a.ID != "" {
			return a.ID, nil
		}
	}
	return "", nil
}

// prepare 执行 Agent 启动握手（/config、/agent、/channel/connections）。
func prepare(ctx context.Context, cred *Credential, baseURLOverride string) error {
	base := AgentHostFor(cred.Region, baseURLOverride)

	// 1. /config 接口
	targetCfg := buildAgentTarget(cred, base, EpConfig, http.MethodGet, "", false)
	if resp, err := doRequest(ctx, cred, targetCfg, base, 20*time.Second); err == nil {
		_ = resp.Body.Close()
	}

	// 2. /agent 列表解析
	if cred.AgentID == "" {
		if agentID, err := fetchAgents(ctx, cred, baseURLOverride); err == nil && agentID != "" {
			cred.AgentID = agentID
		}
	}

	// 3. /channel/connections 接口
	targetConn := buildAgentTarget(cred, base, EpConnections, http.MethodGet, "", false)
	if resp, err := doRequest(ctx, cred, targetConn, base, 20*time.Second); err == nil {
		_ = resp.Body.Close()
	}

	return nil
}

// createSession 打开一个独立的对话 session。
func createSession(ctx context.Context, cred *Credential, baseURLOverride string) (string, error) {
	if cred.UserID == "" {
		_, _, _, _ = fetchUserInfo(ctx, cred, baseURLOverride)
	}
	if cred.AgentID == "" {
		_ = prepare(ctx, cred, baseURLOverride)
	}
	if cred.AgentID == "" {
		return "", fmt.Errorf("minimax agent_id is unknown and could not be discovered")
	}

	base := AgentHostFor(cred.Region, baseURLOverride)
	path := fmt.Sprintf(EpSession, cred.AgentID)
	bodyBytes, _ := json.Marshal(map[string]any{
		"agent_id":     cred.AgentID,
		"worktreeMode": false,
	})

	target := buildAgentTarget(cred, base, path, http.MethodPost, string(bodyBytes), false)
	resp, errDo := doRequest(ctx, cred, target, base, 30*time.Second)
	if errDo != nil {
		return "", errDo
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("minimax session HTTP %d: token invalid or expired", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("minimax session HTTP %d: %s", resp.StatusCode, string(body))
	}

	var raw map[string]any
	if errDec := json.NewDecoder(resp.Body).Decode(&raw); errDec != nil {
		return "", fmt.Errorf("decode create session response: %w", errDec)
	}

	sessionID := findDeepString(raw, "session_id", "sessionId", "id", "sessionID")
	if sessionID == "" {
		rawBytes, _ := json.Marshal(raw)
		return "", fmt.Errorf("no session_id in response: %s", string(rawBytes))
	}

	return sessionID, nil
}

func findDeepString(data any, keys ...string) string {
	switch v := data.(type) {
	case map[string]any:
		for _, k := range keys {
			if s, ok := v[k].(string); ok && s != "" {
				return s
			}
		}
		for _, val := range v {
			if found := findDeepString(val, keys...); found != "" {
				return found
			}
		}
	case []any:
		for _, item := range v {
			if found := findDeepString(item, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}
