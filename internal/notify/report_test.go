package notify

import (
	"crime-buster/internal/capture"
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"crime-buster/internal/store"
	"crime-buster/internal/testutil"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportText(t *testing.T) {
	s := &detect.Stats{
		User: detect.UserInfo{Name: "fuuty119", UUID: "53b4dddf-b884-4439-b26b-be0741d26e8c"}, World: "main",
		From: 1793757149, To: 1793757495, Breaks: 2642, Places: 158, PeakPerS: 421,
		X0: -5712, X1: -5612, Y0: 55, Y1: 94, Z0: -10204, Z1: -10049,
		TopBreaks: []detect.MatCount{{Material: "stone", Count: 820}, {Material: "sand", Count: 357}},
		Commands:  []string{"/co i", "/co i"},
	}
	txt := BuildReport(ReportInput{Stats: s, Trigger: "burst-break", Warp: "ワープ kanso_town の近く (約 12 ブロック)",
		Center: &capture.Cam{X: -5650, Z: -10125}, Out: Outcome{RolledBack: true, RolledDone: 2800, RolledTotal: 2800, RestoreCmd: "co restore u:fuuty119 t:415s r:#main"}, Mode: config.ModeLive,
		Images: []string{"1枚目 被害"}})
	for _, want := range []string{"**ワールド**: main", "**座標**: -5650 -10125 付近", "fuuty119", "2,642", "421 個/秒", "stone 820", "`/co i` ×2", "ロールバック済み (2,800 / 2,800 件)", "co restore u:fuuty119", "kanso_town"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
	if len([]rune(txt)) > 2000 {
		t.Fatal("over discord limit")
	}
	dry := BuildReport(ReportInput{Stats: s, Out: Outcome{Planned: true, Cmd: "co rollback u:fuuty119 t:415s r:#main"}, Mode: config.ModeDryRun})
	if !strings.Contains(dry, "試運転") || !strings.Contains(dry, "対応 (予定)") {
		t.Fatalf("dry-run text: %s", dry)
	}
	later := BuildReport(ReportInput{Stats: s, Out: Outcome{Planned: true, Cmd: "co rollback u:fuuty119 t:4015s r:#main", CmdUntil: s.To + 3600}, Mode: config.ModeReview})
	for _, want := range []string{"ロールバック (未実行)", "t:4015s", "までに実行してください"} {
		if !strings.Contains(later, want) {
			t.Errorf("missing %q in:\n%s", want, later)
		}
	}
}

func TestComma(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 2642: "2,642", 1234567: "1,234,567"} {
		if got := Comma(in); got != want {
			t.Errorf("comma(%d)=%s want %s", in, got, want)
		}
	}
}

func TestLoadBotToken(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/config.yml"
	if err := testutil.WriteFile(p, "ConfigVersion: 1\nBotToken: \"abc.def.ghi\"\nChannels: {\"global\": \"1\"}\n"); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadBotToken(p)
	if err != nil || tok != "abc.def.ghi" {
		t.Fatalf("%q %v", tok, err)
	}
	if err := testutil.WriteFile(p, "BotToken: \"\"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBotToken(p); err == nil {
		t.Fatal("empty token must fail")
	}
}

func TestNearestWarp(t *testing.T) {
	ws := []store.Warp{{Name: "spawn", World: "main", X: 0, Z: 0}, {Name: "kanso_town", World: "main", X: -5728, Z: -10111}, {Name: "far", World: "flat", X: -5700, Z: -10100}}
	w, d, ok := store.Nearest(ws, "main", -5662, -10126)
	if !ok || w.Name != "kanso_town" || d > 100 {
		t.Fatalf("%v %v %v", w, d, ok)
	}
	if WarpText(w, d) == "" {
		t.Fatal("near warp must have text")
	}
	if WarpText(store.Warp{Name: "x"}, 2000) != "" {
		t.Fatal("far warp must be omitted")
	}
}

func TestReporterForPrefersBotToken(t *testing.T) {
	r, err := ReporterFor(&config.Config{DiscordConfig: config.DiscordConfig{BotToken: "direct", DiscordSRVConfig: filepath.Join(t.TempDir(), "none.yml")}})
	if err != nil || r.auth != "Bot direct" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := ReporterFor(&config.Config{DiscordConfig: config.DiscordConfig{DiscordSRVConfig: filepath.Join(t.TempDir(), "none.yml")}}); err == nil {
		t.Fatal("どちらも無ければ、エラー")
	}
}

func TestReportSanitizesPlayerText(t *testing.T) {
	s := &detect.Stats{
		User: detect.UserInfo{Name: "x", UUID: "u"}, World: "main", From: 1793757149, To: 1793757155,
		Commands: []string{"/msg a `@everyone [x](https://example.com)\nnext"},
	}
	txt := BuildReport(ReportInput{Stats: s, Trigger: "burst", Mode: config.ModeReview, Out: Outcome{Error: "bad `@here` [y](https://example.com)\nline"}})
	for _, bad := range []string{"@everyone", "@here", "](", "\nnext", "\nline"} {
		if strings.Contains(txt, bad) {
			t.Errorf("%q が残っている:\n%s", bad, txt)
		}
	}
	// 使ったコマンドのインラインコードを閉じられない (UUID と、コマンドの 2 組だけ)
	if n := strings.Count(txt, "`"); n != 4 {
		t.Errorf("バッククォートの数 %d:\n%s", n, txt)
	}
	noCmd := BuildReport(ReportInput{Stats: s, Trigger: "burst", Mode: config.ModeReview, Out: Outcome{Planned: true, CmdUntil: 1}})
	if !strings.Contains(noCmd, "コマンドを作れません") || strings.Contains(noCmd, "``") {
		t.Fatalf("コマンドが無いとき:\n%s", noCmd)
	}
}
