package testutil

import "testing"

// AssertNoMentions は、投稿の本文が、だれにも通知しない設定かを確かめる。
func AssertNoMentions(t *testing.T, m map[string]any) {
	t.Helper()
	am, ok := m["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("allowed_mentions が無い: %v", m)
	}
	if parse, ok := am["parse"].([]any); !ok || len(parse) != 0 {
		t.Fatalf("allowed_mentions: %v", am)
	}
}
