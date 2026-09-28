package trae

// 本文件实现 Trae 授权登录链接生成。

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

	traceID := machineID
	if len(traceID) > 16 {
		traceID = traceID[len(traceID)-16:]
	}

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
	machineID := randomHex(16)
	deviceID := randomHex(16)
	callbackURL := "http://127.0.0.1:18080/authorize"

	loginURL := BuildLoginURL(r, machineID, deviceID, callbackURL)
	sessionID := string(VendorIDFor(r)) + "_" + randomHex(8)

	return &pluginapi.AuthLoginStartResponse{
		Provider:  "freetier",
		URL:       loginURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

// LoginPoll 轮询状态。
func LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: "请在浏览器中完成登录，或在管理端直接导入凭证 JSON",
	}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
