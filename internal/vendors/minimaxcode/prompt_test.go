package minimaxcode

import (
	"strings"
	"testing"
)

func TestPromptInlinesSystemAndNumbersHistory(t *testing.T) {
	turns := []*Turn{
		{Role: "system", Text: "be brief"},
		{Role: "user", Text: "one"},
		{Role: "assistant", Text: "two"},
		{Role: "user", Text: "three"},
	}
	expected := "[系统指令] be brief\n\n 用户：1. one\n\n助手：two\n\n 用户：2. three"
	got := BuildPrompt(turns)
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
}

func TestSingleQuestionIsSentBare(t *testing.T) {
	turns := []*Turn{
		{Role: "user", Text: "hello"},
	}
	expected := "hello"
	got := BuildPrompt(turns)
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
}

func TestImagesFollowTheTextAndKeepTheirURL(t *testing.T) {
	message := ParseTurn(map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "这是什么"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://cdn/x.png"}},
		},
	})
	text := BuildPrompt([]*Turn{message})
	if !strings.HasPrefix(text, "这是什么") {
		t.Fatalf("expected prefix '这是什么', got %q", text)
	}
	if !strings.HasSuffix(strings.TrimSpace(text), "(https://cdn/x.png)") {
		t.Fatalf("expected suffix '(https://cdn/x.png)', got %q", text)
	}
}

func TestImageOnlyTurnSaysSomething(t *testing.T) {
	message := ParseTurn(map[string]any{
		"role":    "user",
		"content": "",
		"images":  []any{"https://cdn/x.png"},
	})
	expected := "看图\n![已忽略的图片 0](https://cdn/x.png)"
	got := BuildPrompt([]*Turn{message})
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
}

func TestToolAndUnknownRolesAreRenderedAsUserTurns(t *testing.T) {
	tool := ParseTurn(map[string]any{"role": "tool", "content": "42"})
	user := ParseTurn(map[string]any{"role": "user", "content": "so?"})
	expected := " 用户：1. 42\n\n 用户：2. so?"
	got := BuildPrompt([]*Turn{tool, user})
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
	if ParseTurn(map[string]any{"content": "no role"}).Role != "user" {
		t.Fatalf("expected role 'user'")
	}
	if ParseTurn(nil) != nil {
		t.Fatalf("expected nil")
	}
}
