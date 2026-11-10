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

// コネクタから頼まれる操作。人が (または AI が) 確かめて決めたことを、ここで実行する。
// どれも、動かし方が review か live のときだけ受ける。by は、頼んだ人 (記録に残す)。
// reportCmdMargin は、通報に書くコマンドの t: に足す秒数。運営がこの時間のうちに実行すれば、荒らしの始まりまで届く。
const reportCmdMargin = 3600

// IncidentService は、事件への操作 (通報、却下、ロールバック、ミュート、キック) と、
// 閉じた事件の処理のパイプライン (incident.go) を担う。
// 共有する状態 (記録、ロック、設定) は Guard が持ち、依存はポートで受ける。
type IncidentService struct {
	g        *Guard
	cp       Reader
	ai       Judge
	console  Console
	shooter  Shooter
	reporter Reporter
}

func (s *IncidentService) CheckMode() error {
	if !config.CapabilitiesFor(s.g.cfg.Mode).AcceptOps {
		return fmt.Errorf("%w: CRIMEBUSTER_MODE=%s のあいだは、操作を受け付けません (review か live にしてください)", domain.ErrForbidden, s.g.cfg.Mode)
	}
	return nil
}

// protected は、荒らしとして扱ってはいけない人 (信頼する人・op)。
func (g *Guard) Protected(name string) bool {
	l := strings.ToLower(name)
	return g.cfg.DetectConfig.TrustedPlayers[l] || LoadOps(g.cfg.DetectConfig.OpsFile)[l]
}

// claim は、事件に「いま動いている操作」を付ける。別の操作が動いていれば ErrConflict。
// 通報・ロールバック・却下が、同じ事件で重ならないようにする。
func (g *Guard) Claim(id, op string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if b := g.busy[id]; b != "" {
		return fmt.Errorf("%w: いま %s を実行中です。終わってからやり直してください", domain.ErrConflict, b)
	}
	g.busy[id] = op
	return nil
}

func (g *Guard) Release(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.busy, id)
}

// Dismiss は、荒らしではないと判断した事件を閉じる。
func (s *IncidentService) Dismiss(id, by, reason string) (*domain.Record, error) {
	if err := s.g.Claim(id, "dismiss"); err != nil {
		return nil, err
	}
	defer s.g.Release(id)
	return s.g.update(id, func(r *domain.Record) error {
		if r.State == domain.StatePending {
			// pending であることを確かめた直後なので、この遷移は失敗しない
			if err := r.Transition(domain.StateDismissed); err != nil {
				return err
			}
			r.Decide(time.Now().Unix(), by, "dismiss", domain.CleanText(reason, 300))
		}
		return nil
	})
}

