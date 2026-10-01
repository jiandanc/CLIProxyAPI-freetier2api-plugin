package main

import (
	"strings"
	"testing"
)

// 模拟宿主对同一条账号记录给出的三种标识（真实取值形态）。
func TestAccountIdentityIsStableAcrossPaths(t *testing.T) {
	entries := []hostAuthEntry{
		// 有文件名的常规账号（Qoder / ZCode / WorkBuddy 都是这种）。
		{ID: "qodercn-nick1456600333", Name: "qodercn-nick1456600333.json", AuthIndex: "019fa299-3d61-7192-b903-0145508e658c"},
		{ID: "zcode-ac90---6IY9", Name: "zcode-ac90---6IY9.json", AuthIndex: "a1b2c3d4"},
		{ID: "workbuddycn-79fdc1fc", Name: "workbuddycn-79fdc1fc-de43-40da-b6f0-fe05d9e4367b.json", AuthIndex: "deadbeef"},
	}
	for _, entry := range entries {
		got := accountIdentity(entry)
		// 身份取 Name（磁盘文件名），它才是凭证解析时用的同一个标识；
		// ID 可能被截断（宿主对长文件名有截断行为），不能当身份。
		want := strings.TrimSuffix(entry.Name, ".json")
		if got != want {
			t.Fatalf("accountIdentity(%+v) = %q, want %q", entry, got, want)
		}
		// 关键：身份与 AuthIndex 无关——早期签到走 AuthIndex、展示走 Name，
		// 于是同一账号算出两个身份。这条断言锁死该回归。
		if got == entry.AuthIndex {
			t.Fatalf("账号身份不得等于 AuthIndex（会导致签到/展示错位）: %q", got)
		}
	}
}

// 运行时账号没有 Name（宿主内建 provider），必须回落到 ID。
func TestAccountIdentityFallsBackToID(t *testing.T) {
	if got := accountIdentity(hostAuthEntry{ID: "runtime-1", AuthIndex: "idx"}); got != "runtime-1" {
		t.Fatalf("got %q, want runtime-1", got)
	}
	// 三者皆空时返回空串，由调用方决定处置（而不是凭空造一个身份）。
	if got := accountIdentity(hostAuthEntry{}); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	// .json 后缀必须剥掉，否则与凭证解析时的文件名不一致。
	if got := accountIdentity(hostAuthEntry{Name: "cline-me@x.com.json"}); got != "cline-me@x.com" {
		t.Fatalf("got %q, want cline-me@x.com", got)
	}
}
