package tabbit

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
			raw:      map[string]any{"vendor": "tabbit", "api_key": "sk-tabbit-1234"},
			fileName: "any.json",
			want:     true,
		},
		{
			name:     "filename prefix",
			raw:      map[string]any{"api_key": "sk-tabbit-1234"},
			fileName: "tabbit-user1.json",
			want:     true,
		},
		{
			name:     "foreign vendor",
			raw:      map[string]any{"vendor": "zcode"},
			fileName: "tabbit-acc.json",
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
