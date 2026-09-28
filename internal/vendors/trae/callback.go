package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/httpx"
	"freetier2api-plugin/internal/logger"
)

// PendingSession 记录一次进行中的 Trae 或 Trae Solo 登录。
type PendingSession struct {
	SessionID string
	VendorID  string // trae-cn, trae-global, trae-solo
	Region    Region
	IsSolo    bool
	MachineID string
	DeviceID  string
	TraceID   string
	State     string // "pending", "success", "failed"
	AuthData  *pluginapi.AuthData
	ErrMsg    string
	CreatedAt time.Time
}

var (
	callbackServerMu sync.Mutex
	callbackServerOn bool
	pendingStoreMu   sync.Mutex
	pendingStore     = map[string]*PendingSession{}
)

// EnsureCallbackServer 保证本机 18080 回调监听服务已启动。
func EnsureCallbackServer() {
	callbackServerMu.Lock()
	defer callbackServerMu.Unlock()
	if callbackServerOn {
		return
	}

	listener, err := net.Listen("tcp", "127.0.0.1:18080")
	if err != nil {
		logger.Error("trae: 监听 127.0.0.1:18080 失败（可能有其他实例在运行）: %v", err)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", handleAuthorizeCallback)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("trae: 本地授权回调监听已启动: http://127.0.0.1:18080/authorize")
		if errServe := srv.Serve(listener); errServe != nil && errServe != http.ErrServerClosed {
			logger.Error("trae: 回调服务退出: %v", errServe)
		}
	}()

	callbackServerOn = true
}

// RegisterPending 注册一个进行中的登录会话。
func RegisterPending(s *PendingSession) {
	pendingStoreMu.Lock()
	defer pendingStoreMu.Unlock()
	gcPendingLocked()
	pendingStore[s.SessionID] = s
}

// GetPending 取出登录会话。
func GetPending(sessionID string) (*PendingSession, bool) {
	pendingStoreMu.Lock()
	defer pendingStoreMu.Unlock()
	s, ok := pendingStore[sessionID]
	if !ok {
		return nil, false
	}
	if time.Since(s.CreatedAt) > 15*time.Minute {
		delete(pendingStore, sessionID)
		return nil, false
	}
	return s, true
}

// RemovePending 删除登录会话。
func RemovePending(sessionID string) {
	pendingStoreMu.Lock()
	delete(pendingStore, sessionID)
	pendingStoreMu.Unlock()
}

// OwnsSession 检查会话是否归属于该供应商。
func OwnsSession(sessionID, vendorID string) bool {
	norm := strings.ToLower(strings.TrimSpace(sessionID))
	normVendor := strings.ToLower(strings.TrimSpace(vendorID))

	// 前缀匹配快速判定
	if normVendor == "trae-cn" && strings.HasPrefix(norm, "trae_cn_") {
		return true
	}
	if normVendor == "trae-global" && strings.HasPrefix(norm, "trae_global_") {
		return true
	}
	if normVendor == "trae-solo" && (strings.HasPrefix(norm, "traesolo_") || strings.HasPrefix(norm, "trae_solo_")) {
		return true
	}

	pendingStoreMu.Lock()
	defer pendingStoreMu.Unlock()
	s, ok := pendingStore[sessionID]
	if !ok {
		return false
	}
	if time.Since(s.CreatedAt) > 15*time.Minute {
		delete(pendingStore, sessionID)
		return false
	}
	return strings.EqualFold(s.VendorID, vendorID)
}

func gcPendingLocked() {
	now := time.Now()
	for id, s := range pendingStore {
		if now.Sub(s.CreatedAt) > 15*time.Minute {
			delete(pendingStore, id)
		}
	}
}

// MachineTraceID 从 machineID+deviceID 派生 16 位 trace ID。
func MachineTraceID(machineID, deviceID string) string {
	h := machineID + deviceID
	if len(h) >= 16 {
		return h[len(h)-16:]
	}
	return strings.Repeat("0", 16-len(h)) + h
}

