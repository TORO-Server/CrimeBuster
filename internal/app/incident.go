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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Result は 1 件の事件の処理の結果。
type Result struct {
	Dir      string
	Stats    *detect.Stats
	Report   string
	Out      notify.Outcome
	ThreadID string
	Images   []notify.ReportFile
}

// Process は 1 件の事件を、mode に合わせて処理する。
//
//	observe: まとめを記録するだけ
//	dry-run: 画像と報告文を作ってフォルダに置くだけ (投稿もロールバックもしない)
//	live:    画像 → ロールバック → 復旧後の画像 → 通報
func (s *IncidentService) Process(ctx context.Context, inc *store.Incident) (*Result, error) {
	return s.ProcessWindow(ctx, inc, time.Now().Unix())
}

// processWindow は [inc.Start, end] の分を処理する (replay は、過去の end を渡す)。
func (s *IncidentService) ProcessWindow(ctx context.Context, inc *store.Incident, end int64) (*Result, error) {
	if !domain.ValidID(inc.ID) {
		return nil, fmt.Errorf("事件の ID %q はフォルダに使えません", inc.ID)
	}
	user := detect.UserInfo{ID: inc.UserID, Name: inc.Name, UUID: inc.UUID}
	now := time.Now().Unix()
	stats, err := s.cp.Stats(ctx, user, inc.Start-2, end)
	if err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	if stats.Empty() {
		return nil, fmt.Errorf("no block changes found for %s", inc.Name)
	}
	dir := filepath.Join(s.g.cfg.StorageConfig.DataDir, "reports", inc.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	res := &Result{Dir: dir, Stats: stats}
	mapID := s.g.cfg.ShotConfig.MapIDs[stats.World]

	// 1. 証拠の絵 (BlueMap が使えなくても作れる、真上からの図)
	heat := filepath.Join(dir, "heatmap.png")
	if err := capture.RenderHeatmap(stats, heat); err != nil {
		s.g.log.Error("真上の図が作れない", "err", err)
		heat = ""
	}

	if config.CapabilitiesFor(s.g.cfg.Mode).RecordOnly {
		s.g.log.Info("観察のみ", "incident", inc.ID, "breaks", stats.Breaks, "places", stats.Places, "peak_per_s", stats.PeakPerS)
		return res, writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "stats": detect.SummaryOf(stats)})
	}

	// 2. 被害の BlueMap の画面
	cam := capture.CamFor(stats)
	damage := s.bluemapShot(ctx, mapID, stats, cam, filepath.Join(dir, "damage.png"),
		capture.AnnotFor(stats, fmt.Sprintf("%s  BROKEN %s blocks", stats.User.Name, notify.Comma(stats.Breaks)),
			"red = broken columns / yellow = affected area", true), true)
	// 範囲が広いときは、いちばん壊された所に寄った画面も撮る (地図の更新は、全体の分で済んでいる)
	closeup := ""
	if cc, ok := capture.CloseCamFor(stats, cam); ok {
		an := capture.AnnotFor(stats, fmt.Sprintf("%s  CLOSE-UP (most broken spot)", stats.User.Name), "red = broken columns", true)
		an.NoBox = true
		closeup = s.bluemapShot(ctx, mapID, stats, cc, filepath.Join(dir, "closeup.png"), an, false)
	}

	// 3. AI の確認 → ロールバック (live だけ)
	out := notify.Outcome{}
	seconds := int(now-inc.Start) + 5
	out.Cmd = relay.RollbackCmd(inc.Name, seconds, stats.World)
	out.RestoreCmd = relay.RestoreCmd(inc.Name, seconds, stats.World)
	if s.ai != nil {
		v, err := s.ai.ReviewIncident(ctx, stats, inc.Trigger, []string{damage, closeup})
		if err != nil {
			s.g.log.Warn("AI の判定が取れない", "err", err)
			out.AIError = err.Error()
		} else {
			out.AI = v
			s.g.log.Info("AI の判定", "incident", inc.ID, "verdict", v.Verdict, "confidence", v.Confidence, "summary", v.Summary)
		}
	}
	// AI が「荒らしではない」「確信が低い」と答えたら、ロールバックしない (通報はして、運営が見る)。
	// AI が使えないときは、AI_REQUIRED=true でなければ、閾値の決まりだけで進める
	switch {
	case s.ai != nil && out.AI != nil && !out.AI.Allows(s.g.cfg.AIConfig.AIMinConf):
		out.Skipped = fmt.Sprintf("AI の判定が「%s」(確からしさ %d%%) で、基準 %d%% の「荒らし」に届かない", notify.VerdictJP(out.AI.Verdict), out.AI.Confidence, s.g.cfg.AIConfig.AIMinConf)
	case s.g.cfg.AIConfig.AIRequired && out.AI == nil:
		out.Skipped = "AI の判定が得られない (AI_REQUIRED=true)"
	case now-inc.Start > domain.MaxRollbackAge:
		out.Skipped = "事件から 24 時間より長くたっている"
	}
	restored := ""
	switch {
	case out.Skipped != "":
		// ロールバックしない
	case config.CapabilitiesFor(s.g.cfg.Mode).AutoRollback && s.console != nil:
		err := s.console.Rollback(ctx, inc.Name, seconds, stats.World)
		done, total, ok, derr := s.confirmRollback(ctx, err, inc.UserID, stats.World, inc.Start-2, end)
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
			restored = s.bluemapShot(ctx, mapID, stats, cam, filepath.Join(dir, "restored.png"),
				capture.AnnotFor(stats, fmt.Sprintf("%s  RESTORED (rollback done)", stats.User.Name), "yellow = affected area", false), true)
		}
	default:
		out.Planned = true
	}
	res.Out = out

	// 4. 報告文と画像
	var files []notify.ReportFile
	for _, c := range []struct{ name, path string }{{"damage.png", damage}, {"closeup.png", closeup}, {"restored.png", restored}} {
		if c.path != "" {
			files = append(files, notify.ReportFile{Name: c.name, Path: c.path})
		}
	}
	if len(files) < 2 && heat != "" {
		files = append(files, notify.ReportFile{Name: "heatmap.png", Path: heat})
	}
	res.Images = files
	res.Report = s.blockReport(stats, inc.Trigger, out, files, cam)
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(res.Report), 0o644); err != nil {
		return res, err
	}
	_ = writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "stats": detect.SummaryOf(stats), "outcome": out})
	rec := &domain.Record{
		ID: inc.ID, Kind: "block", Name: inc.Name, UUID: inc.UUID, UserID: inc.UserID,
		Created: now, Start: inc.Start, End: end, World: stats.World, Trigger: inc.Trigger,
		State: domain.StatePending, RolledBack: out.RolledBack,
		Breaks: stats.Breaks, Places: stats.Places, RollbackCmd: out.Cmd, RestoreCmd: out.RestoreCmd, AI: out.AI,
	}
	for _, f := range files {
		rec.Images = append(rec.Images, f.Name)
	}
	if out.RolledBack {
		rec.Decide(now, "crime-buster", "rollback", fmt.Sprintf("%d/%d", out.RolledDone, out.RolledTotal))
	}

	// 5. 通報 (live だけ)
	if config.CapabilitiesFor(s.g.cfg.Mode).AutoReport && s.reporter != nil {
		key := store.ThreadKey(inc.Name)
		existing := ""
		s.g.mu.Lock()
		if t, ok := s.g.store.S.Threads[key]; ok && now-t.At < 24*3600 {
			existing = t.ThreadID
		}
		s.g.mu.Unlock()
		id, err := s.reporter.Post(ctx, existing, domain.SafeChat(notify.ThreadTitle(stats), 90), res.Report, files, out.RolledBack)
		if err != nil {
			return res, fmt.Errorf("post: %w", err)
		}
		res.ThreadID = id
		s.g.mu.Lock()
		s.g.store.S.Threads[key] = store.ThreadRef{ThreadID: id, At: now}
		_ = s.g.store.Save()
		s.g.mu.Unlock()
		// 作った直後で状態は pending。遷移できなければ、記録を書かずに中断する
		if err := rec.Transition(domain.StateReported); err != nil {
			return res, err
		}
		rec.Thread = id
		rec.Decide(now, "crime-buster", "report", "")
	}
	if err := store.SaveRecord(dir, rec); err != nil {
		s.g.log.Warn("事件の記録が書けない", "incident", inc.ID, "err", err)
	}
	return res, nil
}

