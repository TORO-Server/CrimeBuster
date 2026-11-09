package main

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testCfg() *Config {
	return &Config{
		BurstWindow: 5 * time.Second, BreakBurst: 100, PlaceBurst: 150,
		SustainedWindow: 10 * time.Minute, SustainedBreaks: 1500,
	}
}

func feedN(d *Detector, user int, sec int64, action, n int) *Trigger {
	var last *Trigger
	for i := 0; i < n; i++ {
		if t := d.Feed(Event{User: user, Time: sec, Action: action}); t != nil {
			last = t
		}
	}
	return last
}

func TestDetectorHumanSpeedNotFlagged(t *testing.T) {
	d := NewDetector(testCfg())
	// 1 秒に 8 個を 2 分続けても (960 個)、バーストにも長い窓にも当たらない
	for s := int64(1000); s < 1120; s++ {
		if tr := feedN(d, 1, s, actBreak, 8); tr != nil {
			t.Fatalf("flagged at %d: %v", s, tr)
		}
	}
}

func TestDetectorBurstBreak(t *testing.T) {
	d := NewDetector(testCfg())
	if tr := feedN(d, 1, 1000, actBreak, 60); tr != nil {
		t.Fatalf("early: %v", tr)
	}
	tr := feedN(d, 1, 1002, actBreak, 60) // 5 秒の窓に 120 個
	if tr == nil || tr.Kind != "burst-break" || tr.Breaks < 100 {
		t.Fatalf("want burst-break, got %v", tr)
	}
}

func TestDetectorWindowExpires(t *testing.T) {
	d := NewDetector(testCfg())
	feedN(d, 1, 1000, actBreak, 90)
	// 5 秒以上あいた次の 90 個は、足されない
	if tr := feedN(d, 1, 1010, actBreak, 90); tr != nil {
		t.Fatalf("flagged across the gap: %v", tr)
	}
}

func TestDetectorBurstPlace(t *testing.T) {
	d := NewDetector(testCfg())
	tr := feedN(d, 2, 1000, actPlace, 150)
	if tr == nil || tr.Kind != "burst-place" {
		t.Fatalf("want burst-place, got %v", tr)
	}
}

func TestDetectorSustained(t *testing.T) {
	d := NewDetector(testCfg())
	var tr *Trigger
	// 1 秒に 5 個 (人でも出せる速さ) を 5 分以上続ける → 1500 個で長い窓に当たる
	for s := int64(1000); s < 1400 && tr == nil; s++ {
		tr = feedN(d, 3, s, actBreak, 5)
	}
	if tr == nil || tr.Kind != "sustained-break" {
		t.Fatalf("want sustained-break, got %v", tr)
	}
}

func TestDetectorIgnoresClicksAndSeparatesUsers(t *testing.T) {
	d := NewDetector(testCfg())
	if tr := feedN(d, 1, 1000, actClick, 500); tr != nil {
		t.Fatalf("click flagged: %v", tr)
	}
	feedN(d, 1, 1000, actBreak, 60)
	if tr := feedN(d, 2, 1000, actBreak, 60); tr != nil {
		t.Fatalf("users mixed: %v", tr)
	}
}

func TestDetectorOutOfOrder(t *testing.T) {
	d := NewDetector(testCfg())
	feedN(d, 1, 1005, actBreak, 60)
	// 少し前の秒が後から来ても、窓の中なら足される
	tr := feedN(d, 1, 1003, actBreak, 60)
	if tr == nil {
		t.Fatal("out-of-order rows inside the window must count")
	}
}

func TestDetectorReset(t *testing.T) {
	d := NewDetector(testCfg())
	feedN(d, 1, 1000, actBreak, 99)
	d.Reset(1)
	if tr := feedN(d, 1, 1000, actBreak, 5); tr != nil {
		t.Fatalf("not reset: %v", tr)
	}
}

func TestRollbackCmd(t *testing.T) {
	if got := RollbackCmd("fuuty119", 415, "main"); got != "co rollback u:fuuty119 t:415s r:#main" {
		t.Fatal(got)
	}
	if got := RestoreCmd("fuuty119", 415, "main"); got != "co restore u:fuuty119 t:415s r:#main" {
		t.Fatal(got)
	}
	// データベースから来た値でも、安全な形でなければコマンドを作らない
	for _, c := range []struct{ name, world string }{
		{"a b", "main"}, {"x\nop", "main"}, {"", "main"}, {"x", ""}, {"x", "ma in"}, {"x", "a/b"}, {"x", "main\nstop"},
	} {
		if got := RollbackCmd(c.name, 10, c.world); got != "" {
			t.Errorf("RollbackCmd(%q, %q) = %q", c.name, c.world, got)
		}
		if got := RestoreCmd(c.name, 10, c.world); got != "" {
			t.Errorf("RestoreCmd(%q, %q) = %q", c.name, c.world, got)
		}
	}
}

