package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Guard は、CoreProtect の変更を監視し、荒らしを見つけたら、証拠の画像を作って通報し、ロールバックする。
type Guard struct {
	cfg      *Config
	cp       *CP
	det      *Detector
	cdet     *ChatDetector
	ai       *AI // キーが無ければ nil
	store    *Store
	ops      *Ops      // コンソール (observe では使わない。replay では nil)
	shooter  *Shooter  // 画像 (observe では使わない)
	reporter *Reporter // live だけ
	warps    []Warp
	log      *slog.Logger

	mu         sync.Mutex
	suppressed map[int]int64 // user → この時刻まで、検知しても無視する (道具を使った人、信頼する人)
	queue      chan *Incident
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
	// maxBlueMapRadius は、1 回に描き直す半径の上限。範囲はプレイヤーの行動で決まるので、広がりすぎないようにする。
	maxBlueMapRadius = 256
	// healthEvery は、監視が回っている印を書く間隔。
	healthEvery = 30 * time.Second
)

func NewGuard(cfg *Config, cp *CP, st *Store, ops *Ops, sh *Shooter, rep *Reporter, log *slog.Logger) *Guard {
	return &Guard{
		cfg: cfg, cp: cp, det: NewDetector(cfg), cdet: NewChatDetector(cfg), ai: NewAI(cfg), store: st, ops: ops, shooter: sh, reporter: rep,
		warps: LoadWarps(cfg.WarpsDir), log: log,
		suppressed: map[int]int64{}, queue: make(chan *Incident, 16), busy: map[string]string{},
		refreshEvery: refreshInterval, rollbackConfirmWait: 5 * time.Second,
	}
}

