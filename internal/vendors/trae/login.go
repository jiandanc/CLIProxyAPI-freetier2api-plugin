package trae

// 本文件实现 Trae 授权登录链接生成与轮询状态检查。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// BuildLoginURL 构造 Trae 授权登录地址。
func BuildLoginURL(r Region, machineID, deviceID, callbackURL string) string {
	cfg := ConfigFor(r)
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", cfg.ClientID)
	v.Set("redirect", "0")

	traceID := MachineTraceID(machineID, deviceID)

	return cfg.ConsoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(traceID) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(cfg.IDEVersion) +
		"&x_app_type=stable"
}

// LoginStart 启动登录流程。
func LoginStart(ctx context.Context, r Region) (*pluginapi.AuthLoginStartResponse, error) {
	EnsureCallbackServer()

	machineID := randomHex(16)
	deviceID := randomHex(16)
	traceID := MachineTraceID(machineID, deviceID)
	callbackURL := "http://127.0.0.1:18080/authorize"

	loginURL := BuildLoginURL(r, machineID, deviceID, callbackURL)
	sessionID := "trae_" + string(r) + "_" + randomHex(8)

	RegisterPending(&PendingSession{
		SessionID: sessionID,
		VendorID:  VendorIDFor(r),
		Region:    r,
		IsSolo:    false,
		MachineID: machineID,
		DeviceID:  deviceID,
		TraceID:   traceID,
		State:     "pending",
		CreatedAt: time.Now(),
	})

	return &pluginapi.AuthLoginStartResponse{
		Provider:  "freetier",
		URL:       loginURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

// LoginPoll 轮询状态。
func LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	session, ok := GetPending(state)
	if !ok || time.Since(session.CreatedAt) > 15*time.Minute {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}

	switch session.State {
	case "success":
		RemovePending(state)
		if session.AuthData != nil {
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusSuccess,
				Message: "登录成功",
				Auth:    *session.AuthData,
			}, nil
		}
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "登录成功",
		}, nil
	case "failed":
		RemovePending(state)
		errMsg := session.ErrMsg
		if errMsg == "" {
			errMsg = "授权登录失败"
		}
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errMsg,
		}, nil
	default:
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}
}

// OwnsLoginSession 报告会话是否由本区域 Trae 拥有。
func OwnsLoginSession(sessionID string, r Region) bool {
	return OwnsSession(sessionID, VendorIDFor(r))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
