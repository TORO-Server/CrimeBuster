package relay

import (
	"context"
	"crime-buster/internal/domain"
	"crime-buster/internal/testutil"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestMuteCommand(t *testing.T) {
	if got := MuteCommand("mute {name} 10m spam", "Steve_1"); got != "mute Steve_1 10m spam" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{"a b", "x\nop me", "a;b", "", strings.Repeat("a", 40)} {
		if got := MuteCommand("mute {name}", bad); got != "" {
			t.Fatalf("不正な名前 %q にコマンドを作った: %q", bad, got)
		}
	}
	if got := MuteCommand("", "Steve"); got != "" {
		t.Fatalf("テンプレートが空なら何もしない: %q", got)
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
	con := &testutil.FakeConsole{}
	o := Ops{C: con}
	if err := o.Rollback(context.Background(), "a b", 10, "main"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("名前: %v", err)
	}
	if err := o.Rollback(context.Background(), "x", 10, "a b"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("ワールド: %v", err)
	}
	if err := o.Kick(context.Background(), "x\nop", "r"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("キック: %v", err)
	}
	if len(con.Sent()) != 0 {
		t.Fatalf("送ってしまった: %q", con.Sent())
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

func TestOpsRollbackFailureDetected(t *testing.T) {
	// 返事が「失敗」の行なら、エラーにする (done の判定が NG の行で true を返す)
	c := &testutil.FakeConsole{}
	o := Ops{C: &testutil.FailConsole{FakeConsole: c}}
	if err := o.Rollback(context.Background(), "x", 10, "main"); !errors.Is(err, errRollbackFailed) {
		t.Fatalf("err=%v", err)
	}
	// エラーに、コンソールの生の行を載せない
	if strings.Contains(errRollbackFailed.Error(), "INFO") {
		t.Fatal(errRollbackFailed)
	}
}