// CallbackInfo 回调解析结果。
type CallbackInfo struct {
	RefreshToken string
	AccessToken  string
	UID          string
	Nickname     string
	EnterpriseID string
	ExpiresAt    int64
	TraceID      string
	MachineID    string
	DeviceID     string
}

// ParseCallback 解析 /authorize 接收到的链接。
func ParseCallback(rawURL string) (*CallbackInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("empty callback url")
	}
	if !strings.HasPrefix(rawURL, "http") {
		rawURL = "http://127.0.0.1" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback url: %w", err)
	}
	q := u.Query()
	info := &CallbackInfo{
		RefreshToken: q.Get("refreshToken"),
		TraceID:      q.Get("loginTraceID"),
		MachineID:    q.Get("machine_id"),
		DeviceID:     q.Get("device_id"),
	}

	userInfo := parseJSONParam(q.Get("userInfo"))
	info.UID = getString(userInfo, "UserID")
	info.Nickname = getString(userInfo, "ScreenName")
	info.EnterpriseID = getString(userInfo, "TenantID")

	userJwt := parseJSONParam(q.Get("userJwt"))
	jwtToken := getString(userJwt, "Token")
	jwtRefresh := getString(userJwt, "RefreshToken")

	if info.RefreshToken == "" {
		info.RefreshToken = jwtRefresh
	}
	if info.RefreshToken == "" {
		info.AccessToken = jwtToken
		if exp := getInt64Param(userJwt, "TokenExpireAt"); exp > 0 {
			if exp > 1e12 {
				exp /= 1000
			}
			info.ExpiresAt = exp
		}
	}

	if info.RefreshToken == "" && info.AccessToken == "" {
		return nil, fmt.Errorf("callback missing refreshToken and userJwt.Token")
	}

	return info, nil
}

