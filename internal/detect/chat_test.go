package detect

import (
	"crime-buster/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chatCfg() *config.Config {
	c, _ := config.LoadConfig()
	return c
}

func feedAll(d *ChatDetector, user int, t0 int64, step int64, msgs ...string) *ChatTrigger {
	var last *ChatTrigger
	for i, m := range msgs {
		if tr := d.Feed(ChatMsg{User: user, Time: t0 + int64(i)*step, Msg: m}); tr != nil && last == nil {
			last = tr
		}
	}
	return last
}

func TestChatNormalTalkIsNotSpam(t *testing.T) {
	d := NewChatDetector(chatCfg())
	if tr := feedAll(d, 1, 1000, 6, "こんにちは", "今日は何する？", "建築しよう", "いいね", "ok", "ok", "了解"); tr != nil {
		t.Fatalf("普通の会話を検知した: %v", tr)
	}
}

func TestChatCountdownIsNotFlood(t *testing.T) {
	// 実際にあった、カウントダウン (8 件 / 10 秒) を誤検知しない
	d := NewChatDetector(chatCfg())
	if tr := feedAll(d, 1, 1000, 1, "５", "4 (４)", "3 (３)", "2 (２)", "1 (１)", "0 (０)", "go (五)", "sex (背x)"); tr != nil {
		t.Fatalf("カウントダウンを検知した: %v", tr)
	}
}

func TestChatFlood(t *testing.T) {
	d := NewChatDetector(chatCfg())
	var msgs []string
	for i := 0; i < 14; i++ {
		msgs = append(msgs, strings.Repeat("x", 3)+string(rune('a'+i))+"hello"+string(rune('k'+i)))
	}
	tr := feedAll(d, 1, 1000, 0, msgs...)
	if tr == nil || tr.Kind != "flood" {
		t.Fatalf("連投を検知するはず: %v", tr)
	}
}

func TestChatRepeatIgnoresDigitsAndSpaces(t *testing.T) {
	d := NewChatDetector(chatCfg())
	tr := feedAll(d, 1, 1000, 1, "買ってね 1", "買ってね 2", "買ってね  3", "買ってね4", "買ってね 5")
	if tr == nil || tr.Kind != "repeat" {
		t.Fatalf("数字だけ違う同じ発言を検知するはず: %v", tr)
	}
}

func TestChatRepeatNeedsWindow(t *testing.T) {
	d := NewChatDetector(chatCfg())
	// 70 秒おきなら、同じ発言でも連投ではない
	if tr := feedAll(d, 1, 1000, 70, "あ", "あ", "あ", "あ", "あ", "あ"); tr != nil {
		t.Fatalf("間が空いた発言を検知した: %v", tr)
	}
}

func TestChatRun(t *testing.T) {
	d := NewChatDetector(chatCfg())
	tr := d.Feed(ChatMsg{User: 1, Time: 1, Msg: strings.Repeat("a", 40)})
	if tr == nil || tr.Kind != "run" {
		t.Fatalf("同じ文字の連続を検知するはず: %v", tr)
	}
	if tr := d.Feed(ChatMsg{User: 2, Time: 1, Msg: strings.Repeat("w", 12)}); tr != nil {
		t.Fatalf("12 文字の「wwww」は普通: %v", tr)
	}
}

func TestChatInviteAndLinks(t *testing.T) {
	d := NewChatDetector(chatCfg())
	if tr := feedAll(d, 1, 1000, 30, "遊ぼう discord.gg/abcd", "また discord.gg/abcd"); tr == nil || tr.Kind != "invite" {
		t.Fatalf("招待リンクの繰り返し: %v", tr)
	}
	d = NewChatDetector(chatCfg())
	if tr := feedAll(d, 2, 1000, 20, "見て https://a.example/1", "これも https://b.example/2"); tr != nil {
		t.Fatalf("リンクが 2 回だけなら普通: %v", tr)
	}
	if tr := feedAll(d, 2, 1100, 20, "ほら https://c.example/3"); tr == nil || tr.Kind != "link" {
		t.Fatalf("リンクが 3 回: %v", tr)
	}
}

func TestChatBadWordsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "words.txt")
	if err := os.WriteFile(p, []byte("# 注釈\nbadword\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := chatCfg()
	c.ChatBadWordsFile = p
	d := NewChatDetector(c)
	if tr := d.Feed(ChatMsg{User: 1, Time: 1, Msg: "You BadWord!"}); tr == nil || tr.Kind != "badword" {
		t.Fatalf("禁止語: %v", tr)
	}
	if tr := NewChatDetector(chatCfg()).Feed(ChatMsg{User: 1, Time: 1, Msg: "badword"}); tr != nil {
		t.Fatalf("ファイルが無いときは、禁止語は使わない: %v", tr)
	}
}