func TestOpsRefusesUnsafeValues(t *testing.T) {
	con := &fakeConsole{}
	o := Ops{C: con}
	if err := o.Rollback(context.Background(), "a b", 10, "main"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("名前: %v", err)
	}
	if err := o.Rollback(context.Background(), "x", 10, "a b"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ワールド: %v", err)
	}
	if err := o.Kick(context.Background(), "x\nop", "r"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("キック: %v", err)
	}
	if len(con.sent()) != 0 {
		t.Fatalf("送ってしまった: %q", con.sent())
	}
}

func TestParseBlueMapIdle(t *testing.T) {
	idle := []string{"BlueMap Status >", "? 2 render-threads are idle", "? last active 2 hours ago", "? map overworld is updated"}
	if !parseBlueMapIdle(idle, "overworld") {
		t.Fatal("idle+updated must be true")
	}
	busy := []string{"BlueMap Status >", "? 2 render-threads are running", "? map overworld is updated"}
	if parseBlueMapIdle(busy, "overworld") {
		t.Fatal("running must be false")
	}
	notUpdated := []string{"? 2 render-threads are idle", "? map overworld is not updated"}
	if parseBlueMapIdle(notUpdated, "overworld") {
		t.Fatal("not updated must be false")
	}
	timed := []string{"> [12:00:00 INFO]: ✔ 2 render-threads are idle", "[12:00:00 INFO]: ✔ map overworld is updated"}
	if !parseBlueMapIdle(timed, "overworld") {
		t.Fatal("時刻のある行も読む")
	}
	// プレイヤーの発言は、状態の行にならない
	chat := []string{"[12:00:00 INFO]: <griefer> 2 render-threads are idle", "[12:00:01 INFO]: <griefer> map overworld is updated"}
	if parseBlueMapIdle(chat, "overworld") {
		t.Fatal("発言を状態と取り違えた")
	}
}

// 完了の判定は、CoreProtect の行だけ。プレイヤーの発言や、ほかの人のロールバックでは終わらない。
func TestConsoleCompletionLines(t *testing.T) {
	const done = `[12:35:53 INFO]: CoreProtect - Rollback completed for "griefer".`
	if !rollbackFinished(done, "griefer") || !rollbackFinished(done, "Griefer") || !rollbackFinished("> "+done, "griefer") {
		t.Fatal("完了の行")
	}
	for _, l := range []string{
		`[12:35:53 INFO]: <griefer> CoreProtect - Rollback completed for "griefer".`,
		`[12:35:53 INFO]: [Not Secure] <griefer> CoreProtect - Rollback completed for "griefer".`,
		`[12:35:53 INFO]: <griefer> [12:35:53 INFO]: CoreProtect - Rollback completed for "griefer".`,
		`[12:35:53 INFO]: CoreProtect - Rollback completed for "someone".`,
		`CoreProtect - Rollback completed for "griefer".`,
	} {
		if rollbackFinished(l, "griefer") {
			t.Errorf("完了と取り違えた: %s", l)
		}
	}
	for _, l := range []string{
		`[12:35:53 INFO]: CoreProtect - Rollback failed.`,
		`[12:35:53 INFO]: CoreProtect - Rollback or restore aborted.`,
		`[12:35:53 INFO]: CoreProtect - A rollback/restore is already in progress.`,
		`[12:35:53 INFO]: CoreProtect - No results found.`,
		`[12:35:53 INFO]: CoreProtect - Database busy. Please try again later.`,
	} {
		if !rollbackNG.MatchString(l) {
			t.Errorf("失敗を読めない: %s", l)
		}
	}
	if rollbackNG.MatchString(`[12:35:53 INFO]: <griefer> CoreProtect - Rollback failed.`) {
		t.Error("発言を失敗と取り違えた")
	}
	if !savedRE.MatchString(`[12:35:53 INFO]: Saved the game`) || savedRE.MatchString(`[12:35:53 INFO]: <griefer> Saved the game`) {
		t.Error("書き出しの完了の判定")
	}
}

func TestCleanLine(t *testing.T) {
	in := "\x1b[?1l\x1b>\x1b[K[12:35:53 INFO]: CoreProtect - Rollback completed for \"x\".\r"
	got := cleanLine(in)
	if !strings.HasPrefix(got, "[12:35:53 INFO]:") || !rollbackOK.MatchString(got) {
		t.Fatalf("got %q", got)
	}
}

