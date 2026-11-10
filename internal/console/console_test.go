package console

import (
	"context"
	"crime-buster/internal/domain"
	"crime-buster/internal/testutil"
	"errors"
	"strings"
	"testing"
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

// 応答の判定: 書き出しは「書き出した」の文面、ロールバックは失敗の文面だけを見る。
// RCON の応答には送った相手への返事しか入らないので、発言と取り違えることはない。
func TestOpsResponseChecks(t *testing.T) {
	ctx := context.Background()
	ok := &testutil.FakeConsole{Reply: map[string][]string{
		"save-all flush": {"Saving the game (this may take a moment!)Saved the game"},
	}}
	if err := (Ops{C: ok}).SaveFlush(ctx); err != nil {
		t.Fatalf("書き出し: %v", err)
	}
	quiet := &testutil.FakeConsole{}
	if err := (Ops{C: quiet}).SaveFlush(ctx); err == nil {
		t.Fatal("確認の文面が無いのに成功とした")
	}
	// ロールバック: 応答が空でも、失敗の文面でなければ送れたものとする (成否はデータベースで確かめる)
	if err := (Ops{C: quiet}).Rollback(ctx, "x", 10, "main"); err != nil {
		t.Fatalf("ロールバック (応答なし): %v", err)
	}
	for _, line := range []string{
		"CoreProtect - Rollback failed.",
		"CoreProtect - Rollback or restore aborted.",
		"CoreProtect - A rollback/restore is already in progress.",
		"CoreProtect - No results found.",
		"CoreProtect - Database busy. Please try again later.",
		"Rollback cancelled by another process",
	} {
		con := &testutil.FakeConsole{Reply: map[string][]string{"co rollback": {line}}}
		if err := (Ops{C: con}).Rollback(ctx, "x", 10, "main"); !errors.Is(err, ErrRollbackFailed) {
			t.Errorf("失敗を読めない: %q -> %v", line, err)
		}
	}
	// 完了の文面は、失敗ではない
	done := &testutil.FakeConsole{Reply: map[string][]string{"co rollback": {`CoreProtect - Rollback completed for "x".`}}}
	if err := (Ops{C: done}).Rollback(ctx, "x", 10, "main"); err != nil {
		t.Fatalf("完了: %v", err)
	}
}

func TestOpsRollbackFailureDetected(t *testing.T) {
	c := &testutil.FakeConsole{}
	o := Ops{C: &testutil.FailConsole{FakeConsole: c}}
	if err := o.Rollback(context.Background(), "x", 10, "main"); !errors.Is(err, ErrRollbackFailed) {
		t.Fatalf("err=%v", err)
	}
	// エラーに、コンソールの生の行を載せない
	if strings.Contains(ErrRollbackFailed.Error(), "INFO") {
		t.Fatal(ErrRollbackFailed)
	}
}