// ReportIncident は、事件をフォーラムに通報する。note があれば、本文の頭に足す。
// ブロックの荒らしは、いまのロールバックの状態で、報告文を作り直す。
func (s *IncidentService) ReportIncident(ctx context.Context, id, by, note string) (*domain.Record, error) {
	if err := s.CheckMode(); err != nil {
		return nil, err
	}
	if s.reporter == nil {
		return nil, fmt.Errorf("%w: Discord に投稿する設定 (DiscordSRV のトークンと Discord-MCP) がありません", domain.ErrUnavailable)
	}
	s.g.opMu.Lock()
	defer s.g.opMu.Unlock()
	if err := s.g.Claim(id, "report"); err != nil {
		return nil, err
	}
	defer s.g.Release(id)
	rec, err := s.g.Record(id)
	if err != nil {
		return nil, err
	}
	if rec.State == domain.StateReported {
		return nil, fmt.Errorf("%w: すでに通報しています (投稿 %s)", domain.ErrConflict, rec.Thread)
	}
	if rec.State == domain.StateDismissed {
		return nil, fmt.Errorf("%w: 却下した事件です", domain.ErrConflict)
	}
	dir := s.g.ReportDir(id)
	var files []notify.ReportFile
	for _, n := range rec.Images {
		files = append(files, notify.ReportFile{Name: n, Path: filepath.Join(dir, filepath.Base(n))})
	}

	var text string
	if rec.Kind == "block" {
		user := detect.UserInfo{ID: rec.UserID, Name: rec.Name, UUID: rec.UUID}
		stats, err := s.cp.Stats(ctx, user, rec.Start-2, rec.End)
		if err != nil {
			return nil, fmt.Errorf("stats: %w", err)
		}
		// t: は「実行するときから何秒前まで」なので、検知のときの秒数のままだと、後で実行すると荒らしの始まりに届かない
		now := time.Now().Unix()
		seconds := int(now-rec.Start) + reportCmdMargin
		rec.RollbackCmd, rec.RestoreCmd = relay.RollbackCmd(rec.Name, seconds, rec.World), relay.RestoreCmd(rec.Name, seconds, rec.World)
		out := notify.Outcome{Cmd: rec.RollbackCmd, RestoreCmd: rec.RestoreCmd, RolledBack: rec.RolledBack, AI: rec.AI, Planned: !rec.RolledBack, CmdUntil: now + reportCmdMargin}
		if rec.RolledBack {
			done, total, derr := s.cp.RolledBack(ctx, rec.UserID, rec.World, rec.Start-2, rec.End)
			if derr == nil {
				out.RolledDone, out.RolledTotal = done, total
			}
		}
		text = s.blockReport(stats, rec.Trigger, out, files, capture.CamFor(stats))
	} else {
		b, err := os.ReadFile(filepath.Join(dir, "report.md"))
		if err != nil {
			return nil, err
		}
		text = string(b)
	}
	if n := domain.SafeChat(note, 500); n != "" {
		text = "**確認した人のメモ**: " + n + "\n" + text
	}
	if r := []rune(text); len(r) > 1900 {
		text = string(r[:1900])
	}

	now := time.Now().Unix()
	key := store.ThreadKey(rec.Name)
	existing := ""
	s.g.mu.Lock()
	if t, ok := s.g.store.S.Threads[key]; ok && now-t.At < 24*3600 {
		existing = t.ThreadID
	}
	s.g.mu.Unlock()
	var tags []string
	if rec.RolledBack && s.reporter.TagID() != "" {
		tags = append(tags, s.reporter.TagID())
	}
	if rec.Muted && s.g.cfg.ChatConfig.ChatMuteTagID != "" {
		tags = append(tags, s.g.cfg.ChatConfig.ChatMuteTagID)
	}
	thread, err := s.reporter.PostTags(ctx, existing, domain.SafeChat(rec.Name, 90), text, files, tags)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	s.g.mu.Lock()
	s.g.store.S.Threads[key] = store.ThreadRef{ThreadID: thread, At: now}
	_ = s.g.store.Save()
	s.g.mu.Unlock()
	return s.g.update(id, func(r *domain.Record) error {
		if err := r.Transition(domain.StateReported); err != nil {
			return err
		}
		r.Thread = thread
		r.RollbackCmd, r.RestoreCmd = rec.RollbackCmd, rec.RestoreCmd
		r.Decide(now, by, "report", domain.CleanText(note, 300))
		return nil
	})
}

// StartRollback は、ブロックの荒らしのロールバックを裏で始める (数分かかることがある)。
// 終わったかは、事件の記録 (busy が消え、rolled_back が立つ) で分かる。
func (s *IncidentService) StartRollback(ctx context.Context, id, by string) (*domain.Record, error) {
	if err := s.CheckMode(); err != nil {
		return nil, err
	}
	if s.console == nil {
		return nil, fmt.Errorf("%w: Minecraft のコンソールにつなげない設定です", domain.ErrUnavailable)
	}
	if err := s.g.Claim(id, "rollback"); err != nil {
		return nil, err
	}
	// 印を付けてから読むので、確かめたあとに状態が変わることはない
	rec, err := s.rollbackable(id)
	if err != nil {
		s.g.Release(id)
		return nil, err
	}
	go func() {
		defer s.g.Release(id)
		s.g.opMu.Lock()
		defer s.g.opMu.Unlock()
		rctx, cancel := context.WithTimeout(ctx, incidentDeadline)
		defer cancel()
		note, err := s.runRollback(rctx, rec)
		if err != nil {
			s.g.log.Error("ロールバックに失敗", "incident", id, "err", err)
			note = "失敗: " + err.Error()
		}
		_, _ = s.g.update(id, func(r *domain.Record) error {
			if err == nil {
				r.RolledBack = true
			}
			r.Decide(time.Now().Unix(), by, "rollback", note)
			return nil
		})
	}()
	return rec, nil
}

