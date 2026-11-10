package console

import (
	"context"
	"crime-buster/internal/domain"
	"strings"
	"testing"
)

func TestConsoleAllow(t *testing.T) {
	a, err := NewAllow("mute {name} 10m spam")
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("あ", MaxKickReason)
	for _, cmd := range []string{
		"save-all flush",
		"list",
		"co rollback u:fuuty119 t:415s r:#main",
		"co rollback u:a.b_c t:86405s r:#main_nether",
		"kick spammer 荒らし op everyone",
		"kick spammer " + long,
		"mute Steve_1 10m spam",
	} {
		if !a.Allowed(cmd) {
			t.Errorf("通るはず: %q", cmd)
		}
	}
	for _, cmd := range []string{
		"",
		"save-all",
		"save-all flush ",
		"bluemap",
		"bluemap update overworld 1 2 3",
		"list extra",
		"co restore u:x t:10s r:#main",
		"co rollback u:x t:10s",
		"co rollback u:x t:10s r:#main extra",
		"co rollback u:a;b t:10s r:#main",
		"co rollback u:x t:0s r:#main",
		"co rollback u:x t:999999s r:#main",
		"co rollback u:x t:10s r:#ma in",
		"co rollback u:x t:10s r:#main\rstop",
		"kick x",
		"kick x ",
		"kick a;b reason",
		"kick x line\nnext",
		"kick x line\rnext",
		"kick x line next",
		"kick x " + long + "あ",
		"mute Steve_1 10m spam extra",
		"mute a b 10m spam",
		"mute Steve_1 10m spamx",
		"mute Steve_1\n10m spam",
		"stop",
		"say hello",
		"op someone",
	} {
		if a.Allowed(cmd) {
			t.Errorf("断るはず: %q", cmd)
		}
	}
}

func TestConsoleAllowMuteTemplate(t *testing.T) {
	a, err := NewAllow("")
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed("mute Steve 10m spam") {
		t.Fatal("CHAT_MUTE_CMD が空なら、ミュートは通さない")
	}
	for _, bad := range []string{"mute all", "mute {name}\nstop"} {
		if _, err := NewAllow(bad); err == nil {
			t.Errorf("テンプレート %q は断るはず", bad)
		}
	}
	// {name} が 2 つなら、同じ名前のときだけ通す
	a, err = NewAllow("tempmute {name} 5m ({name})")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Allowed("tempmute Steve 5m (Steve)") || a.Allowed("tempmute Steve 5m (Alex)") {
		t.Fatal("{name} の照合")
	}
}

// crime-buster が送るコマンドは、すべて許可を通る。
func TestAllowAcceptsWhatOpsSend(t *testing.T) {
	const muteTmpl = "mute {name} 10m spam"
	a, err := NewAllow(muteTmpl)
	if err != nil {
		t.Fatal(err)
	}
	con := &recordingConsole{}
	ops := Ops{C: con}
	ctx := context.Background()
	_ = ops.SaveFlush(ctx)
	_ = ops.Ping(ctx)
	_ = ops.Rollback(ctx, "fuuty119", domain.MaxRollbackAge+5, "main")
	_ = ops.Kick(ctx, "fuuty119", "ブロックを不自然な速さで変えたため")
	_ = ops.Kick(ctx, "fuuty119", domain.CleanText("理由\nつき"+strings.Repeat("字", 200), MaxKickReason))
	_, _ = con.Run(ctx, MuteCommand(muteTmpl, "fuuty119"))
	sent := con.Sent()
	if len(sent) != 6 {
		t.Fatalf("commands: %q", sent)
	}
	for _, cmd := range sent {
		if !a.Allowed(cmd) {
			t.Errorf("許可に断られる: %q", cmd)
		}
	}
}

type recordingConsole struct{ cmds []string }

func (r *recordingConsole) Run(_ context.Context, cmd string) (string, error) {
	r.cmds = append(r.cmds, cmd)
	return "", nil
}

func (r *recordingConsole) Sent() []string { return append([]string(nil), r.cmds...) }
