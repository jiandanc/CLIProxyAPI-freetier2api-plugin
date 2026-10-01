package main

import (
	"testing"

	"freetier2api-plugin/internal/core"
)

// 端到端：同一条账号记录，签到路径与展示路径必须解析出同一个账号身份，
// 并且在 parseVendorCredential 的 FileID 兜底之后仍然一致。
func TestCheckinAndDisplayResolveSameIdentity(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// Qoder 凭证（UID 取自文件名）——这正是出问题的形态。
	const qoderCredential = `{"type":"freetier","vendor":"qodercn","region":"cn",
		"device_token":"dt-abc","refresh_token":"drt-xyz","label":"nick"}`
	entry := hostAuthEntry{
		ID:        "qodercn-nick1456600333",
		Name:      "qodercn-nick1456600333.json",
		AuthIndex: "019fa299-3d61-7192-b903-0145508e658c",
	}

	identity := accountIdentity(entry)
	credential, okParse := parseVendorCredential([]byte(qoderCredential), identity, nil)
	if !okParse {
		t.Fatal("qoder credential not parsed")
	}

	// 核心断言：凭证解析出的 FileID 与两条路径传入的文件名一致。
	if got := credential.FileIDValue(); got != identity {
		t.Fatalf("FileID = %q, want %q", got, identity)
	}
	// 且不得再是 AuthIndex —— 那是导致签到/展示错位的根源。
	if credential.FileIDValue() == entry.AuthIndex {
		t.Fatal("FileID 不得等于 AuthIndex")
	}
	if credential.FileIDValue() == "" {
		t.Fatal("FileID 不得为空（签到记录会因此永远查不到）")
	}
}

// FileID 兜底：供应商 Parse 没设置 FileID 时，根层必须补上。
func TestParseVendorCredentialFillsFileID(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// WorkBuddy 的 UID 是上游 UUID；FileID 仍必须是文件名。
	const wbCredential = `{"type":"freetier","vendor":"workbuddycn",
		"auth":{"accessToken":"at","refreshToken":"rt","realm":"cn"},
		"account":{"uid":"79fdc1fc-de43-40da-b6f0-fe05d9e4367b"}}`
	credential, okParse := parseVendorCredential([]byte(wbCredential), "workbuddycn-79fdc1fc.json", nil)
	if !okParse {
		t.Fatal("workbuddy credential not parsed")
	}

	if got := credential.FileIDValue(); got != "workbuddycn-79fdc1fc" {
		t.Fatalf("FileID = %q, want workbuddycn-79fdc1fc", got)
	}
	// 上游 UID 必须原样保留：设备指纹与 X-Uid 头都靠它，改了会被上游当作新设备。
	if got := credential.UIDValue(); got != "79fdc1fc-de43-40da-b6f0-fe05d9e4367b" {
		t.Fatalf("UID = %q（上游身份不得被 FileID 覆盖）", got)
	}
}

// 上游标识不参与任何身份判定。
func TestUpstreamUIDIsNotUsedForIdentity(t *testing.T) {
	credential := &core.Credential{UID: "upstream-uuid", FileID: "file-id"}
	if credential.FileIDValue() == credential.UIDValue() {
		t.Fatal("两个字段必须是独立语义")
	}
}
