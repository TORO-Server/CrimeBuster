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
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

// Guard は、CoreProtect の変更を監視し、荒らしを見つけたら、証拠の画像を作って通報し、ロールバックする。
type Guard struct {
	cfg      *config.Config
	cp       Reader
	det      *detect.Detector
	cdet     *detect.ChatDetector
	ai       Judge // キーが無ければ nil
	store    *store.Store
	console  Console  // コンソール (observe では使わない。replay では nil)
	shooter  Shooter  // 画像 (observe では使わない)
	reporter Reporter // 通報 (live だけ)
	warps    []store.Warp
	log      *slog.Logger

	incidents *IncidentService
	chat      *ChatService
	retention *Retention

	mu         sync.Mutex
	suppressed map[int]int64 // user → この時刻まで、検知しても無視する (道具を使った人、信頼する人)
	queue      chan *store.Incident
	busy       map[string]string // 事件の ID → 裏で動いている操作 (g.mu を持って触る)

	opMu  sync.Mutex // 時間のかかる処理 (事件の処理、ロールバック、撮影) を 1 つずつにする。キックとミュートは、これを待たない
	recMu sync.Mutex // 事件の記録 (record.json) の読み書き

	lastRefresh         time.Time     // 最後に書き出しと地図の更新を頼んだ時刻 (g.opMu を持って触る)
	refreshEvery        time.Duration // 書き出しと地図の更新の間隔
	rollbackConfirmWait time.Duration // データベースでロールバックを確かめ直す間隔
}

const (
	// incidentDeadline は、1 件の処理の全体の期限。これを超えて g.opMu を持ち続けない。
	incidentDeadline = 30 * time.Minute
	// refreshInterval は、save-all flush と bluemap update の間隔。事件が続いても、サーバーへの負荷を増やさない。
	refreshInterval = time.Minute
	// rollbackConfirmTries は、ロールバックをデータベースで確かめる回数。
	rollbackConfirmTries = 7
	// healthEvery は、監視が回っている印を書く間隔。
	healthEvery = 30 * time.Second
)

func NewGuard(cfg *config.Config, cp *detect.CP, st *store.Store, ops *relay.Ops, sh *capture.Shooter, rep *notify.Reporter, log *slog.Logger) *Guard {
	return newGuard(cfg, st, log, ports{
		cp:       readerOrNil(cp),
		ai:       judgeOrNil(notify.NewAI(cfg)),
		console:  consoleOrNil(ops),
		shooter:  shooterOrNil(sh),
		reporter: reporterOrNil(rep),
	})
}

// ports は、Guard とそのサービスが使う依存の集まり (ports.go で定義した境界)。
type ports struct {
	cp       Reader
	ai       Judge
	console  Console
	shooter  Shooter
	reporter Reporter
}

func newGuard(cfg *config.Config, st *store.Store, log *slog.Logger, p ports) *Guard {
	g := &Guard{
		cfg: cfg, cp: p.cp, det: detect.NewDetector(cfg), cdet: detect.NewChatDetector(cfg), ai: p.ai, store: st,
		console: p.console, shooter: p.shooter, reporter: p.reporter,
		warps:      store.LoadWarps(cfg.StorageConfig.WarpsDir),
		log:        log,
		suppressed: map[int]int64{}, queue: make(chan *store.Incident, 16), busy: map[string]string{},
		refreshEvery: refreshInterval, rollbackConfirmWait: 5 * time.Second,
	}
	g.incidents = &IncidentService{g: g, cp: p.cp, ai: p.ai, console: p.console, shooter: p.shooter, reporter: p.reporter}
	g.chat = &ChatService{g: g, cp: p.cp, ai: p.ai, console: p.console, reporter: p.reporter}
	g.retention = &Retention{g: g}
	return g
}

// 具象型が nil のときは、ポート自体も nil にする。型付き nil を入れると、
// 「依存が無いので、その機能は使えない」という nil 判定が壊れるため。
func readerOrNil(cp *detect.CP) Reader {
	if cp == nil {
		return nil
	}
	return cp
}

