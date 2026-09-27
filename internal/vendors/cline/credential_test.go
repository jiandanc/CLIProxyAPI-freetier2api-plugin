package cline

import (
	"strings"
	"testing"
)

func TestLooksLikeCredential_Cline(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		raw      map[string]any
		want     bool
	}{
		{"file name convention", "cline-abc.json", map[string]any{"refreshToken": "rt"}, true},
		{"workos prefix", "x.json", map[string]any{"accessToken": "workos:at"}, true},
		{"camel refreshToken", "x.json", map[string]any{"refreshToken": "rt"}, true},
		{"snake refresh_token alone is not enough", "x.json", map[string]any{"refresh_token": "rt"}, false},
		{"qoder style pt prefix", "x.json", map[string]any{"access_token": "pt-abc"}, false},
		{"workbuddy device token", "x.json", map[string]any{"device_token": "dt-abc", "account": map[string]any{}}, false},
		{"empty", "x.json", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeCredential(tc.raw, tc.fileName, "freetier"); got != tc.want {
				t.Fatalf("LooksLikeCredential(%q) = %v, want %v", tc.fileName, got, tc.want)
			}
		})
	}
}

func TestParseCredential_Cline(t *testing.T) {
	cred, err := ParseCredential([]byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000000,"email":"a@b.c"}`))
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	if cred.AccessToken != "at" || cred.RefreshToken != "rt" {
		t.Fatalf("tokens = %q/%q", cred.AccessToken, cred.RefreshToken)
	}
	// expiresAt 是毫秒，应转成秒。
	if cred.ExpiresAt != 1753600000 {
		t.Fatalf("ExpiresAt = %d, want 1753600000", cred.ExpiresAt)
	}

	// workos: 前缀在解析时被剥掉。
	prefixed, err := ParseCredential([]byte(`{"accessToken":"workos:at","refreshToken":"rt"}`))
	if err != nil {
		t.Fatalf("ParseCredential prefixed: %v", err)
	}
	if prefixed.AccessToken != "at" {
		t.Fatalf("AccessToken = %q, want stripped", prefixed.AccessToken)
	}
	if prefixed.BearerToken() != "workos:at" {
		t.Fatalf("BearerToken = %q", prefixed.BearerToken())
	}
}

func TestWithWorkOSPrefix_IsIdempotent(t *testing.T) {
	if got := WithWorkOSPrefix("workos:abc"); got != "workos:abc" {
		t.Fatalf("double prefix = %q", got)
	}
	if got := WithWorkOSPrefix("abc"); got != "workos:abc" {
		t.Fatalf("single prefix = %q", got)
	}
}

func TestMergeStorageJSON_RespectsExistingKeys(t *testing.T) {
	updated := &Credential{AccessToken: "new-at", RefreshToken: "new-rt", ExpiresAt: 123}
	merged, err := MergeStorageJSON([]byte(`{"access_token":"old","refresh_token":"old-rt","email":"a@b.c"}`), updated)
	if err != nil {
		t.Fatalf("MergeStorageJSON: %v", err)
	}
	// 原文件用下划线键，合并后应保持下划线键而不是改成驼峰。
	if !strings.Contains(string(merged), `"access_token": "new-at"`) {
		t.Fatalf("snake case key not preserved: %s", merged)
	}
	if !strings.Contains(string(merged), `"email": "a@b.c"`) {
		t.Fatalf("email field lost: %s", merged)
	}
}
