package minimaxcode

import (
	"testing"
)

func TestSignatureIsMD5OverSaltAndBody(t *testing.T) {
	got := XSignature(1700000000, `{"a":1}`)
	expected := MD5Hex("1700000000" + SignatureSalt + `{"a":1}`)
	if got != expected {
		t.Fatalf("got %s, want %s", got, expected)
	}
}

func TestYYCoversTheEncodedURL(t *testing.T) {
	url := "https://agent.minimax.io/x?token=ab&client=web"
	expected := MD5Hex(
		EncodeURIComponent(url) +
			"_" +
			"{}" +
			MD5Hex("1700000000000") +
			"ooui",
	)
	got := YYSignature(url, "{}", 1700000000000)
	if got != expected {
		t.Fatalf("got %s, want %s", got, expected)
	}
}

func TestEncodeURIComponentMatchesJavaScript(t *testing.T) {
	unreserved := "!'()*-._~"
	for i := 0; i < len(unreserved); i++ {
		char := string(unreserved[i])
		if got := EncodeURIComponent(char); got != char {
			t.Fatalf("EncodeURIComponent(%q) = %q, want %q", char, got, char)
		}
	}
	if got := EncodeURIComponent(" "); got != "%20" {
		t.Fatalf("EncodeURIComponent(' ') = %q, want %%20", got)
	}
	if got := EncodeURIComponent("中"); got != "%E4%B8%AD" {
		t.Fatalf("EncodeURIComponent('中') = %q, want %%E4%%B8%%AD", got)
	}
	if got := EncodeURIComponent("a+b"); got != "a%2Bb" {
		t.Fatalf("EncodeURIComponent('a+b') = %q, want 'a%%2Bb'", got)
	}
	if got := EncodeURIComponent("a/b"); got != "a%2Fb" {
		t.Fatalf("EncodeURIComponent('a/b') = %q, want 'a%%2Fb'", got)
	}
}

func TestFormEncodeUsesPlusForSpace(t *testing.T) {
	if got := FormEncode("a b"); got != "a+b" {
		t.Fatalf("FormEncode('a b') = %q, want 'a+b'", got)
	}
	if got := FormEncode("a+b"); got != "a%2Bb" {
		t.Fatalf("FormEncode('a+b') = %q, want 'a%%2Bb'", got)
	}
}
