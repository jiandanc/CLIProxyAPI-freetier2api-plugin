package traesolo

import (
	"testing"
)

func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		provider string
		want     bool
	}{
		{
			name:     "explicit vendor",
			raw:      map[string]any{"vendor": "traesolo", "access_token": "token123"},
			fileName: "any.json",
			want:     true,
		},
		{
			name:     "filename prefix",
			raw:      map[string]any{"access_token": "token123"},
			fileName: "traesolo-u1.json",
			want:     true,
		},
		{
			name:     "foreign vendor",
			raw:      map[string]any{"vendor": "workbuddycn"},
			fileName: "traesolo-test.json",
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LooksLikeCredential(tc.raw, tc.fileName, tc.provider)
			if got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestParseCredentialNested(t *testing.T) {
	raw := []byte(`{
		"auth": {
			"accessToken": "atk_123",
			"refreshToken": "rtk_456",
			"expiresAt": 1700000000
		},
		"account": {
			"uid": "user_789",
			"nickname": "Alice"
		}
	}`)
	cred, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cred.AccessToken != "atk_123" || cred.RefreshToken != "rtk_456" || cred.UID != "user_789" {
		t.Fatalf("unexpected parsed result: %+v", cred)
	}
}
