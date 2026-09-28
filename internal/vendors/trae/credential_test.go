package trae

import (
	"testing"
)

func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		region   Region
		want     bool
	}{
		{
			name:     "traecn explicit",
			raw:      map[string]any{"vendor": "traecn"},
			fileName: "any.json",
			region:   RegionCN,
			want:     true,
		},
		{
			name:     "traeglobal explicit",
			raw:      map[string]any{"vendor": "traeglobal"},
			fileName: "any.json",
			region:   RegionGlobal,
			want:     true,
		},
		{
			name:     "cross rejection",
			raw:      map[string]any{"vendor": "traeglobal"},
			fileName: "any.json",
			region:   RegionCN,
			want:     false,
		},
		{
			name:     "generic trae with global region",
			raw:      map[string]any{"region": "global"},
			fileName: "trae-acc.json",
			region:   RegionGlobal,
			want:     true,
		},
		{
			name:     "generic trae without region defaults to cn",
			raw:      map[string]any{},
			fileName: "trae-acc.json",
			region:   RegionCN,
			want:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LooksLikeCredential(tc.raw, tc.fileName, "", tc.region)
			if got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}
