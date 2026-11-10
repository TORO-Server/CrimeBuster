package app

import (
	"context"
	"crime-buster/internal/capture"
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"crime-buster/internal/domain"
	"crime-buster/internal/notify"
	"crime-buster/internal/store"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// このファイルのフェイクは、ポート (ports.go) の差し替え可能性を確かめるためのもの。
// testutil には置かない: testutil は検知・通報・撮影のパッケージの試験からも使われており、
// そちらの型を取り込むと import が循環する。

type fakeReader struct {
	chat  []detect.ChatMsg
	user  detect.UserInfo
	stats *detect.Stats
}

func (f *fakeReader) MaxRowID(context.Context) (int64, error)     { return 0, nil }
func (f *fakeReader) MaxChatRowID(context.Context) (int64, error) { return 0, nil }

func (f *fakeReader) Poll(context.Context, int64, int) ([]detect.Event, error) {
	return nil, nil
}

func (f *fakeReader) PollChat(context.Context, int64, int) ([]detect.ChatMsg, error) {
	return nil, nil
}

func (f *fakeReader) User(_ context.Context, id int) (detect.UserInfo, error) {
	u := f.user
	u.ID = id
	return u, nil
}

func (f *fakeReader) RecentEditCommand(context.Context, int, int64) (string, error) {
	return "", nil
}

func (f *fakeReader) Stats(context.Context, detect.UserInfo, int64, int64) (*detect.Stats, error) {
	return f.stats, nil
}

func (f *fakeReader) RolledBack(context.Context, int, string, int64, int64) (int, int, error) {
	return 0, 0, nil
}

func (f *fakeReader) ChatFor(context.Context, int, int64, int64) ([]detect.ChatMsg, error) {
	return f.chat, nil
}

type fakeJudge struct{}

func (fakeJudge) ReviewIncident(context.Context, *detect.Stats, string, []string) (*domain.Verdict, error) {
	return nil, nil
}

func (fakeJudge) ReviewChat(context.Context, []detect.ChatMsg, map[int]string) ([]notify.ChatFlag, error) {
	return nil, nil
}

type fakeReporter struct {
	thread string
	posts  int
}

func (f *fakeReporter) Post(context.Context, string, string, string, []notify.ReportFile, bool) (string, error) {
	f.posts++
	return f.thread, nil
}

func (f *fakeReporter) PostTags(context.Context, string, string, string, []notify.ReportFile, []string) (string, error) {
	f.posts++
	return f.thread, nil
}

func (f *fakeReporter) TagID() string { return "" }

type fakeShooter struct{ shots int }

func (f *fakeShooter) Shot(context.Context, string, capture.Cam, string, *capture.Annot) error {
	f.shots++
	return nil
}

func (f *fakeShooter) OverlayResult() string { return "ok: fake" }

type fakeConsole struct{ ran []string }

func (f *fakeConsole) Run(_ context.Context, cmd string, _ time.Duration, _ func(string) bool) ([]string, error) {
	f.ran = append(f.ran, cmd)
	return nil, nil
}

func (f *fakeConsole) Rollback(context.Context, string, int, string) error { return nil }
func (f *fakeConsole) Kick(context.Context, string, string) error          { return nil }
func (f *fakeConsole) SaveFlush(context.Context) error                     { return nil }

func (f *fakeConsole) BlueMapUpdate(context.Context, string, int, int, int) error {
	return nil
}

func (f *fakeConsole) WaitBlueMap(context.Context, string, time.Duration) error { return nil }

// fakeGuard は、全てのポートにフェイクを差した Guard を作る。
func fakeGuard(t *testing.T, mode config.Mode, p ports) (*Guard, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Mode:          mode,
		StorageConfig: config.StorageConfig{DataDir: dir},
		ChatConfig:    config.ChatConfig{ChatMuteCmd: "mute {name} 10m spam"},
		DetectConfig:  config.DetectConfig{TrustedPlayers: map[string]bool{}},
	}
	return newGuard(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)), p), dir
}

