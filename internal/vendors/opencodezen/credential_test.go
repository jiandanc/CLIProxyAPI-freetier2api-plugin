package opencodezen

import (
	"testing"
)

func TestLooksLikeCredential_OpenCodeZEN(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		raw      map[string]any
		want     bool
	}{
		{"file name convention", "opencodezen-key1.json", map[string]any{"api_key": "sk-abc"}, true},
		{"file name opencode-zen", "opencode-zen-1.json", map[string]any{"api_key": "sk-abc"}, true},
		{"zen specific field", "anything.json", map[string]any{"zen_key": "sk-abc"}, true},
		{"api_key alone", "x.json", map[string]any{"api_key": "sk-abc"}, true},
		{"has foreign marker", "x.json", map[string]any{"api_key": "sk-abc", "device_token": "dt-x"}, false},
		{"token field alone is not enough", "x.json", map[string]any{"token": "sk-abc"}, false},
		{"qoder pt prefix", "x.json", map[string]any{"token": "pt-abc"}, false},
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

func TestParseCredential(t *testing.T) {
	cred, err := ParseCredential([]byte(`{"api_key":"sk-1234567890abcdef","label":"my zen"}`))
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	if cred.APIKey != "sk-1234567890abcdef" {
		t.Fatalf("APIKey = %q", cred.APIKey)
	}
	if cred.Label != "my zen" {
		t.Fatalf("Label = %q", cred.Label)
	}

	if _, err := ParseCredential([]byte(`{}`)); err == nil {
		t.Fatal("expected error for empty credential")
	}
	if _, err := ParseCredential([]byte(`{"label":"only a label"}`)); err == nil {
		t.Fatal("expected error for credential without api key")
	}
}

func TestMaskedKey(t *testing.T) {
	// 12 位：保留前 4 与后 4。
	if got := MaskedKey("abcdefghijkl"); got != "abcd***ijkl" {
		t.Fatalf("MaskedKey = %q, want abcd***ijkl", got)
	}
	// 恰好 8 位：露出前后四位等于露出全部，因此全遮。
	if got := MaskedKey("12345678"); got != "***" {
		t.Fatalf("8-char MaskedKey = %q, want ***", got)
	}
	if got := MaskedKey(""); got != "" {
		t.Fatalf("empty MaskedKey = %q", got)
	}
}
