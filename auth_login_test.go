package main

// 登录会话绑定的回归测试。
//
// 守卫的问题：宿主靠 AuthLoginStartResponse.Metadata 里的 vendor 字段把登录
// 会话绑定回具体供应商；缺了它宿主拿不到授权链接，页面只显示「获取授权链接
// 失败」。这条约束曾经只靠各供应商自己记得设置，导致 codearts / trae /
// traesolo 三个供应商长期无法添加账号——而插件侧 LoginStart 明明成功返回了
// URL，日志上只有一句 "started login"，排查方向被完全带偏。
//
// 现在标识由根层（auth_login.go 的分发点）统一注入，本用例即该契约的守卫。

import (
	"encoding/json"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// TestLoginStartInjectsVendorMetadata 验证根层为供应商补齐登录会话绑定。
//
// 用**真实的 codearts** 而不是桩：它的 LoginStart 纯本地构造（无网络调用），
// 且其包内实现本身不设置 Metadata——正是回归要覆盖的场景。
func TestLoginStartInjectsVendorMetadata(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	vendor, okVendor := core.VendorByID("codearts")
	if !okVendor {
		t.Fatal("codearts vendor not registered")
	}

	result := callMethod(t, pluginabi.MethodAuthLoginStart, map[string]any{
		"metadata": map[string]any{core.VendorKey: vendor.ID()},
	})

	var response pluginapi.AuthLoginStartResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode login response: %v (raw=%s)", errUnmarshal, result)
	}
	if response.URL == "" {
		t.Fatal("授权链接为空")
	}
	// 核心断言：供应商未设置 Metadata，根层也必须补上 vendor 绑定。
	if got := response.Metadata[core.VendorKey]; got != vendor.ID() {
		t.Fatalf("metadata.vendor = %v, want %q（宿主靠它绑定登录会话）", got, vendor.ID())
	}
	if got := response.Metadata["region"]; got != vendor.Region() {
		t.Fatalf("metadata.region = %v, want %q", got, vendor.Region())
	}
}