// incidentID は事件の ID (フォルダの名前になる)。名前が ID に使えないときは、ユーザーの番号を使う (一覧から消えないように)。
func incidentID(prefix string, at int64, name string, userID int) string {
	base := prefix + time.Unix(at, 0).In(jst).Format("20060102-150405") + "-"
	if id := base + name; validName(name) && validID(id) {
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
	if g.cfg.ChatEnabled && g.store.S.ChatCur == 0 {
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
	go g.sweepLoop(ctx)

	tick := time.NewTicker(g.cfg.PollInterval)
	defer tick.Stop()
	var healthAt time.Time
	for {
		if err := g.step(ctx); err != nil {
			if ctx.Err() == nil {
				g.log.Error("監視の 1 回が失敗", "err", err)
			}
		} else if time.Since(healthAt) >= healthEvery {
			// healthcheck は、監視の 1 回が最後に成功した時刻で判定する
			touchHealth(g.cfg)
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
			if now-e.Time > int64(g.cfg.MaxEventAge.Seconds()) { // 古い行 (止まっていたあいだのもの) は、見ても報告しない
				continue
			}
			g.onEvent(ctx, e)
		}
		g.mu.Unlock()
		if len(events) < 20000 {
			break
		}
	}
	if g.cfg.ChatEnabled {
		if err := g.stepChat(ctx); err != nil {
			g.log.Error("チャットの監視が失敗", "err", err)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closeIdle(time.Now().Unix())
	return g.store.Save()
}

// stepChat は新しい発言を読んで、スパムを事件にする。
func (g *Guard) stepChat(ctx context.Context) error {
	for {
		msgs, err := g.cp.PollChat(ctx, g.store.S.ChatCur, 5000)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		flags := g.aiChat(ctx, msgs, now) // 通信があるので、ロックの外で
		g.mu.Lock()
		for _, m := range msgs {
			g.store.S.ChatCur = m.RowID
			if now-m.Time > int64(g.cfg.MaxEventAge.Seconds()) {
				continue
			}
			g.onChat(ctx, m, flags[m.RowID])
		}
		g.mu.Unlock()
		if len(msgs) < 5000 {
			return nil
		}
	}
}

// maxAIChatBatch は、AI に 1 度に見せる発言の数の上限。
const maxAIChatBatch = 40

// aiChat は、新しい発言を AI に見せて、対応すべき発言 (rowid → 判定) を返す。AI を使わない・失敗したときは空。
// 発言が多い (起動直後に溜まっていたときなど) ときは、新しいほうから maxAIChatBatch 件だけ見せる。
func (g *Guard) aiChat(ctx context.Context, msgs []ChatMsg, now int64) map[int64]*ChatFlag {
	if g.ai == nil || !g.cfg.AIChat {
		return nil
	}
	var fresh []ChatMsg
	for _, m := range msgs {
		if now-m.Time <= int64(g.cfg.MaxEventAge.Seconds()) && strings.TrimSpace(m.Msg) != "" {
			fresh = append(fresh, m)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	if len(fresh) > maxAIChatBatch {
		fresh = fresh[len(fresh)-maxAIChatBatch:]
	}
	names := map[int]string{}
	for _, m := range fresh {
		if _, ok := names[m.User]; !ok {
			if u, err := g.cp.User(ctx, m.User); err == nil {
				names[m.User] = u.Name
			}
		}
	}
	// 1 人ずつ尋ねる (ほかの人の発言の文字が、その人の判定に影響しないように)
	out := map[int64]*ChatFlag{}
	for _, part := range chatByUser(fresh) {
		list, err := g.ai.ReviewChat(ctx, part, names)
		if err != nil {
			g.log.Warn("AI のチャットの判定が取れない (決まりだけで見張る)", "err", err)
			continue
		}
		for i := range list {
			f := list[i]
			if f.Confidence < g.cfg.AIMinConf {
				continue
			}
			out[part[f.ID].RowID] = &f
		}
	}
	return out
}

// chatByUser は、発言を人ごとに分ける。人の順は最初の発言の順、発言の順はそのまま。
func chatByUser(msgs []ChatMsg) [][]ChatMsg {
	index := map[int]int{}
	var out [][]ChatMsg
	for _, m := range msgs {
		i, ok := index[m.User]
		if !ok {
			i = len(out)
			index[m.User] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], m)
	}
	return out
}

// onChat は g.mu を持って呼ぶ。flag は、AI がこの発言を対応すべきと判定したとき (無ければ nil)。
func (g *Guard) onChat(ctx context.Context, m ChatMsg, flag *ChatFlag) {
	key := chatKey(m.User)
	open := g.store.S.Incidents[key]
	if open != nil && m.Time > open.Last {
		open.Last = m.Time
	}
	t := g.cdet.Feed(m)
	// 決まりの検知が重なったら、AI の判定だけの事件ではなくなる
	if t != nil && open != nil && open.AIOnly {
		open.AIOnly, open.Trigger = false, t.String()
	}
	if t == nil && flag != nil {
		t = &ChatTrigger{User: m.User, Time: m.Time, Kind: "ai", Detail: fmt.Sprintf("%s (確からしさ %d%%) %s", flag.Category, flag.Confidence, flag.Summary)}
	}
	if t == nil || open != nil || m.Time < g.suppressed[-m.User] {
		return
	}
	u, err := g.cp.User(ctx, m.User)
	if err != nil {
		g.log.Error("ユーザーが引けない", "user", m.User, "err", err)
		return
	}
	lname := strings.ToLower(u.Name)
	if g.cfg.TrustedPlayers[lname] || LoadOps(g.cfg.OpsFile)[lname] {
		g.suppressed[-m.User] = m.Time + 3600 // チャットは、ブロックと別に数える (鍵を負の数にする)
		g.log.Info("チャットのスパムを検知したが対象外", "player", u.Name, "trigger", t.String())
		return
	}
	inc := &Incident{
		ID:     incidentID("chat-", m.Time, u.Name, m.User),
		UserID: m.User, Name: u.Name, UUID: u.UUID, Kind: "chat", AIOnly: t.Kind == "ai",
		Start: m.Time, Last: m.Time, Trigger: t.String(),
	}
	g.store.S.Incidents[key] = inc
	g.log.Warn("チャットのスパムを検知", "player", u.Name, "trigger", t.String(), "incident", inc.ID, "mode", g.cfg.Mode)
}

// onEvent は g.mu を持って呼ぶ。
func (g *Guard) onEvent(ctx context.Context, e Event) {
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
func (g *Guard) consider(ctx context.Context, t Trigger, e Event) {
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
	if g.cfg.TrustedPlayers[strings.ToLower(u.Name)] {
		suppress("trusted", time.Hour)
		return
	}
	if LoadOps(g.cfg.OpsFile)[strings.ToLower(u.Name)] {
		suppress("op", time.Hour)
		return
	}
	cmd, err := g.cp.RecentEditCommand(ctx, t.User, e.Time-int64(g.cfg.WEGrace.Seconds()))
	if err != nil {
		g.log.Error("コマンドの確認に失敗", "err", err)
	}
	trigger := t.String()
	if cmd != "" {
		// 文字列が一致しただけで、権限も成否も分からない。人が見る review では隠さず、事件に書いて確認待ちに置く
		if !CapabilitiesFor(g.cfg.Mode).KeepEditCommandUsers {
			suppress("edit command: "+cmd, 10*time.Minute)
			return
		}
		trigger += " / 編集コマンドの使用あり (権限と成否は未確認): " + safeChat(cmd, 60)
	}

	inc := &Incident{
		ID:     incidentID("", e.Time, u.Name, t.User),
		UserID: t.User, Name: u.Name, UUID: u.UUID,
		Start: e.Time - int64(t.Window), Last: e.Time, Trigger: trigger,
	}
	g.store.S.Incidents[fmt.Sprint(t.User)] = inc
	g.log.Warn("荒らしを検知", "player", u.Name, "uuid", u.UUID, "trigger", trigger, "incident", inc.ID, "mode", g.cfg.Mode)

	if CapabilitiesFor(g.cfg.Mode).AutoKick && g.cfg.KickEnabled && g.ops != nil {
		go g.kickDetected(ctx, u.Name) // g.mu を持ったままコンソールを待たない
	}
}

func (g *Guard) kickDetected(ctx context.Context, name string) {
	if err := g.ops.Kick(ctx, name, "ブロックを不自然な速さで変えたため"); err != nil {
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
		quietFor := g.cfg.Quiet
		if inc.Kind == "chat" {
			quietFor = g.cfg.ChatQuiet
		}
		quiet := now-inc.Last >= int64(quietFor.Seconds())
		long := now-inc.Start >= int64(g.cfg.MaxIncident.Seconds())
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
func (g *Guard) handle(ctx context.Context, inc *Incident) {
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

// Result は 1 件の事件の処理の結果。
type Result struct {
	Dir      string
	Stats    *Stats
	Report   string
	Out      Outcome
	ThreadID string
	Images   []ReportFile
}

// Process は 1 件の事件を、mode に合わせて処理する。
//
//	observe: まとめを記録するだけ
//	dry-run: 画像と報告文を作ってフォルダに置くだけ (投稿もロールバックもしない)
//	live:    画像 → ロールバック → 復旧後の画像 → 通報
func (g *Guard) Process(ctx context.Context, inc *Incident) (*Result, error) {
	return g.processWindow(ctx, inc, time.Now().Unix())
}

// processWindow は [inc.Start, end] の分を処理する (replay は、過去の end を渡す)。
func (g *Guard) processWindow(ctx context.Context, inc *Incident, end int64) (*Result, error) {
	if !validID(inc.ID) {
		return nil, fmt.Errorf("事件の ID %q はフォルダに使えません", inc.ID)
	}
	user := UserInfo{ID: inc.UserID, Name: inc.Name, UUID: inc.UUID}
	now := time.Now().Unix()
	stats, err := g.cp.Stats(ctx, user, inc.Start-2, end)
	if err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	if stats.Empty() {
		return nil, fmt.Errorf("no block changes found for %s", inc.Name)
	}
	dir := filepath.Join(g.cfg.DataDir, "reports", inc.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	res := &Result{Dir: dir, Stats: stats}
	mapID := g.cfg.MapIDs[stats.World]

	// 1. 証拠の絵 (BlueMap が使えなくても作れる、真上からの図)
	heat := filepath.Join(dir, "heatmap.png")
	if err := RenderHeatmap(stats, heat); err != nil {
		g.log.Error("真上の図が作れない", "err", err)
		heat = ""
	}

	if CapabilitiesFor(g.cfg.Mode).RecordOnly {
		g.log.Info("観察のみ", "incident", inc.ID, "breaks", stats.Breaks, "places", stats.Places, "peak_per_s", stats.PeakPerS)
		return res, writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "stats": summaryOf(stats)})
	}

	// 2. 被害の BlueMap の画面
	cam := CamFor(stats)
	damage := g.bluemapShot(ctx, mapID, stats, cam, filepath.Join(dir, "damage.png"),
		AnnotFor(stats, fmt.Sprintf("%s  BROKEN %s blocks", stats.User.Name, comma(stats.Breaks)),
			"red = broken columns / yellow = affected area", true), true)
	// 範囲が広いときは、いちばん壊された所に寄った画面も撮る (地図の更新は、全体の分で済んでいる)
	closeup := ""
	if cc, ok := CloseCamFor(stats, cam); ok {
		an := AnnotFor(stats, fmt.Sprintf("%s  CLOSE-UP (most broken spot)", stats.User.Name), "red = broken columns", true)
		an.NoBox = true
		closeup = g.bluemapShot(ctx, mapID, stats, cc, filepath.Join(dir, "closeup.png"), an, false)
	}

	// 3. AI の確認 → ロールバック (live だけ)
	out := Outcome{}
	seconds := int(now-inc.Start) + 5
	out.Cmd = RollbackCmd(inc.Name, seconds, stats.World)
	out.RestoreCmd = RestoreCmd(inc.Name, seconds, stats.World)
	if g.ai != nil {
		v, err := g.ai.ReviewIncident(ctx, stats, inc.Trigger, []string{damage, closeup})
		if err != nil {
			g.log.Warn("AI の判定が取れない", "err", err)
			out.AIError = err.Error()
		} else {
			out.AI = v
			g.log.Info("AI の判定", "incident", inc.ID, "verdict", v.Verdict, "confidence", v.Confidence, "summary", v.Summary)
		}
	}
	// AI が「荒らしではない」「確信が低い」と答えたら、ロールバックしない (通報はして、運営が見る)。
	// AI が使えないときは、AI_REQUIRED=true でなければ、閾値の決まりだけで進める
	switch {
	case g.ai != nil && out.AI != nil && !out.AI.Allows(g.cfg.AIMinConf):
		out.Skipped = fmt.Sprintf("AI の判定が「%s」(確からしさ %d%%) で、基準 %d%% の「荒らし」に届かない", verdictJP(out.AI.Verdict), out.AI.Confidence, g.cfg.AIMinConf)
	case g.cfg.AIRequired && out.AI == nil:
		out.Skipped = "AI の判定が得られない (AI_REQUIRED=true)"
	case now-inc.Start > maxRollbackAge:
		out.Skipped = "事件から 24 時間より長くたっている"
	}
	restored := ""
	switch {
	case out.Skipped != "":
		// ロールバックしない
	case CapabilitiesFor(g.cfg.Mode).AutoRollback && g.ops != nil:
		err := g.ops.Rollback(ctx, inc.Name, seconds, stats.World)
		done, total, ok, derr := g.confirmRollback(ctx, err, inc.UserID, stats.World, inc.Start-2, end)
		if derr == nil {
			out.RolledDone, out.RolledTotal = done, total
		}
		// 判定の基準はデータベース。コンソールの返事が取れなくても、ほぼ全部が戻っていれば成功とする
		if ok {
			out.RolledBack = true
		} else {
			out.Error = rollbackError(err, done, total).Error()
		}
		if out.RolledBack {
			restored = g.bluemapShot(ctx, mapID, stats, cam, filepath.Join(dir, "restored.png"),
				AnnotFor(stats, fmt.Sprintf("%s  RESTORED (rollback done)", stats.User.Name), "yellow = affected area", false), true)
		}
	default:
		out.Planned = true
	}
	res.Out = out

	// 4. 報告文と画像
	var files []ReportFile
	for _, c := range []struct{ name, path string }{{"damage.png", damage}, {"closeup.png", closeup}, {"restored.png", restored}} {
		if c.path != "" {
			files = append(files, ReportFile{Name: c.name, Path: c.path})
		}
	}
	if len(files) < 2 && heat != "" {
		files = append(files, ReportFile{Name: "heatmap.png", Path: heat})
	}
	res.Images = files
	res.Report = g.blockReport(stats, inc.Trigger, out, files, cam)
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(res.Report), 0o644); err != nil {
		return res, err
	}
	_ = writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "stats": summaryOf(stats), "outcome": out})
	rec := &Record{
		ID: inc.ID, Kind: "block", Name: inc.Name, UUID: inc.UUID, UserID: inc.UserID,
		Created: now, Start: inc.Start, End: end, World: stats.World, Trigger: inc.Trigger,
		State: StatePending, RolledBack: out.RolledBack,
		Breaks: stats.Breaks, Places: stats.Places, RollbackCmd: out.Cmd, RestoreCmd: out.RestoreCmd, AI: out.AI,
	}
	for _, f := range files {
		rec.Images = append(rec.Images, f.Name)
	}
	if out.RolledBack {
		rec.decide(now, "crime-buster", "rollback", fmt.Sprintf("%d/%d", out.RolledDone, out.RolledTotal))
	}

	// 5. 通報 (live だけ)
	if CapabilitiesFor(g.cfg.Mode).AutoReport && g.reporter != nil {
		key := threadKey(inc.Name)
		existing := ""
		g.mu.Lock()
		if t, ok := g.store.S.Threads[key]; ok && now-t.At < 24*3600 {
			existing = t.ThreadID
		}
		g.mu.Unlock()
		id, err := g.reporter.Post(ctx, existing, safeChat(ThreadTitle(stats), 90), res.Report, files, out.RolledBack)
		if err != nil {
			return res, fmt.Errorf("post: %w", err)
		}
		res.ThreadID = id
		g.mu.Lock()
		g.store.S.Threads[key] = ThreadRef{ThreadID: id, At: now}
		_ = g.store.Save()
		g.mu.Unlock()
		rec.State, rec.Reported, rec.Thread = StateReported, true, id
		rec.decide(now, "crime-buster", "report", "")
	}
	if err := saveRecord(dir, rec); err != nil {
		g.log.Warn("事件の記録が書けない", "incident", inc.ID, "err", err)
	}
	return res, nil
}

// blockReport はブロックの荒らしの報告文。ロールバックや通報のあとにも、同じ形で作り直す。
func (g *Guard) blockReport(stats *Stats, trigger string, out Outcome, files []ReportFile, cam Cam) string {
	what := map[string]string{
		"damage.png":   "被害の全体 (赤い線 = 壊された列)",
		"closeup.png":  "いちばん壊された所",
		"restored.png": "ロールバック後",
		"heatmap.png":  "壊した場所 (真上から)",
	}
	var desc []string
	for i, f := range files {
		desc = append(desc, fmt.Sprintf("%d枚目 %s", i+1, what[f.Name]))
	}
	warp := ""
	if w, d, ok := Nearest(g.warps, stats.World, float64(stats.X0+stats.X1)/2, float64(stats.Z0+stats.Z1)/2); ok {
		warp = WarpText(w, d)
	}
	return BuildReport(ReportInput{Stats: stats, Trigger: trigger, Warp: warp, Out: out, Images: desc, Mode: g.cfg.Mode, Center: &cam})
}

// bluemapShot は、書き出し → BlueMap の更新 → 待つ → 撮る。失敗しても事件の処理は続ける (空の文字列を返す)。
// refresh が false のときは、書き出しと更新を飛ばして、いまの地図で撮る (同じ事件の 2 枚目以降)。
func (g *Guard) bluemapShot(ctx context.Context, mapID string, s *Stats, cam Cam, out string, ann *Annot, refresh bool) string {
	if g.shooter == nil || mapID == "" {
		return ""
	}
	if g.ops != nil && refresh {
		if err := g.waitRefresh(ctx); err != nil {
			g.log.Warn("書き出しの順番を待ちきれない", "err", err)
			return ""
		}
		cx, cz, radius := blueMapArea(s, cam)
		if err := g.ops.SaveFlush(ctx); err != nil {
			g.log.Warn("ワールドの書き出しに失敗", "err", err)
		}
		if err := g.ops.BlueMapUpdate(ctx, mapID, cx, cz, radius); err != nil {
			g.log.Warn("BlueMap の更新を頼めない", "err", err)
		} else if err := g.ops.WaitBlueMap(ctx, mapID, 6*time.Minute); err != nil {
			g.log.Warn("BlueMap の更新を待ちきれない (古い地図で撮る)", "err", err)
		}
	}
	if err := g.shooter.Shot(ctx, mapID, cam, out, ann); err != nil {
		g.log.Warn("BlueMap の画面が撮れない", "err", err)
		return ""
	}
	if ann != nil && !strings.HasPrefix(g.shooter.LastOverlay, "ok:") {
		g.log.Warn("画面に印を重ねられなかった (印の無い画面を使う)", "result", g.shooter.LastOverlay)
	}
	return out
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

// blueMapArea は、描き直す範囲。中心はカメラ (壊した数の中央値) で、半径は maxBlueMapRadius まで。
func blueMapArea(s *Stats, cam Cam) (cx, cz, radius int) {
	radius = maxInt(maxInt(s.X1-s.X0, s.Z1-s.Z0)/2+48, 64)
	return int(math.Round(cam.X)), int(math.Round(cam.Z)), minInt(radius, maxBlueMapRadius)
}

func summaryOf(s *Stats) map[string]any {
	commands := make([]string, 0, len(s.Commands))
	for _, c := range s.Commands {
		commands = append(commands, safeChat(c, 120)) // プレイヤーが書いた文字。短くして、1 行にする
	}
	return map[string]any{
		"player": s.User.Name, "uuid": s.User.UUID, "world": s.World,
		"from": s.From, "to": s.To, "breaks": s.Breaks, "places": s.Places, "clicks": s.Clicks,
		"peak_per_s": s.PeakPerS, "peak_at": s.PeakAt,
		"x": []int{s.X0, s.X1}, "y": []int{s.Y0, s.Y1}, "z": []int{s.Z0, s.Z1},
		"top_breaks": s.TopBreaks, "commands": commands, "containers": s.Containers,
	}
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
