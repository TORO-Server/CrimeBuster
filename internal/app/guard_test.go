package app

import (
	"context"
	"crime-buster/internal/capture"
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"crime-buster/internal/domain"
	"crime-buster/internal/notify"
	"crime-buster/internal/relay"
	"crime-buster/internal/store"
	"crime-buster/internal/testutil"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func TestLoadOps(t *testing.T) {
	p := t.TempDir() + "/ops.json"
	if err := testutil.WriteFile(p, `[{"uuid":"1","name":"test_player","level":4},{"uuid":"2","name":"Other","level":2}]`); err != nil {
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

func TestBlueMapAreaIsCapped(t *testing.T) {
	// 遠くに 1 個だけ壊すと範囲は広がるが、半径は上限で止まり、中心はカメラ (中央値) のまま
	s := &detect.Stats{X0: -30000, X1: 30000, Z0: 0, Z1: 10}
	for x := 95; x <= 105; x++ {
		s.Columns = append(s.Columns, detect.Column{X: x, Z: 5, N: 50})
	}
	s.Columns = append(s.Columns, detect.Column{X: -30000, Z: 0, N: 1}, detect.Column{X: 30000, Z: 10, N: 1})
	cx, cz, r := blueMapArea(s, capture.CamFor(s))
	if r != domain.MaxBlueMapRadius || cx < 95 || cx > 105 || cz != 5 {
		t.Fatalf("cx=%d cz=%d r=%d", cx, cz, r)
	}
	// 狭い範囲は、今までどおり
	small := &detect.Stats{X0: 0, X1: 100, Z0: 0, Z1: 20}
	if _, _, r := blueMapArea(small, capture.Cam{X: 50, Z: 10}); r != 98 {
		t.Fatalf("r=%d", r)
	}
	if _, _, r := blueMapArea(&detect.Stats{}, capture.Cam{}); r != 64 {
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
		if !domain.ValidID(id) || !strings.HasPrefix(id, c.prefix) {
			t.Errorf("%q: ID %q", c.name, id)
		}
		if got := strings.HasSuffix(id, "-uid42"); got == c.safe {
			t.Errorf("%q: ID %q", c.name, id)
		}
	}
}

func TestChatByUser(t *testing.T) {
	parts := chatByUser([]detect.ChatMsg{{RowID: 1, User: 5}, {RowID: 2, User: 7}, {RowID: 3, User: 5}})
	if len(parts) != 2 || len(parts[0]) != 2 || parts[0][1].RowID != 3 || parts[1][0].RowID != 2 {
		t.Fatalf("%+v", parts)
	}
}

func TestSweepData(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGuard(&config.Config{StorageConfig: config.StorageConfig{DataDir: dir}, RetentionConfig: config.RetentionConfig{ReportRetention: 24 * time.Hour}}, nil, st, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	put := func(id string, created time.Time) string {
		d := filepath.Join(dir, "reports", id)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveRecord(d, &domain.Record{ID: id, Created: created.Unix(), State: domain.StatePending}); err != nil {
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
	_ = testutil.WriteFile(oldShot, "x")
	_ = testutil.WriteFile(newShot, "x")
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
	g, _, db := newDBGuard(t, config.ModeObserve,
		testutil.FakeAnswer{Has: "MAX(rowid)", Rows: [][]driver.Value{{int64(1)}}},
		testutil.FakeAnswer{Has: "WHERE rowid > ?", Err: errors.New("データベースが止まっている")},
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
	waitFor("監視が回らない", func() bool { return len(db.Find("WHERE rowid > ?")) >= 3 })
	if err := CheckHealthFile(g.cfg.DataDir, time.Now()); err == nil {
		t.Fatal("失敗しているのに、印がある")
	}
	db.Set(testutil.FakeAnswer{Has: "WHERE rowid > ?"}) // 直った (新しい行は無い)
	waitFor("成功しても印が付かない", func() bool { return CheckHealthFile(g.cfg.DataDir, time.Now()) == nil })
}

func TestCheckHealthFile(t *testing.T) {
	dir := t.TempDir()
	if err := CheckHealthFile(dir, time.Now()); err == nil {
		t.Fatal("ファイルが無ければ、エラー")
	}
	TouchHealth(&config.Config{StorageConfig: config.StorageConfig{DataDir: dir}})
	if err := CheckHealthFile(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := CheckHealthFile(dir, time.Now().Add(3*time.Minute)); err == nil {
		t.Fatal("古ければ、エラー")
	}
}

// newDBGuard は、偽のデータベースと偽のコンソールを持つ Guard。
func newDBGuard(t *testing.T, mode config.Mode, answers ...testutil.FakeAnswer) (*Guard, *testutil.FakeConsole, *testutil.FakeDB) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sqldb, db := testutil.NewFakeDB(t, answers...)
	cp := detect.NewCP(sqldb, "coreprotect_")
	con := &testutil.FakeConsole{}
	cfg := &config.Config{Mode: mode, StorageConfig: config.StorageConfig{DataDir: dir}, DetectConfig: config.DetectConfig{WEGrace: 30 * time.Minute, TrustedPlayers: map[string]bool{}}, ChatConfig: config.ChatConfig{ChatMuteCmd: "mute {name} 10m spam"}, ShotConfig: config.ShotConfig{MapIDs: map[string]string{}}}
	g := NewGuard(cfg, cp, st, &relay.Ops{C: con}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.rollbackConfirmWait = time.Millisecond
	return g, con, db
}

func userRow(name string) testutil.FakeAnswer {
	return testutil.FakeAnswer{Has: "FROM coreprotect_user WHERE rowid", Rows: [][]driver.Value{{name, "uuid-1"}}}
}

func TestConsiderEditCommand(t *testing.T) {
	edit := testutil.FakeAnswer{Has: "message LIKE", Rows: [][]driver.Value{{"//set `@everyone"}}}
	ev := detect.Event{User: 7, Time: 1793757149}
	tr := detect.Trigger{User: 7, Time: ev.Time, Kind: "burst-break", Breaks: 120, Window: 5}

	// review: 抑止せず、事件に注記して確認待ちに置く
	g, _, _ := newDBGuard(t, config.ModeReview, userRow("griefer"), edit)
	g.consider(context.Background(), tr, ev)
	inc := g.store.S.Incidents["7"]
	if inc == nil || !strings.Contains(inc.Trigger, "編集コマンドの使用あり") || strings.Contains(inc.Trigger, "@everyone") || strings.Contains(inc.Trigger, "`") {
		t.Fatalf("incident: %+v", inc)
	}

	// それ以外の動かし方は、今までどおり対象外
	for _, mode := range []config.Mode{config.ModeObserve, config.ModeDryRun, config.ModeLive} {
		g, _, _ := newDBGuard(t, mode, userRow("griefer"), edit)
		g.consider(context.Background(), tr, ev)
		if len(g.store.S.Incidents) != 0 || g.suppressed[7] == 0 {
			t.Errorf("%s: %+v", mode, g.store.S.Incidents)
		}
	}

	// コマンドが無ければ、注記は付かない
	g, _, _ = newDBGuard(t, config.ModeReview, userRow("griefer"))
	g.consider(context.Background(), tr, ev)
	if inc := g.store.S.Incidents["7"]; inc == nil || strings.Contains(inc.Trigger, "編集コマンド") {
		t.Fatalf("incident: %+v", inc)
	}
}

func TestConsiderUnsafeNameUsesSafeID(t *testing.T) {
	g, _, _ := newDBGuard(t, config.ModeReview, userRow("../../etc"))
	g.consider(context.Background(), detect.Trigger{User: 7, Kind: "burst-break"}, detect.Event{User: 7, Time: 1793757149})
	inc := g.store.S.Incidents["7"]
	if inc == nil || !domain.ValidID(inc.ID) || !strings.HasSuffix(inc.ID, "-uid7") {
		t.Fatalf("incident: %+v", inc)
	}
}

func TestLiveKickDoesNotBlockDetection(t *testing.T) {
	g, con, _ := newDBGuard(t, config.ModeLive, userRow("griefer"))
	g.cfg.KickEnabled = true
	g.mu.Lock() // onEvent と同じに g.mu を持って呼ぶ
	g.consider(context.Background(), detect.Trigger{User: 7, Kind: "burst-break"}, detect.Event{User: 7, Time: 1793757149})
	g.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for len(con.Sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s := con.Sent(); len(s) != 1 || !strings.HasPrefix(s[0], "kick griefer ") {
		t.Fatalf("sent %q", s)
	}
}

// AI の判定だけの事件は、live でもミュートしない。決まりの検知が重なれば、ミュートする。
func TestAIOnlyChatIsNotMuted(t *testing.T) {
	g, con, _ := newDBGuard(t, config.ModeLive, userRow("talker"), testutil.ChatRows(9))
	ctx := context.Background()
	g.onChat(ctx, detect.ChatMsg{RowID: 1, Time: 1793757149, User: 9, Msg: "a"}, &notify.ChatFlag{Category: "hate", Confidence: 90, Summary: "s"})
	inc := g.store.S.Incidents[store.ChatKey(9)]
	if inc == nil || !inc.AIOnly {
		t.Fatalf("incident: %+v", inc)
	}
	res, err := g.ProcessChat(ctx, inc, 1793757160)
	if err != nil {
		t.Fatal(err)
	}
	if res.Muted || len(con.Sent()) != 0 || strings.Contains(res.Report, "mute") {
		t.Fatalf("muted=%v sent=%q report:\n%s", res.Muted, con.Sent(), res.Report)
	}
	if !strings.Contains(res.Report, "一つ目") || strings.Index(res.Report, "一つ目") > strings.Index(res.Report, "二つ目") {
		t.Fatalf("発言は時間の順:\n%s", res.Report)
	}

	// 決まりの検知が重なると、AI だけの事件ではなくなる
	g.cfg.ChatRun = 3
	g.onChat(ctx, detect.ChatMsg{RowID: 2, Time: 1793757150, User: 9, Msg: "aaaa"}, nil)
	if inc.AIOnly {
		t.Fatal("AIOnly のまま")
	}
	res, err = g.ProcessChat(ctx, inc, 1793757160)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Muted || len(con.Sent()) != 1 || con.Sent()[0] != "mute talker 10m spam" {
		t.Fatalf("muted=%v sent=%q", res.Muted, con.Sent())
	}
}

func TestProcessRefusesUnsafeIncidentID(t *testing.T) {
	g, _, _ := newDBGuard(t, config.ModeReview)
	inc := &store.Incident{ID: "../../x", Name: "x"}
	if _, err := g.ProcessWindow(context.Background(), inc, 1); err == nil {
		t.Fatal("ブロックの事件")
	}
	if _, err := g.ProcessChat(context.Background(), inc, 1); err == nil {
		t.Fatal("チャットの事件")
	}
}