// streamConsole: コマンドを送り、「完了」の行で止まる。送った行は \r で終わる。
func TestStreamConsole(t *testing.T) {
	pr, pw := io.Pipe() // サーバー → こちら
	var sent strings.Builder
	w := writerFunc(func(p []byte) (int, error) {
		sent.Write(p)
		go func() {
			io.WriteString(pw, "\x1b[K[10:00:00 INFO]: noise\r\n")
			io.WriteString(pw, "[10:00:01 INFO]: CoreProtect - Rollback completed for \"x\".\r\n")
		}()
		return len(p), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var lines []string
	err := streamConsole(ctx, w, pr, "co rollback u:x t:10s r:#main", 3*time.Second, func(l string) bool {
		lines = append(lines, l)
		return rollbackFinished(l, "x")
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.String() != "co rollback u:x t:10s r:#main\r" {
		t.Fatalf("sent %q", sent.String())
	}
	if len(lines) != 2 || !rollbackOK.MatchString(lines[1]) {
		t.Fatalf("lines %q", lines)
	}
}

func TestStreamConsoleTimeout(t *testing.T) {
	pr, _ := io.Pipe()
	w := writerFunc(func(p []byte) (int, error) { return len(p), nil })
	err := streamConsole(context.Background(), w, pr, "x", 300*time.Millisecond, func(string) bool { return false })
	if !errors.Is(err, errConsoleTimeout) {
		t.Fatalf("err=%v, want timeout", err)
	}
}

// 入出力が切れたら、完了と取り違えないように io.ErrUnexpectedEOF を返す。
func TestStreamConsoleDisconnect(t *testing.T) {
	w := writerFunc(func(p []byte) (int, error) { return len(p), nil })
	never := func(string) bool { return false }
	// コマンドを送る前に切れた
	wrote := false
	before := writerFunc(func(p []byte) (int, error) { wrote = true; return len(p), nil })
	if err := streamConsole(context.Background(), before, strings.NewReader(""), "x", time.Second, never); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("送る前: %v", err)
	}
	if wrote {
		t.Fatal("切れているのに送った")
	}
	// コマンドを送ったあとで切れた
	pr, pw := io.Pipe()
	after := writerFunc(func(p []byte) (int, error) {
		go pw.Close()
		return w(p)
	})
	if err := streamConsole(context.Background(), after, pr, "x", 5*time.Second, never); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("送ったあと: %v", err)
	}
}

// fakeConsole は Ops の試験用。
type fakeConsole struct {
	mu      sync.Mutex
	cmds    []string
	reply   map[string][]string
	err     error
	longest time.Duration // いちばん長い timeout
}

func (f *fakeConsole) Run(_ context.Context, cmd string, timeout time.Duration, done func(string) bool) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	f.longest = max(f.longest, timeout)
	if f.err != nil {
		return nil, f.err
	}
	for prefix, lines := range f.reply {
		if strings.HasPrefix(cmd, prefix) {
			for _, l := range lines {
				if done != nil && done(l) {
					break
				}
			}
			return lines, nil
		}
	}
	return nil, nil
}

// sent は、受けたコマンドの写し。
func (f *fakeConsole) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakeConsole) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = nil
}

func TestOpsRollbackFailureDetected(t *testing.T) {
	// 返事が「失敗」の行なら、エラーにする (done の判定が NG の行で true を返す)
	c := &fakeConsole{}
	o := Ops{C: &failConsole{c}}
	if err := o.Rollback(context.Background(), "x", 10, "main"); !errors.Is(err, errRollbackFailed) {
		t.Fatalf("err=%v", err)
	}
	// エラーに、コンソールの生の行を載せない
	if strings.Contains(errRollbackFailed.Error(), "INFO") {
		t.Fatal(errRollbackFailed)
	}
}

func TestRollbackVerified(t *testing.T) {
	dbDown := errors.New("db")
	consoleNG := errors.New("console")
	for _, c := range []struct {
		name        string
		console     error
		done, total int
		db          error
		want        bool
	}{
		{"両方よい", nil, 100, 100, nil, true},
		{"95% でよい", nil, 95, 100, nil, true},
		{"コンソールが完了でもデータベースで戻っていない", nil, 10, 100, nil, false},
		{"行が無い", nil, 0, 0, nil, false},
		{"コンソールが切れてもデータベースで戻っている", consoleNG, 100, 100, nil, true},
		{"コンソールが切れてデータベースでも戻っていない", consoleNG, 0, 100, nil, false},
		{"データベースが読めないときはコンソールの完了", nil, 0, 0, dbDown, true},
		{"どちらも確かめられない", consoleNG, 0, 0, dbDown, false},
	} {
		if got := rollbackVerified(c.console, c.done, c.total, c.db); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

type failConsole struct{ *fakeConsole }

func (f *failConsole) Run(ctx context.Context, cmd string, to time.Duration, done func(string) bool) ([]string, error) {
	line := "[10:00:00 INFO]: CoreProtect - Rollback failed."
	done(line)
	return []string{line}, nil
}

func TestReportText(t *testing.T) {
	s := &Stats{
		User: UserInfo{Name: "fuuty119", UUID: "53b4dddf-b884-4439-b26b-be0741d26e8c"}, World: "main",
		From: 1793757149, To: 1793757495, Breaks: 2642, Places: 158, PeakPerS: 421,
		X0: -5712, X1: -5612, Y0: 55, Y1: 94, Z0: -10204, Z1: -10049,
		TopBreaks: []MatCount{{"stone", 820}, {"sand", 357}},
		Commands:  []string{"/co i", "/co i"},
	}
	txt := BuildReport(ReportInput{Stats: s, Trigger: "burst-break", Warp: "ワープ kanso_town の近く (約 12 ブロック)",
		Center: &Cam{X: -5650, Z: -10125}, Out: Outcome{RolledBack: true, RolledDone: 2800, RolledTotal: 2800, RestoreCmd: "co restore u:fuuty119 t:415s r:#main"}, Mode: ModeLive,
		Images: []string{"1枚目 被害"}})
	for _, want := range []string{"**ワールド**: main", "**座標**: -5650 -10125 付近", "fuuty119", "2,642", "421 個/秒", "stone 820", "`/co i` ×2", "ロールバック済み (2,800 / 2,800 件)", "co restore u:fuuty119", "kanso_town"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q in:\n%s", want, txt)
		}
	}
	if len([]rune(txt)) > 2000 {
		t.Fatal("over discord limit")
	}
	dry := BuildReport(ReportInput{Stats: s, Out: Outcome{Planned: true, Cmd: "co rollback u:fuuty119 t:415s r:#main"}, Mode: ModeDryRun})
	if !strings.Contains(dry, "試運転") || !strings.Contains(dry, "対応 (予定)") {
		t.Fatalf("dry-run text: %s", dry)
	}
	later := BuildReport(ReportInput{Stats: s, Out: Outcome{Planned: true, Cmd: "co rollback u:fuuty119 t:4015s r:#main", CmdUntil: s.To + 3600}, Mode: ModeReview})
	for _, want := range []string{"ロールバック (未実行)", "t:4015s", "までに実行してください"} {
		if !strings.Contains(later, want) {
			t.Errorf("missing %q in:\n%s", want, later)
		}
	}
}

func TestComma(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 2642: "2,642", 1234567: "1,234,567"} {
		if got := comma(in); got != want {
			t.Errorf("comma(%d)=%s want %s", in, got, want)
		}
	}
}