func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func getInt64Param(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func handleAuthorizeCallback(w http.ResponseWriter, r *http.Request) {
	info, errParse := ParseCallback(r.URL.String())
	if errParse != nil {
		renderCallbackHTML(w, http.StatusBadRequest, "授权回调解析失败", errParse.Error())
		return
	}

	// 匹配正在等待的 pending 登录会话
	var matched *PendingSession
	pendingStoreMu.Lock()
	// 1. 优先按 TraceID / MachineID 精确匹配
	for _, s := range pendingStore {
		if s.State != "pending" {
			continue
		}
		if (info.TraceID != "" && s.TraceID == info.TraceID) ||
			(info.MachineID != "" && s.MachineID == info.MachineID) {
			matched = s
			break
		}
	}
	// 2. 兜底匹配最近 15 分钟内创建的 pending 会话
	if matched == nil {
		var latest *PendingSession
		for _, s := range pendingStore {
			if s.State == "pending" && time.Since(s.CreatedAt) <= 15*time.Minute {
				if latest == nil || s.CreatedAt.After(latest.CreatedAt) {
					latest = s
				}
			}
		}
		matched = latest
	}
	pendingStoreMu.Unlock()

	if matched == nil {
		renderCallbackHTML(w, http.StatusNotFound, "未找到对应的登录会话", "登录会话可能已过期或已被处理，请返回控制台重新发起登录。")
		return
	}

	// 执行 Token 换取
	authHost := "https://api.trae.com.cn"
	clientID := "trae_login"
	if !matched.IsSolo {
		cfg := ConfigFor(matched.Region)
		authHost = cfg.AuthHost
		clientID = cfg.ClientID
	}

	accessToken := info.AccessToken
	refreshToken := info.RefreshToken
	expiresAt := info.ExpiresAt

	if refreshToken != "" {
		newAccess, newRefresh, exp, errEx := exchangeTraeToken(r.Context(), authHost, clientID, refreshToken)
		if errEx != nil {
			logger.Error("trae: callback exchange token warning: %v, falling back to original", errEx)
		} else {
			accessToken = newAccess
			if newRefresh != "" {
				refreshToken = newRefresh
			}
			expiresAt = exp
		}
	}

	uid := info.UID
	if uid == "" {
		uid = "user_" + matched.MachineID[:min(8, len(matched.MachineID))]
	}
	nickname := info.Nickname
	if nickname == "" {
		nickname = uid
	}

	label := "Trae-" + nickname
	if matched.IsSolo {
		label = "TraeSolo-" + nickname
	} else if matched.Region == RegionGlobal {
		label = "TraeGlobal-" + nickname
	}

	storageMap := map[string]any{
		"type":          "freetier",
		"vendor":        matched.VendorID,
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"expires_at":    expiresAt,
		"uid":           uid,
		"nickname":      nickname,
		"enterprise_id": info.EnterpriseID,
		"machine_id":    matched.MachineID,
		"device_id":     matched.DeviceID,
		"label":         label,
	}
	if !matched.IsSolo {
		storageMap["region"] = string(matched.Region)
	}

	rawStorage, errJSON := json.MarshalIndent(storageMap, "", "  ")
	if errJSON != nil {
		matched.State = "failed"
		matched.ErrMsg = "生成凭证失败: " + errJSON.Error()
		renderCallbackHTML(w, http.StatusInternalServerError, "生成凭证失败", errJSON.Error())
		return
	}

	fileName := matched.VendorID + "-" + uid + ".json"
	matched.AuthData = &pluginapi.AuthData{
		Provider:    "freetier",
		ID:          matched.VendorID + "-" + uid,
		FileName:    fileName,
		Label:       label,
		StorageJSON: rawStorage,
	}
	matched.State = "success"

	logger.Info("trae: 授权成功，账号=%s (%s), vendor=%s", uid, nickname, matched.VendorID)
	renderCallbackHTML(w, http.StatusOK, "授权成功", "账号已成功授权！现在可以关闭此窗口返回管理面板。")
}

func exchangeTraeToken(ctx context.Context, authHost, clientID, refreshToken string) (string, string, int64, error) {
	payload := map[string]string{
		"refreshToken": refreshToken,
		"client_id":    clientID,
	}
	bodyBytes, _ := json.Marshal(payload)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, authHost+"/api/v1/auth/exchange", bytes.NewReader(bodyBytes))
	if errReq != nil {
		return "", "", 0, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Trae/1.0.0")

	client := httpx.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", "", 0, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", 0, fmt.Errorf("upstream exchange error %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken         string `json:"accessToken"`
			RefreshToken        string `json:"refreshToken"`
			TokenExpireAt       int64  `json:"tokenExpireAt"`
			TokenExpireDuration int64  `json:"tokenExpireDuration"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", "", 0, err
	}
	if res.Code != 0 || res.Data.AccessToken == "" {
		return "", "", 0, fmt.Errorf("exchange error code=%d msg=%s", res.Code, res.Msg)
	}

	exp := res.Data.TokenExpireAt
	if exp > 1e12 {
		exp /= 1000
	}
	if exp <= 0 && res.Data.TokenExpireDuration > 0 {
		exp = time.Now().Unix() + res.Data.TokenExpireDuration
	}

	return res.Data.AccessToken, res.Data.RefreshToken, exp, nil
}

func renderCallbackHTML(w http.ResponseWriter, code int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	color := "#3fb950"
	if code >= 400 {
		color = "#f85149"
	}
	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>%s</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; background: #0d1117; color: #c9d1d9; }
    .card { background: #161b22; border: 1px solid #30363d; padding: 36px 40px; border-radius: 12px; box-shadow: 0 8px 24px rgba(0,0,0,0.5); text-align: center; max-width: 440px; }
    h2 { color: %s; margin: 0 0 16px 0; font-size: 24px; }
    p { margin: 8px 0; font-size: 15px; color: #8b949e; line-height: 1.5; }
    .msg { color: #f0f6fc; font-weight: 500; font-size: 16px; margin-bottom: 16px; }
    .tip { color: #6e7681; font-size: 13px; margin-top: 24px; }
  </style>
</head>
<body>
  <div class="card">
    <h2>%s</h2>
    <div class="msg">%s</div>
    <p class="tip">您可以关闭此页面返回管理面板。</p>
  </div>
  <script>
    if (%d === 200) {
      setTimeout(function() { window.close(); }, 3000);
    }
  </script>
</body>
</html>`, title, color, title, message, code)
	_, _ = w.Write([]byte(html))
}
