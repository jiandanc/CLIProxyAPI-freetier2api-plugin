package zcode

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
			raw:      map[string]any{"vendor": "zcode", "api_key": "abc.123"},
			fileName: "any.json",
			want:     true,
		},
		{
			name:     "filename prefix",
			raw:      map[string]any{"api_key": "abc.123"},
			fileName: "zcode-acc1.json",
			want:     true,
		},
		{
			name:     "foreign vendor",
			raw:      map[string]any{"vendor": "qodercn"},
			fileName: "zcode-test.json",
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

func TestMaskedKey(t *testing.T) {
	if got := MaskedKey("1234567890abcdef"); got != "1234***cdef" {
		t.Fatalf("unexpected masked key: %s", got)
	}
}