func TestCamFor(t *testing.T) {
	// 点が 1 つの塊 (x -5660..-5640) と、遠くの外れ値 1 個 → 中心は塊、距離は外れ値で伸びない
	s := &Stats{X0: -5712, X1: -5602, Y0: 55, Y1: 94, Z0: -10210, Z1: -10048}
	for x := -5660; x <= -5640; x++ {
		for z := -10130; z <= -10120; z++ {
			s.Columns = append(s.Columns, Column{X: x, Z: z, N: 10})
		}
	}
	s.Columns = append(s.Columns, Column{X: -5712, Z: -10048, N: 1})
	c := CamFor(s)
	if c.X < -5660 || c.X > -5640 || c.Z < -10130 || c.Z > -10120 {
		t.Fatalf("center %v", c)
	}
	if c.Dist < 70 || c.Dist > 150 {
		t.Fatalf("outlier stretched the camera: dist %v", c.Dist)
	}
	if h := c.Hash("overworld"); !strings.HasPrefix(h, "#overworld:") || !strings.HasSuffix(h, ":0:0:perspective") {
		t.Fatalf("hash %s", h)
	}
	// 列が無いときは範囲の真ん中
	c2 := CamFor(&Stats{X0: 0, X1: 100, Z0: 0, Z1: 200, Y0: 64, Y1: 64})
	if c2.X != 50 || c2.Z != 100 {
		t.Fatalf("bbox center %v", c2)
	}
}
func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.S.Cursor = 42
	st.S.Incidents["7"] = &Incident{ID: "a", UserID: 7, Name: "x", Start: 1, Last: 2}
	st.S.Threads["x"] = ThreadRef{ThreadID: "99", At: 5}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.S.Cursor != 42 || st2.S.Incidents["7"].Name != "x" || st2.S.Threads["x"].ThreadID != "99" {
		t.Fatalf("%+v", st2.S)
	}
}

func TestConfigMode(t *testing.T) {
	t.Setenv("CRIMEBUSTER_MODE", "nope")
	if _, err := loadConfig(); err == nil {
		t.Fatal("bad mode must fail")
	}
	t.Setenv("CRIMEBUSTER_MODE", "")
	c, err := loadConfig()
	if err != nil || c.Mode != ModeObserve {
		t.Fatalf("default must be observe: %v %v", c, err)
	}
}

func TestLoadBotToken(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/config.yml"
	if err := writeFile(p, "ConfigVersion: 1\nBotToken: \"abc.def.ghi\"\nChannels: {\"global\": \"1\"}\n"); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadBotToken(p)
	if err != nil || tok != "abc.def.ghi" {
		t.Fatalf("%q %v", tok, err)
	}
	if err := writeFile(p, "BotToken: \"\"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBotToken(p); err == nil {
		t.Fatal("empty token must fail")
	}
}