func judgeOrNil(ai *notify.AI) Judge {
	if ai == nil {
		return nil
	}
	return ai
}

func reporterOrNil(r *notify.Reporter) Reporter {
	if r == nil {
		return nil
	}
	return r
}

func shooterOrNil(s *capture.Shooter) Shooter {
	if s == nil {
		return nil
	}
	return s
}

func consoleOrNil(ops *relay.Ops) Console {
	if ops == nil {
		return nil
	}
	return opsConsole{ops: ops}
}

// incidentID は事件の ID (フォルダの名前になる)。名前が ID に使えないときは、ユーザーの番号を使う (一覧から消えないように)。
func incidentID(prefix string, at int64, name string, userID int) string {
	base := prefix + time.Unix(at, 0).In(notify.JST).Format("20060102-150405") + "-"
	if id := base + name; domain.ValidName(name) && domain.ValidID(id) {
		return id
	}
	return fmt.Sprintf("%suid%d", base, userID)
}

// Run は監視の本体。ctx が終わるまで回る。
func (g *Guard) Run(ctx context.Context) error {
	if g.store.S.Cursor == 0 {
		n, err := g.cp.MaxRowID(ctx)
		if err != nil {
			return fmt.Errorf("max rowid: %w", err)
		}
		g.mu.Lock()
		g.store.S.Cursor = n
		_ = g.store.Save()
		g.mu.Unlock()
		g.log.Info("起動: 今ある最後の行から見る", "cursor", n)
	}
	if g.cfg.ChatConfig.ChatEnabled && g.store.S.ChatCur == 0 {
		n, err := g.cp.MaxChatRowID(ctx)
		if err != nil {
			return fmt.Errorf("max chat rowid: %w", err)
		}
		g.mu.Lock()
		g.store.S.ChatCur = n
		_ = g.store.Save()
		g.mu.Unlock()
	}
	// 前回の続きの事件は、またキューに入れる
	g.mu.Lock()
	for _, inc := range g.store.S.Incidents {
		inc.Queued = false
	}
	g.mu.Unlock()

	go g.worker(ctx)
	go g.retention.Loop(ctx)

	tick := time.NewTicker(g.cfg.DetectConfig.PollInterval)
	defer tick.Stop()
	var healthAt time.Time
	for {
		if err := g.step(ctx); err != nil {
			if ctx.Err() == nil {
				g.log.Error("監視の 1 回が失敗", "err", err)
			}
		} else if time.Since(healthAt) >= healthEvery {
			// healthcheck は、監視の 1 回が最後に成功した時刻で判定する
			TouchHealth(g.cfg)
			healthAt = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (g *Guard) step(ctx context.Context) error {
	for {
		events, err := g.cp.Poll(ctx, g.store.S.Cursor, 20000)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		g.mu.Lock()
		for _, e := range events {
			g.store.S.Cursor = e.RowID
			if now-e.Time > int64(g.cfg.DetectConfig.MaxEventAge.Seconds()) { // 古い行 (止まっていたあいだのもの) は、見ても報告しない
				continue
			}
			g.onEvent(ctx, e)
		}
		g.mu.Unlock()
		if len(events) < 20000 {
			break
		}
	}
	if g.cfg.ChatConfig.ChatEnabled {
		if err := g.chat.stepChat(ctx); err != nil {
			g.log.Error("チャットの監視が失敗", "err", err)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closeIdle(time.Now().Unix())
	return g.store.Save()
}

// onEvent は g.mu を持って呼ぶ。
func (g *Guard) onEvent(ctx context.Context, e detect.Event) {
	key := fmt.Sprint(e.User)
	if inc := g.store.S.Incidents[key]; inc != nil && e.Time > inc.Last {
		inc.Last = e.Time
	}
	t := g.det.Feed(e)
	if t == nil {
		return
	}
	until := g.suppressed[e.User]
	if e.Time < until || g.store.S.Incidents[key] != nil {
		return
	}
	g.consider(ctx, *t, e)
}

// consider は、閾値を超えた人を、事件にするか決める (g.mu を持って呼ぶ)。
func (g *Guard) consider(ctx context.Context, t detect.Trigger, e detect.Event) {
	u, err := g.cp.User(ctx, t.User)
	if err != nil {
		g.log.Error("ユーザーが引けない", "user", t.User, "err", err)
		return
	}
	suppress := func(why string, d time.Duration) {
		g.suppressed[t.User] = e.Time + int64(d.Seconds())
		g.log.Info("検知したが対象外", "player", u.Name, "why", why, "trigger", t.String())
	}
	if strings.HasPrefix(u.Name, "#") { // #tnt, #fire, #decay など (人ではない)
		suppress("not a player", time.Hour)
		return
	}
	if g.cfg.DetectConfig.TrustedPlayers[strings.ToLower(u.Name)] {
		suppress("trusted", time.Hour)
		return
	}
	if LoadOps(g.cfg.DetectConfig.OpsFile)[strings.ToLower(u.Name)] {
		suppress("op", time.Hour)
		return
	}
	cmd, err := g.cp.RecentEditCommand(ctx, t.User, e.Time-int64(g.cfg.DetectConfig.WEGrace.Seconds()))
	if err != nil {
		g.log.Error("コマンドの確認に失敗", "err", err)
	}
	trigger := t.String()
	if cmd != "" {
		// 文字列が一致しただけで、権限も成否も分からない。人が見る review では隠さず、事件に書いて確認待ちに置く
		if !config.CapabilitiesFor(g.cfg.Mode).KeepEditCommandUsers {
			suppress("edit command: "+cmd, 10*time.Minute)
			return
		}
		trigger += " / 編集コマンドの使用あり (権限と成否は未確認): " + domain.SafeChat(cmd, 60)
	}

	inc := &store.Incident{
		ID:     incidentID("", e.Time, u.Name, t.User),
		UserID: t.User, Name: u.Name, UUID: u.UUID,
		Start: e.Time - int64(t.Window), Last: e.Time, Trigger: trigger,
	}
	g.store.S.Incidents[fmt.Sprint(t.User)] = inc
	g.log.Warn("荒らしを検知", "player", u.Name, "uuid", u.UUID, "trigger", trigger, "incident", inc.ID, "mode", g.cfg.Mode)

	if config.CapabilitiesFor(g.cfg.Mode).AutoKick && g.cfg.ConsoleConfig.KickEnabled && g.console != nil {
		go g.kickDetected(ctx, u.Name) // g.mu を持ったままコンソールを待たない
	}
}

func (g *Guard) kickDetected(ctx context.Context, name string) {
	if err := g.console.Kick(ctx, name, "ブロックを不自然な速さで変えたため"); err != nil {
		g.log.Error("キックに失敗", "err", err)
		return
	}
	g.log.Warn("キックした", "player", name)
}

// closeIdle は、動きが止まった (または長すぎる) 事件を閉じて、報告のキューに入れる (g.mu を持って呼ぶ)。
func (g *Guard) closeIdle(now int64) {
	for _, inc := range g.store.S.Incidents {
		if inc.Queued {
			continue
		}
		quietFor := g.cfg.DetectConfig.Quiet
		if inc.Kind == "chat" {
			quietFor = g.cfg.ChatConfig.ChatQuiet
		}
		quiet := now-inc.Last >= int64(quietFor.Seconds())
		long := now-inc.Start >= int64(g.cfg.DetectConfig.MaxIncident.Seconds())
		if !quiet && !long {
			continue
		}
		select {
		case g.queue <- inc:
			inc.Queued = true
		default:
		}
	}
}

// worker は閉じた事件を 1 つずつ処理する (コンソールとブラウザを同時に使わないため)。
func (g *Guard) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case inc := <-g.queue:
			// 閉じる前に Reset (同じ人の続きは新しい事件として数え直す)
			g.mu.Lock()
			if inc.Kind == "chat" {
				g.cdet.Reset(inc.UserID)
			} else {
				g.det.Reset(inc.UserID)
			}
			delete(g.store.S.Incidents, inc.Key())
			_ = g.store.Save()
			g.mu.Unlock()
			g.handle(ctx, inc)
		}
	}
}

// handle は閉じた事件を 1 件処理する。コネクタからの操作とコンソール・ブラウザを取り合わないよう、g.opMu を持つ。
func (g *Guard) handle(ctx context.Context, inc *store.Incident) {
	g.opMu.Lock()
	defer g.opMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, incidentDeadline)
	defer cancel()
	if inc.Kind == "chat" {
		if res, err := g.ProcessChat(ctx, inc, time.Now().Unix()); err != nil {
			g.log.Error("チャットの事件の処理に失敗", "incident", inc.ID, "err", err)
		} else {
			g.log.Info("チャットの事件を処理した", "incident", inc.ID, "dir", res.Dir, "posted", res.ThreadID != "", "muted", res.Muted)
		}
		return
	}
	if res, err := g.Process(ctx, inc); err != nil {
		g.log.Error("事件の処理に失敗", "incident", inc.ID, "err", err)
	} else {
		g.log.Info("事件を処理した", "incident", inc.ID, "dir", res.Dir, "posted", res.ThreadID != "", "rolled_back", res.Out.RolledBack)
	}
}

// waitRefresh は、書き出しと地図の更新の間隔を空ける (g.opMu を持って呼ぶ)。
func (g *Guard) waitRefresh(ctx context.Context) error {
	if wait := time.Until(g.lastRefresh.Add(g.refreshEvery)); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	g.lastRefresh = time.Now()
	return nil
}

// blueMapArea は、描き直す範囲。中心はカメラ (壊した数の中央値) で、半径は MaxBlueMapRadius まで。
func blueMapArea(s *detect.Stats, cam capture.Cam) (cx, cz, radius int) {
	radius = max(max(s.X1-s.X0, s.Z1-s.Z0)/2+48, 64)
	return int(math.Round(cam.X)), int(math.Round(cam.Z)), min(radius, domain.MaxBlueMapRadius)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Config は、組み立てに使った設定を返す。
func (g *Guard) Config() *config.Config { return g.cfg }

// Logger は、組み立てに使ったロガーを返す。
func (g *Guard) Logger() *slog.Logger { return g.log }

// CP は、読み取りに使うデータベース接続を返す。ポートにフェイクが入っているときは nil
// (コネクタの、データベースへ直接つなぐ経路は、その構成では使えない)。
func (g *Guard) CP() *detect.CP {
	cp, _ := g.cp.(*detect.CP)
	return cp
}

// Reporter は、通報の部品を返す (無いときは nil)。
func (g *Guard) Reporter() *notify.Reporter {
	r, _ := g.reporter.(*notify.Reporter)
	return r
}

// Shooter は、撮影の部品を返す (無いときは nil)。
func (g *Guard) Shooter() *capture.Shooter {
	s, _ := g.shooter.(*capture.Shooter)
	return s
}

// Ops は、コンソール操作の部品を返す (無いときは nil)。
func (g *Guard) Ops() *relay.Ops {
	c, _ := g.console.(opsConsole)
	return c.ops
}

// AI は、判定の部品を返す (無いときは nil)。
func (g *Guard) AI() *notify.AI {
	ai, _ := g.ai.(*notify.AI)
	return ai
}

// ActiveIncidents は、いま開いている事件の数を返す。
func (g *Guard) ActiveIncidents() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.store.S.Incidents)
}

// WarpCount は、読み込んだワープの数を返す。
func (g *Guard) WarpCount() int { return len(g.warps) }

// LockOp は、事件への操作の直列化に使うロックを取る。
func (g *Guard) LockOp() { g.opMu.Lock() }

// UnlockOp は、LockOp で取ったロックを離す。
func (g *Guard) UnlockOp() { g.opMu.Unlock() }

// SetRollbackConfirmWait は、ロールバックの確認待ち時間を差し替える (試験用)。
func (g *Guard) SetRollbackConfirmWait(d time.Duration) { g.rollbackConfirmWait = d }

// SetShooter は、撮影の部品を差し替える (試験用)。
func (g *Guard) SetShooter(s *capture.Shooter) {
	g.shooter = shooterOrNil(s)
	if g.incidents != nil {
		g.incidents.shooter = g.shooter
	}
}
