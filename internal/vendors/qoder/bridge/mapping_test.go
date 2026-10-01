package bridge

import (
	"testing"
)

func TestMapModel(t *testing.T) {
	// 注册 ID 即上游 key，因此 MapModel 对已知 key 是恒等映射；
	// 未知名走内置兜底表（默认档 performance → gmodel）。
	cases := []struct {
		agent    string
		model    string
		expected string
	}{
		{"codex", "gfmodel", "gfmodel"},
		{"codex", "gmodel", "gmodel"},
		{"codex", "kmodel_latest", "kmodel_latest"},
		{"codex", "qmodel_38max", "qmodel_38max"},
		{"claude", "claude-sonnet-4-6", "gmodel"}, // sonnet 模糊匹配兜底
	}

	for _, c := range cases {
		got := MapModel(c.agent, c.model)
		if got != c.expected {
			t.Errorf("MapModel(%q, %q) = %q; want %q", c.agent, c.model, got, c.expected)
		}
	}
}