func TestNearestWarp(t *testing.T) {
	ws := []Warp{{Name: "spawn", World: "main", X: 0, Z: 0}, {Name: "kanso_town", World: "main", X: -5728, Z: -10111}, {Name: "far", World: "flat", X: -5700, Z: -10100}}
	w, d, ok := Nearest(ws, "main", -5662, -10126)
	if !ok || w.Name != "kanso_town" || d > 100 {
		t.Fatalf("%v %v %v", w, d, ok)
	}
	if WarpText(w, d) == "" {
		t.Fatal("near warp must have text")
	}
	if WarpText(Warp{Name: "x"}, 2000) != "" {
		t.Fatal("far warp must be omitted")
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

func TestLoadOps(t *testing.T) {
	p := t.TempDir() + "/ops.json"
	if err := writeFile(p, `[{"uuid":"1","name":"test_player","level":4},{"uuid":"2","name":"Other","level":2}]`); err != nil {
		t.Fatal(err)
	}
	ops := LoadOps(p)
	if !ops["test_player"] || !ops["other"] || len(ops) != 2 {
		t.Fatalf("%v", ops)
	}
	if len(LoadOps("")) != 0 || len(LoadOps(t.TempDir()+"/none.json")) != 0 {
		t.Fatal("missing file must be empty")
	}
}

func TestConfigSecretsFromFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) string {
		p := filepath.Join(dir, name)
		if err := writeFile(p, value); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("CRIMEBUSTER_MODE", "")
	t.Setenv("DB_PASSWORD", "from-env")
	t.Setenv("AI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "key-env")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	c, err := loadConfig()
	if err != nil || c.DBPassword != "from-env" || c.AIKey != "key-env" || c.BotToken != "" {
		t.Fatalf("env: %+v %v", c, err)
	}
	t.Setenv("DB_PASSWORD_FILE", write("db", "from-file\n"))
	t.Setenv("AI_API_KEY_FILE", write("ai", "key-file\n"))
	t.Setenv("DISCORD_BOT_TOKEN_FILE", write("bot", "bot-file\n"))
	c, err = loadConfig()
	if err != nil || c.DBPassword != "from-file" || c.AIKey != "key-file" || c.BotToken != "bot-file" {
		t.Fatalf("file: %+v %v", c, err)
	}
	// ファイルが読めないのに、空のまま動かない
	t.Setenv("DB_PASSWORD_FILE", filepath.Join(dir, "none"))
	if _, err := loadConfig(); err == nil {
		t.Fatal("読めない秘密のファイルは、エラー")
	}
}

func TestReporterForPrefersBotToken(t *testing.T) {
	r, err := reporterFor(&Config{BotToken: "direct", DiscordSRVConfig: filepath.Join(t.TempDir(), "none.yml")})
	if err != nil || r.auth != "Bot direct" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := reporterFor(&Config{DiscordSRVConfig: filepath.Join(t.TempDir(), "none.yml")}); err == nil {
		t.Fatal("どちらも無ければ、エラー")
	}
}

func TestBlueMapAreaIsCapped(t *testing.T) {
	// 遠くに 1 個だけ壊すと範囲は広がるが、半径は上限で止まり、中心はカメラ (中央値) のまま
	s := &Stats{X0: -30000, X1: 30000, Z0: 0, Z1: 10}
	for x := 95; x <= 105; x++ {
		s.Columns = append(s.Columns, Column{X: x, Z: 5, N: 50})
	}
	s.Columns = append(s.Columns, Column{X: -30000, Z: 0, N: 1}, Column{X: 30000, Z: 10, N: 1})
	cx, cz, r := blueMapArea(s, CamFor(s))
	if r != maxBlueMapRadius || cx < 95 || cx > 105 || cz != 5 {
		t.Fatalf("cx=%d cz=%d r=%d", cx, cz, r)
	}
	// 狭い範囲は、今までどおり
	small := &Stats{X0: 0, X1: 100, Z0: 0, Z1: 20}
	if _, _, r := blueMapArea(small, Cam{X: 50, Z: 10}); r != 98 {
		t.Fatalf("r=%d", r)
	}
	if _, _, r := blueMapArea(&Stats{}, Cam{}); r != 64 {
		t.Fatalf("最小の半径: %d", r)
	}
}

func TestWaitRefreshKeepsInterval(t *testing.T) {
	const every = 400 * time.Millisecond
	g := &Guard{refreshEvery: every}
	start := time.Now()
	if err := g.waitRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) >= every {
		t.Fatal("1 回目は待たない")
	}
	first := g.lastRefresh
	if err := g.waitRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastRefresh.Sub(first) < every {
		t.Fatal("2 回目は間隔を空ける")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.waitRefresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestIncidentID(t *testing.T) {
	const at = 1793757149
	for _, c := range []struct {
		prefix, name string
		safe         bool
	}{
		{"", "fuuty119", true},
		{"chat-", "a.b_c", true},
		{"", "../../x", false},
		{"", "a..b", false},
		{"chat-", "a b", false},
		{"", "", false},
		{"", "x\ny", false},
	} {
		id := incidentID(c.prefix, at, c.name, 42)
		if !validID(id) || !strings.HasPrefix(id, c.prefix) {
			t.Errorf("%q: ID %q", c.name, id)
		}
		if got := strings.HasSuffix(id, "-uid42"); got == c.safe {
			t.Errorf("%q: ID %q", c.name, id)
		}
	}
}

func TestChatByUser(t *testing.T) {
	parts := chatByUser([]ChatMsg{{RowID: 1, User: 5}, {RowID: 2, User: 7}, {RowID: 3, User: 5}})
	if len(parts) != 2 || len(parts[0]) != 2 || parts[0][1].RowID != 3 || parts[1][0].RowID != 2 {
		t.Fatalf("%+v", parts)
	}
}

func TestSafeChat(t *testing.T) {
	got := safeChat("a\nb\x00c @everyone `x` [click](https://example.com) \u2028 d", 200)
	for _, bad := range []string{"\n", "\x00", "\u2028", "@everyone", "`", "]("} {
		if strings.Contains(got, bad) {
			t.Errorf("%q が残っている: %q", bad, got)
		}
	}
	if r := []rune(safeChat(strings.Repeat("あ", 50), 10)); len(r) != 11 {
		t.Fatalf("長さ: %d", len(r))
	}
}

func TestReportSanitizesPlayerText(t *testing.T) {
	s := &Stats{
		User: UserInfo{Name: "x", UUID: "u"}, World: "main", From: 1793757149, To: 1793757155,
		Commands: []string{"/msg a `@everyone [x](https://example.com)\nnext"},
	}
	txt := BuildReport(ReportInput{Stats: s, Trigger: "burst", Mode: ModeReview, Out: Outcome{Error: "bad `@here` [y](https://example.com)\nline"}})
	for _, bad := range []string{"@everyone", "@here", "](", "\nnext", "\nline"} {
		if strings.Contains(txt, bad) {
			t.Errorf("%q が残っている:\n%s", bad, txt)
		}
	}
	// 使ったコマンドのインラインコードを閉じられない (UUID と、コマンドの 2 組だけ)
	if n := strings.Count(txt, "`"); n != 4 {
		t.Errorf("バッククォートの数 %d:\n%s", n, txt)
	}
	noCmd := BuildReport(ReportInput{Stats: s, Trigger: "burst", Mode: ModeReview, Out: Outcome{Planned: true, CmdUntil: 1}})
	if !strings.Contains(noCmd, "コマンドを作れません") || strings.Contains(noCmd, "``") {
		t.Fatalf("コマンドが無いとき:\n%s", noCmd)
	}
}

func TestSweepData(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGuard(&Config{DataDir: dir, ReportRetention: 24 * time.Hour}, nil, st, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	put := func(id string, created time.Time) string {
		d := filepath.Join(dir, "reports", id)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := saveRecord(d, &Record{ID: id, Created: created.Unix(), State: StatePending}); err != nil {
			t.Fatal(err)
		}
		return d
	}
	old := put("old", now.Add(-48*time.Hour))
	fresh := put("fresh", now.Add(-time.Hour))
	busy := put("busy", now.Add(-48*time.Hour))
	g.busy["busy"] = "rollback"
	bare := filepath.Join(dir, "reports", "bare") // 記録の無いフォルダは、更新時刻で決める
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bare, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(dir, "held", "keep") // reports/ と shots/ の外には触らない
	if err := os.MkdirAll(held, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(held, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	shots := filepath.Join(dir, "shots")
	if err := os.MkdirAll(shots, 0o755); err != nil {
		t.Fatal(err)
	}
	oldShot, newShot := filepath.Join(shots, "a.png"), filepath.Join(shots, "b.png")
	_ = writeFile(oldShot, "x")
	_ = writeFile(newShot, "x")
	if err := os.Chtimes(oldShot, now.Add(-3*time.Hour), now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}

	g.sweepData(now)

	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	for p, want := range map[string]bool{old: false, bare: false, oldShot: false, fresh: true, busy: true, held: true, newShot: true} {
		if exists(p) != want {
			t.Errorf("%s: exists=%v, want %v", p, !want, want)
		}
	}

	// 0 なら、事件は消さない
	g.cfg.ReportRetention = 0
	g.sweepData(now.Add(1000 * time.Hour))
	if !exists(fresh) {
		t.Fatal("REPORT_RETENTION=0 で消した")
	}
}

// healthcheck の印は、監視の 1 回が成功したときだけ新しくなる。
func TestRunTouchesHealthOnlyAfterSuccess(t *testing.T) {
	g, _, db := newDBGuard(t, ModeObserve,
		fakeAnswer{has: "MAX(rowid)", rows: [][]driver.Value{{int64(1)}}},
		fakeAnswer{has: "WHERE rowid > ?", err: errors.New("データベースが止まっている")},
	)
	g.cfg.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("監視が回らない", func() bool { return len(db.find("WHERE rowid > ?")) >= 3 })
	if err := checkHealthFile(g.cfg.DataDir, time.Now()); err == nil {
		t.Fatal("失敗しているのに、印がある")
	}
	db.set(fakeAnswer{has: "WHERE rowid > ?"}) // 直った (新しい行は無い)
	waitFor("成功しても印が付かない", func() bool { return checkHealthFile(g.cfg.DataDir, time.Now()) == nil })
}

func TestCheckHealthFile(t *testing.T) {
	dir := t.TempDir()
	if err := checkHealthFile(dir, time.Now()); err == nil {
		t.Fatal("ファイルが無ければ、エラー")
	}
	touchHealth(&Config{DataDir: dir})
	if err := checkHealthFile(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := checkHealthFile(dir, time.Now().Add(3*time.Minute)); err == nil {
		t.Fatal("古ければ、エラー")
	}
}

// newDBGuard は、偽のデータベースと偽のコンソールを持つ Guard。
func newDBGuard(t *testing.T, mode Mode, answers ...fakeAnswer) (*Guard, *fakeConsole, *fakeDB) {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp, db := newFakeCP(t, answers...)
	con := &fakeConsole{}
	cfg := &Config{Mode: mode, DataDir: dir, WEGrace: 30 * time.Minute, ChatMuteCmd: "mute {name} 10m spam", TrustedPlayers: map[string]bool{}, MapIDs: map[string]string{}}
	g := NewGuard(cfg, cp, st, &Ops{C: con}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.rollbackConfirmWait = time.Millisecond
	return g, con, db
}

func userRow(name string) fakeAnswer {
	return fakeAnswer{has: "FROM coreprotect_user WHERE rowid", rows: [][]driver.Value{{name, "uuid-1"}}}
}

func TestConsiderEditCommand(t *testing.T) {
	edit := fakeAnswer{has: "message LIKE", rows: [][]driver.Value{{"//set `@everyone"}}}
	ev := Event{User: 7, Time: 1793757149}
	tr := Trigger{User: 7, Time: ev.Time, Kind: "burst-break", Breaks: 120, Window: 5}

	// review: 抑止せず、事件に注記して確認待ちに置く
	g, _, _ := newDBGuard(t, ModeReview, userRow("griefer"), edit)
	g.consider(context.Background(), tr, ev)
	inc := g.store.S.Incidents["7"]
	if inc == nil || !strings.Contains(inc.Trigger, "編集コマンドの使用あり") || strings.Contains(inc.Trigger, "@everyone") || strings.Contains(inc.Trigger, "`") {
		t.Fatalf("incident: %+v", inc)
	}

	// それ以外の動かし方は、今までどおり対象外
	for _, mode := range []Mode{ModeObserve, ModeDryRun, ModeLive} {
		g, _, _ := newDBGuard(t, mode, userRow("griefer"), edit)
		g.consider(context.Background(), tr, ev)
		if len(g.store.S.Incidents) != 0 || g.suppressed[7] == 0 {
			t.Errorf("%s: %+v", mode, g.store.S.Incidents)
		}
	}

	// コマンドが無ければ、注記は付かない
	g, _, _ = newDBGuard(t, ModeReview, userRow("griefer"))
	g.consider(context.Background(), tr, ev)
	if inc := g.store.S.Incidents["7"]; inc == nil || strings.Contains(inc.Trigger, "編集コマンド") {
		t.Fatalf("incident: %+v", inc)
	}
}

func TestConsiderUnsafeNameUsesSafeID(t *testing.T) {
	g, _, _ := newDBGuard(t, ModeReview, userRow("../../etc"))
	g.consider(context.Background(), Trigger{User: 7, Kind: "burst-break"}, Event{User: 7, Time: 1793757149})
	inc := g.store.S.Incidents["7"]
	if inc == nil || !validID(inc.ID) || !strings.HasSuffix(inc.ID, "-uid7") {
		t.Fatalf("incident: %+v", inc)
	}
}

func TestLiveKickDoesNotBlockDetection(t *testing.T) {
	g, con, _ := newDBGuard(t, ModeLive, userRow("griefer"))
	g.cfg.KickEnabled = true
	g.mu.Lock() // onEvent と同じに g.mu を持って呼ぶ
	g.consider(context.Background(), Trigger{User: 7, Kind: "burst-break"}, Event{User: 7, Time: 1793757149})
	g.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for len(con.sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s := con.sent(); len(s) != 1 || !strings.HasPrefix(s[0], "kick griefer ") {
		t.Fatalf("sent %q", s)
	}
}

func chatRows(user int64) fakeAnswer {
	// ChatFor は新しい順に読む
	return fakeAnswer{has: "chat WHERE user = ?", rows: [][]driver.Value{
		{int64(2), int64(1793757150), user, "二つ目"},
		{int64(1), int64(1793757149), user, "一つ目"},
	}}
}

// AI の判定だけの事件は、live でもミュートしない。決まりの検知が重なれば、ミュートする。
func TestAIOnlyChatIsNotMuted(t *testing.T) {
	g, con, _ := newDBGuard(t, ModeLive, userRow("talker"), chatRows(9))
	ctx := context.Background()
	g.onChat(ctx, ChatMsg{RowID: 1, Time: 1793757149, User: 9, Msg: "a"}, &ChatFlag{Category: "hate", Confidence: 90, Summary: "s"})
	inc := g.store.S.Incidents[chatKey(9)]
	if inc == nil || !inc.AIOnly {
		t.Fatalf("incident: %+v", inc)
	}
	res, err := g.ProcessChat(ctx, inc, 1793757160)
	if err != nil {
		t.Fatal(err)
	}
	if res.Muted || len(con.sent()) != 0 || strings.Contains(res.Report, "mute") {
		t.Fatalf("muted=%v sent=%q report:\n%s", res.Muted, con.sent(), res.Report)
	}
	if !strings.Contains(res.Report, "一つ目") || strings.Index(res.Report, "一つ目") > strings.Index(res.Report, "二つ目") {
		t.Fatalf("発言は時間の順:\n%s", res.Report)
	}

	// 決まりの検知が重なると、AI だけの事件ではなくなる
	g.cfg.ChatRun = 3
	g.onChat(ctx, ChatMsg{RowID: 2, Time: 1793757150, User: 9, Msg: "aaaa"}, nil)
	if inc.AIOnly {
		t.Fatal("AIOnly のまま")
	}
	res, err = g.ProcessChat(ctx, inc, 1793757160)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Muted || len(con.sent()) != 1 || con.sent()[0] != "mute talker 10m spam" {
		t.Fatalf("muted=%v sent=%q", res.Muted, con.sent())
	}
}

func TestProcessRefusesUnsafeIncidentID(t *testing.T) {
	g, _, _ := newDBGuard(t, ModeReview)
	inc := &Incident{ID: "../../x", Name: "x"}
	if _, err := g.processWindow(context.Background(), inc, 1); err == nil {
		t.Fatal("ブロックの事件")
	}
	if _, err := g.ProcessChat(context.Background(), inc, 1); err == nil {
		t.Fatal("チャットの事件")
	}
}

func TestChatForLimitsAndOrders(t *testing.T) {
	cp, db := newFakeCP(t, chatRows(9))
	msgs, err := cp.ChatFor(context.Background(), 9, 1, 2)
	if err != nil || len(msgs) != 2 || msgs[0].Msg != "一つ目" {
		t.Fatalf("%+v %v", msgs, err)
	}
	q := db.find("chat WHERE user = ?")[0]
	if !strings.Contains(q.q, "LIMIT ?") || q.args[3] != int64(maxChatFor) {
		t.Fatalf("%+v", q)
	}
}

func TestRolledBackCountsTheWorld(t *testing.T) {
	cp, db := newFakeCP(t, fakeAnswer{has: "rolled_back", rows: [][]driver.Value{{int64(3), int64(4)}}})
	done, total, err := cp.RolledBack(context.Background(), 7, "main", 1, 2)
	if err != nil || done != 3 || total != 4 {
		t.Fatal(done, total, err)
	}
	q := db.find("rolled_back")[0]
	if !strings.Contains(q.q, "coreprotect_world WHERE world = ?") || q.args[3] != "main" {
		t.Fatalf("%+v", q)
	}
}

// 接続が 1 本でも、マテリアルの名前を引くところで止まらない。
func TestStatsDoesNotHoldRowsWhileLookingUpMaterial(t *testing.T) {
	cp, _ := newFakeCP(t,
		fakeAnswer{has: "SELECT action, COUNT(*)", rows: [][]driver.Value{{int64(actBreak), int64(10), int64(1000), int64(1010)}}},
		fakeAnswer{has: "SELECT wid, COUNT(*)", rows: [][]driver.Value{{int64(1), int64(10)}}},
		fakeAnswer{has: "world GROUP BY", rows: [][]driver.Value{{int64(1), "main"}}},
		fakeAnswer{has: "SELECT MIN(x)", rows: [][]driver.Value{{int64(0), int64(10), int64(60), int64(70), int64(0), int64(10)}}},
		fakeAnswer{has: "SELECT time, COUNT(*)", rows: [][]driver.Value{{int64(1005), int64(5)}}},
		fakeAnswer{has: "SELECT type, COUNT(*)", rows: [][]driver.Value{{int64(3), int64(10)}}},
		fakeAnswer{has: "material_map GROUP BY", rows: [][]driver.Value{{int64(3), "minecraft:stone"}}},
	)
	cp.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := cp.Stats(ctx, UserInfo{ID: 7, Name: "x"}, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if s.World != "main" || len(s.TopBreaks) != 1 || s.TopBreaks[0].Material != "stone" {
		t.Fatalf("%+v", s)
	}
}

func TestSummaryLimitsCommands(t *testing.T) {
	s := &Stats{Commands: []string{"/x " + strings.Repeat("a", 500) + "\nnext"}}
	got := summaryOf(s)["commands"].([]string)
	if len(got) != 1 || len([]rune(got[0])) > 121 || strings.Contains(got[0], "\n") {
		t.Fatalf("%q", got)
	}
}