// 全てのポートにフェイクを差した構成で、チャットの処理と事件への操作が従来どおり動くこと。
func TestPortsAcceptFakes(t *testing.T) {
	rep := &fakeReporter{thread: "thread-1"}
	con := &fakeConsole{}
	g, dir := fakeGuard(t, config.ModeLive, ports{
		cp: &fakeReader{chat: []detect.ChatMsg{
			{RowID: 1, Time: 100, User: 9, Msg: "一つ目"},
			{RowID: 2, Time: 120, User: 9, Msg: "二つ目"},
		}},
		ai: fakeJudge{}, console: con, shooter: &fakeShooter{}, reporter: rep,
	})
	ctx := context.Background()

	// チャットの事件の処理: ミュートは Console、通報は Reporter のフェイクで動く
	inc := &store.Incident{ID: "chat-9-100", UserID: 9, Name: "talker", UUID: "uuid-9", Kind: "chat", Start: 100, Last: 120, Trigger: "rate: test"}
	res, err := g.ProcessChat(ctx, inc, 130)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Muted || res.ThreadID != "thread-1" {
		t.Fatalf("muted=%v thread=%q", res.Muted, res.ThreadID)
	}
	if len(con.ran) != 1 || con.ran[0] != "mute talker 10m spam" {
		t.Fatalf("console: %q", con.ran)
	}
	rec, err := g.Record(inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != domain.StateReported || rec.Thread != "thread-1" || !rec.Muted {
		t.Fatalf("record: %+v", rec)
	}

	// 事件への操作 (通報): チャットの事件は report.md を本文にする
	dir2 := filepath.Join(dir, "reports", "chat-9-200")
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "report.md"), []byte("報告の本文"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecord(dir2, &domain.Record{ID: "chat-9-200", Kind: "chat", Name: "talker", State: domain.StatePending}); err != nil {
		t.Fatal(err)
	}
	got, err := g.ReportIncident(ctx, "chat-9-200", "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.StateReported || got.Thread != "thread-1" || rep.posts != 2 {
		t.Fatalf("reported: %+v posts=%d", got, rep.posts)
	}

	// 事件への操作 (却下)
	dir3 := filepath.Join(dir, "reports", "chat-9-300")
	if err := os.MkdirAll(dir3, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecord(dir3, &domain.Record{ID: "chat-9-300", Kind: "chat", Name: "talker", State: domain.StatePending}); err != nil {
		t.Fatal(err)
	}
	dismissed, err := g.Dismiss("chat-9-300", "tester", "対象外")
	if err != nil {
		t.Fatal(err)
	}
	if dismissed.State != domain.StateDismissed {
		t.Fatalf("dismissed: %+v", dismissed)
	}
}

// 依存が無いときは、ポート自体が nil であり、「使えない」という判定が従来どおり効くこと。
func TestNilDependenciesStayNil(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Mode:          config.ModeObserve,
		StorageConfig: config.StorageConfig{DataDir: dir},
		DetectConfig:  config.DetectConfig{TrustedPlayers: map[string]bool{}},
	}
	g := NewGuard(cfg, nil, st, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if g.cp != nil || g.ai != nil || g.console != nil || g.shooter != nil || g.reporter != nil {
		t.Fatal("ポートに型付き nil が入っている")
	}
	if g.incidents.cp != nil || g.incidents.reporter != nil || g.chat.console != nil || g.chat.reporter != nil {
		t.Fatal("サービス側のポートに型付き nil が入っている")
	}
	if g.CP() != nil || g.Reporter() != nil || g.Shooter() != nil || g.Ops() != nil || g.AI() != nil {
		t.Fatal("アクセサが nil でないものを返した")
	}
	// observe では、従来どおり操作自体を受け付けない
	if _, err := g.ReportIncident(context.Background(), "x1", "tester", ""); err == nil {
		t.Fatal("部品が無いのに通報できた")
	}
}
