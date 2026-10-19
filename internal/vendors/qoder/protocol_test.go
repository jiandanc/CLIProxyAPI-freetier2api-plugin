package qoder

import (
	"testing"
)

func TestMapModel(t *testing.T) {
	cases := []struct {
		agent    string
		model    string
		expected string
	}{
		// Raw upstream keys (backward compatibility)
		{"codex", "gfmodel", "gfmodel"},
		{"codex", "gmodel", "gmodel"},
		{"codex", "kmodel_latest", "kmodel_latest"},
		{"codex", "qmodel_38max", "qmodel_38max"},
		{"codex", "qfmodel", "qfmodel"},
		// Canonical slugs
		{"codex", "qwen3.8-flash", "qfmodel"},
		{"codex", "qwen3.8-max", "qmodel_38max"},
		{"codex", "deepseek-flash", "dfmodel"},
		{"codex", "deepseek-v4-pro", "dmodel"},
		{"codex", "glm-5.3-flash", "gfmodel"},
		{"codex", "kimi-k3", "kmodel_latest"},
		{"codex", "sonus", "smodel"},
		{"codex", "cantus", "cmodel"},
		{"codex", "minimax-m3", "mmodel"},
		{"codex", "minimax-m2.7", "mmodel"},
		{"claude", "claude-sonnet-4-6", "gmodel"}, // sonnet 模糊匹配兜底
	}

	for _, c := range cases {
		got := MapModel(c.agent, c.model)
		if got != c.expected {
			t.Errorf("MapModel(%q, %q) = %q; want %q", c.agent, c.model, got, c.expected)
		}
	}
}

func TestCanonicalSlug(t *testing.T) {
	cases := []struct {
		displayName string
		rawKey      string
		expected    string
	}{
		{"Qwen3.8-Flash", "qfmodel", "qwen3.8-flash"},
		{"Qwen3.8-Max", "qmodel_38max", "qwen3.8-max"},
		{"DeepSeek-V4-Pro", "dmodel", "deepseek-v4-pro"},
		{"DeepSeek V4.1 Flash", "dfmodel", "deepseek-v4.1-flash"},
		{"Auto", "auto", "auto"},
		{"Ultimate", "ultimate", "ultimate"},
		{"Performance", "performance", "performance"},
		{"", "qfmodel", "qfmodel"},
	}

	for _, c := range cases {
		got := CanonicalSlug(c.displayName, c.rawKey)
		if got != c.expected {
			t.Errorf("CanonicalSlug(%q, %q) = %q; want %q", c.displayName, c.rawKey, got, c.expected)
		}
	}
}