// rollbackable は事件を読み、ロールバックしてよいかを確かめる。
func (s *IncidentService) rollbackable(id string) (*domain.Record, error) {
	rec, err := s.g.Record(id)
	if err != nil {
		return nil, err
	}
	switch {
	case rec.Kind != "block":
		return nil, fmt.Errorf("%w: チャットの事件はロールバックできません", domain.ErrForbidden)
	case rec.RolledBack:
		return nil, fmt.Errorf("%w: すでにロールバックしています (やり直すときは、運営がコンソールで %s)", domain.ErrConflict, rec.RestoreCmd)
	case rec.State == domain.StateDismissed:
		return nil, fmt.Errorf("%w: 却下した事件です", domain.ErrConflict)
	case !domain.ValidName(rec.Name) || !domain.ValidWorld(rec.World):
		return nil, fmt.Errorf("%w: 名前 %q かワールド %q はコンソールに安全に渡せません。運営が手で行ってください", domain.ErrForbidden, rec.Name, rec.World)
	case s.g.Protected(rec.Name):
		return nil, fmt.Errorf("%w: %s は信頼する人 (または op) です", domain.ErrForbidden, rec.Name)
	case time.Now().Unix()-rec.Start > domain.MaxRollbackAge:
		return nil, fmt.Errorf("%w: 事件から 24 時間より長くたっています。ほかの人の作業を巻き込むので、運営が確かめてコンソールで実行してください", domain.ErrConflict)
	}
	return rec, nil
}

// rollbackVerified は、ロールバックが効いたかの判定。基準はデータベース (コンソールの行は、発言や切断で誤ることがある)。
// データベースを読めないときだけ、コンソールの「完了」の行を信じる。
func rollbackVerified(consoleErr error, done, total int, dbErr error) bool {
	if dbErr != nil {
		return consoleErr == nil
	}
	return total > 0 && done*100/total >= 95
}

// confirmRollback はデータベースで確かめる。CoreProtect の書き込みが少し遅れることがあるので、コンソールが「完了」のときは何度か読む。
func (s *IncidentService) confirmRollback(ctx context.Context, consoleErr error, userID int, world string, from, to int64) (done, total int, ok bool, dbErr error) {
	for try := 0; ; try++ {
		done, total, dbErr = s.cp.RolledBack(ctx, userID, world, from, to)
		ok = rollbackVerified(consoleErr, done, total, dbErr)
		if ok || consoleErr != nil || try >= rollbackConfirmTries-1 {
			return
		}
		select {
		case <-time.After(s.g.rollbackConfirmWait):
		case <-ctx.Done():
			return
		}
	}
}

// rollbackError は、確かめた結果が「戻っていない」ときのエラー。
func rollbackError(consoleErr error, done, total int) error {
	if consoleErr != nil {
		return consoleErr
	}
	return fmt.Errorf("データベースで確かめると、戻ったのは %d / %d 件です", done, total)
}

// runRollback はロールバックを実行し、前の画面と同じカメラで「ロールバック後」の画像を撮る。
// データベースでほぼ全部が戻ったときは、エラーを返さない。
func (s *IncidentService) runRollback(ctx context.Context, rec *domain.Record) (string, error) {
	seconds := int(time.Now().Unix()-rec.Start) + 5
	err := s.console.Rollback(ctx, rec.Name, seconds, rec.World)
	done, total, ok, derr := s.confirmRollback(ctx, err, rec.UserID, rec.World, rec.Start-2, rec.End)
	if !ok {
		return "", rollbackError(err, done, total)
	}
	note := fmt.Sprintf("%d/%d", done, total)
	if derr != nil {
		note = "データベースで確かめられない (コンソールは完了)"
	}

	user := detect.UserInfo{ID: rec.UserID, Name: rec.Name, UUID: rec.UUID}
	stats, serr := s.cp.Stats(ctx, user, rec.Start-2, rec.End)
	if serr != nil || stats.Empty() || s.shooter == nil || s.g.cfg.ShotConfig.MapIDs[stats.World] == "" {
		return note, nil
	}
	out := filepath.Join(s.g.ReportDir(rec.ID), "restored.png")
	got := s.bluemapShot(ctx, s.g.cfg.ShotConfig.MapIDs[stats.World], stats, capture.CamFor(stats), out,
		capture.AnnotFor(stats, fmt.Sprintf("%s  RESTORED (rollback done)", stats.User.Name), "yellow = affected area", false), true)
	if got != "" {
		_, _ = s.g.update(rec.ID, func(r *domain.Record) error {
			for _, n := range r.Images {
				if n == "restored.png" {
					return nil
				}
			}
			r.Images = append(r.Images, "restored.png")
			return nil
		})
	}
	return note, nil
}