// blockReport はブロックの荒らしの報告文。ロールバックや通報のあとにも、同じ形で作り直す。
func (s *IncidentService) blockReport(stats *detect.Stats, trigger string, out notify.Outcome, files []notify.ReportFile, cam capture.Cam) string {
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
	if w, d, ok := store.Nearest(s.g.warps, stats.World, float64(stats.X0+stats.X1)/2, float64(stats.Z0+stats.Z1)/2); ok {
		warp = notify.WarpText(w, d)
	}
	return notify.BuildReport(notify.ReportInput{Stats: stats, Trigger: trigger, Warp: warp, Out: out, Images: desc, Mode: s.g.cfg.Mode, Center: &cam})
}

// bluemapShot は、書き出し → BlueMap の更新 → 待つ → 撮る。失敗しても事件の処理は続ける (空の文字列を返す)。
// refresh が false のときは、書き出しと更新を飛ばして、いまの地図で撮る (同じ事件の 2 枚目以降)。
func (s *IncidentService) bluemapShot(ctx context.Context, mapID string, stats *detect.Stats, cam capture.Cam, out string, ann *capture.Annot, refresh bool) string {
	if s.shooter == nil || mapID == "" {
		return ""
	}
	if s.console != nil && refresh {
		if err := s.g.waitRefresh(ctx); err != nil {
			s.g.log.Warn("書き出しの順番を待ちきれない", "err", err)
			return ""
		}
		cx, cz, radius := blueMapArea(stats, cam)
		if err := s.console.SaveFlush(ctx); err != nil {
			s.g.log.Warn("ワールドの書き出しに失敗", "err", err)
		}
		if err := s.console.BlueMapUpdate(ctx, mapID, cx, cz, radius); err != nil {
			s.g.log.Warn("BlueMap の更新を頼めない", "err", err)
		} else if err := s.console.WaitBlueMap(ctx, mapID, 6*time.Minute); err != nil {
			s.g.log.Warn("BlueMap の更新を待ちきれない (古い地図で撮る)", "err", err)
		}
	}
	if err := s.shooter.Shot(ctx, mapID, cam, out, ann); err != nil {
		s.g.log.Warn("BlueMap の画面が撮れない", "err", err)
		return ""
	}
	if ann != nil && !strings.HasPrefix(s.shooter.OverlayResult(), "ok:") {
		s.g.log.Warn("画面に印を重ねられなかった (印の無い画面を使う)", "result", s.shooter.OverlayResult())
	}
	return out
}

// ---- Guard からの委譲 (組み立て側とコマンドが使う入口) ----

// Process は、事件の処理を IncidentService へ委譲する。
func (g *Guard) Process(ctx context.Context, inc *store.Incident) (*Result, error) {
	return g.incidents.Process(ctx, inc)
}

// ProcessWindow は、事件の処理を IncidentService へ委譲する。
func (g *Guard) ProcessWindow(ctx context.Context, inc *store.Incident, end int64) (*Result, error) {
	return g.incidents.ProcessWindow(ctx, inc, end)
}
