package domain

import (
	"strings"
	"testing"
)

func TestSafeChat(t *testing.T) {
	got := SafeChat("a\nb\x00c @everyone `x` [click](https://example.com) \u2028 d", 200)
	for _, bad := range []string{"\n", "\x00", "\u2028", "@everyone", "`", "]("} {
		if strings.Contains(got, bad) {
			t.Errorf("%q が残っている: %q", bad, got)
		}
	}
	if r := []rune(SafeChat(strings.Repeat("あ", 50), 10)); len(r) != 11 {
		t.Fatalf("長さ: %d", len(r))
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{"20261104-101500-fuuty119": true, "chat-1-a.b": true, "": false, "../x": false, "a/b": false, "a\\b": false, "a..b": false, "x y": false} {
		if ValidID(id) != want {
			t.Errorf("validID(%q) = %v", id, !want)
		}
	}
}

func TestCleanText(t *testing.T) {
	if got := CleanText("a\nb\x00c\r\n", 50); got != "a b c" {
		t.Errorf("%q", got)
	}
	if got := CleanText("あいうえお", 3); got != "あいう" {
		t.Errorf("%q", got)
	}
}
