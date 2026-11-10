package notify

import (
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"strings"
	"testing"
)

func TestChatReportIsSafe(t *testing.T) {
	msgs := []detect.ChatMsg{{Time: 1762221000, Msg: "@everyone 見て `code` " + strings.Repeat("あ", 200)}}
	for i := 0; i < 30; i++ {
		msgs = append(msgs, detect.ChatMsg{Time: 1762221001 + int64(i), Msg: "spam"})
	}
	txt := BuildChatReport(ChatReportInput{Name: "x", UUID: "u", Msgs: msgs, Trigger: "repeat: 5", Mode: config.ModeLive, MuteCmd: "mute x 10m spam"})
	if strings.Contains(txt, "@everyone") || strings.Contains(txt, "`code`") {
		t.Fatalf("メンションが残っている: %s", txt)
	}
	if n := len([]rune(txt)); n > 1901 {
		t.Fatalf("長すぎる: %d", n)
	}
	if !strings.Contains(txt, "対応 (予定)") || strings.Contains(txt, "試運転") {
		t.Fatalf("live の予定の表示: %s", txt)
	}
	dry := BuildChatReport(ChatReportInput{Name: "x", UUID: "u", Msgs: msgs[:2], Trigger: "flood", Mode: config.ModeDryRun})
	if !strings.Contains(dry, "試運転") {
		t.Fatalf("dry-run の表示: %s", dry)
	}
	if got := len(pickChatLines(msgs, maxChatLines)); got != maxChatLines {
		t.Fatalf("抜粋の数: %d", got)
	}
}