// Mute は、ミュートのコマンド (CHAT_MUTE_CMD) を送る。急ぐ操作なので、事件の処理 (s.g.opMu) を待たない。
// コンソールの行は返さない (発言やアドレスが混ざる)。
func (s *IncidentService) Mute(ctx context.Context, name, by, reason string) error {
	if err := s.CheckMode(); err != nil {
		return err
	}
	cmd := relay.MuteCommand(s.g.cfg.ChatConfig.ChatMuteCmd, name)
	switch {
	case s.console == nil:
		return fmt.Errorf("%w: Minecraft のコンソールにつなげない設定です", domain.ErrUnavailable)
	case strings.TrimSpace(s.g.cfg.ChatConfig.ChatMuteCmd) == "":
		return fmt.Errorf("%w: CHAT_MUTE_CMD が空なので、ミュートできません", domain.ErrUnavailable)
	case cmd == "":
		return fmt.Errorf("%w: 名前 %q はコンソールに安全に渡せません", domain.ErrForbidden, name)
	case s.g.Protected(name):
		return fmt.Errorf("%w: %s は信頼する人 (または op) です", domain.ErrForbidden, name)
	}
	if _, err := s.console.Run(ctx, cmd, relay.SendOnlyTimeout, nil); err != nil {
		return err
	}
	s.g.log.Warn("ミュートした", "player", name, "by", by, "reason", domain.CleanText(reason, 100))
	return nil
}

// Kick は、プレイヤーを理由つきで退出させる。急ぐ操作なので、事件の処理 (s.g.opMu) を待たない。
func (s *IncidentService) Kick(ctx context.Context, name, by, reason string) error {
	if err := s.CheckMode(); err != nil {
		return err
	}
	switch {
	case s.console == nil:
		return fmt.Errorf("%w: Minecraft のコンソールにつなげない設定です", domain.ErrUnavailable)
	case !domain.ValidName(name):
		return fmt.Errorf("%w: 名前 %q はコンソールに安全に渡せません", domain.ErrForbidden, name)
	case s.g.Protected(name):
		return fmt.Errorf("%w: %s は信頼する人 (または op) です", domain.ErrForbidden, name)
	}
	reason = domain.CleanText(reason, relay.MaxKickReason)
	if reason == "" {
		reason = "運営の判断による"
	}
	if err := s.console.Kick(ctx, name, reason); err != nil {
		return err
	}
	s.g.log.Warn("キックした", "player", name, "by", by, "reason", reason)
	return nil
}

// ---- Guard からの委譲 (コネクタが使う入口) ----

// CheckMode は IncidentService へ委譲する。
func (g *Guard) CheckMode() error { return g.incidents.CheckMode() }

// Dismiss は IncidentService へ委譲する。
func (g *Guard) Dismiss(id, by, reason string) (*domain.Record, error) {
	return g.incidents.Dismiss(id, by, reason)
}

// ReportIncident は IncidentService へ委譲する。
func (g *Guard) ReportIncident(ctx context.Context, id, by, note string) (*domain.Record, error) {
	return g.incidents.ReportIncident(ctx, id, by, note)
}

// StartRollback は IncidentService へ委譲する。
func (g *Guard) StartRollback(ctx context.Context, id, by string) (*domain.Record, error) {
	return g.incidents.StartRollback(ctx, id, by)
}

// Mute は IncidentService へ委譲する。
func (g *Guard) Mute(ctx context.Context, name, by, reason string) error {
	return g.incidents.Mute(ctx, name, by, reason)
}

// Kick は IncidentService へ委譲する。
func (g *Guard) Kick(ctx context.Context, name, by, reason string) error {
	return g.incidents.Kick(ctx, name, by, reason)
}
