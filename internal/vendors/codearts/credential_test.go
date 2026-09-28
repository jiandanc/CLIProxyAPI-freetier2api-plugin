package codearts

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
			raw:      map[string]any{"vendor": "codearts", "user_id": "u123"},
			fileName: "any.json",
			want:     true,
		},
		{
			name:     "filename prefix",
			raw:      map[string]any{"user_id": "u123"},
			fileName: "codearts-user1.json",
			want:     true,
		},
		{
			name:     "huawei ak/sk match",
			raw:      map[string]any{"access_key_id": "AKIA...", "secret_access_key": "secret..."},
			fileName: "test.json",
			want:     true,
		},
		{
			name:     "foreign vendor",
			raw:      map[string]any{"vendor": "traesolo"},
			fileName: "codearts-test.json",
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
