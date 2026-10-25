package core

import "testing"

// TestFormatRateTreatsPlaceholdersAsUnknown 锁定「未知」与「免费」的边界。
//
// 上游偶尔把缺数据写成 -1 / NaN / Inf；这些值一旦进控制台，用户会以为那是
// 真实倍率。0 必须保留 —— 它表示限免，是合法值。
func TestFormatRateTreatsPlaceholdersAsUnknown(t *testing.T) {
	cases := map[string]string{
		"x0.94 credits": "0.94X",
		"0":             "0.00X",
		"x0.05":         "0.05X",
		"免费":            "免费",
		"-1":            "",
		"NaN":           "",
		"-Inf":          "",
		"":              "",
	}
	for raw, want := range cases {
		if got := FormatRate(raw); got != want {
			t.Fatalf("FormatRate(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestCreditTableReplaceAndSnapshot 验证整表替换与空表语义。
func TestCreditTableReplaceAndSnapshot(t *testing.T) {
	var table CreditTable
	if got := table.Snapshot(); got != nil {
		t.Fatalf("零值应返回 nil，实际 %v", got)
	}
	table.Replace(map[string]string{"glm-5": "0.70X", "": "x", "glm-6": "  "})
	got := table.Snapshot()
	if len(got) != 1 || got["glm-5"] != "0.70X" {
		t.Fatalf("Snapshot = %v, want 只含 glm-5", got)
	}
	got["glm-5"] = "tampered"
	if table.Snapshot()["glm-5"] != "0.70X" {
		t.Fatal("Snapshot 必须返回副本")
	}
	table.Replace(nil)
	if got := table.Snapshot(); got != nil {
		t.Fatalf("空表替换后应返回 nil，实际 %v", got)
	}
}

// TestFillCreditsNormalizesAndReplaces 锁定六家共用的入表口径：
// 原始文案统一过 FormatRate，空 ID / 空文案不写进表，空批次清空上一轮。
func TestFillCreditsNormalizesAndReplaces(t *testing.T) {
	type item struct {
		ID  string
		Raw string
	}
	var table CreditTable
	FillCredits(&table, []item{
		{"a", "x0.5 credits"},
		{" b ", "1"},
		{"", "0.9"},
		{"c", ""},
		{"d", "免费"},
		{"e", "-1"},
	}, func(m item) string { return m.ID }, func(m item) string { return m.Raw })

	want := map[string]string{"a": "0.50X", "b": "1.00X", "d": "免费"}
	got := table.Snapshot()
	if len(got) != len(want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	for id, label := range want {
		if got[id] != label {
			t.Fatalf("snapshot[%s] = %q, want %q", id, got[id], label)
		}
	}

	FillCredits(&table, nil, func(m item) string { return m.ID }, func(m item) string { return m.Raw })
	if table.Snapshot() != nil {
		t.Fatal("empty batch must clear the table")
	}
}
