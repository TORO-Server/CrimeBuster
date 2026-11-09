package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// コネクタ (control) から頼まれる操作。人が (または AI が) 確かめて決めたことを、ここで実行する。
// どれも、動かし方が review か live のときだけ受ける。by は、頼んだ人 (記録に残す)。

// reportCmdMargin は、通報に書くコマンドの t: に足す秒数。運営がこの時間のうちに実行すれば、荒らしの始まりまで届く。
const reportCmdMargin = 3600

// maxRollbackAge は、コネクタ経由と自動のロールバックが受ける事件の古さの上限 (秒)。
// t: は「いまからさかのぼる」ので、古い事件ほど、そのあとの正当な作業も巻き込む。
const maxRollbackAge = 24 * 3600

// maxRollbackSeconds は、コンソールに送る t: の上限 (秒)。中継もこの値で断る。
const maxRollbackSeconds = maxRollbackAge + 600

var (
	// ErrForbidden は、してはいけない相手 (信頼する人・op) や、受け付けない動かし方のとき。
	ErrForbidden = errors.New("forbidden")
	// ErrConflict は、すでにやった・いま別の操作が動いているなど、いまはできないとき。
	ErrConflict = errors.New("conflict")
	// ErrUnavailable は、必要な部品 (コンソール・Discord・ブラウザ) が使えない設定のとき。
	ErrUnavailable = errors.New("unavailable")
)

func (g *Guard) checkMode() error {
	if !CapabilitiesFor(g.cfg.Mode).AcceptOps {
		return fmt.Errorf("%w: CRIMEBUSTER_MODE=%s のあいだは、操作を受け付けません (review か live にしてください)", ErrForbidden, g.cfg.Mode)
	}
	return nil
}

// protected は、荒らしとして扱ってはいけない人 (信頼する人・op)。
func (g *Guard) protected(name string) bool {
	l := strings.ToLower(name)
	return g.cfg.TrustedPlayers[l] || LoadOps(g.cfg.OpsFile)[l]
}

// ctlRE は、制御文字と、行を分ける文字。コンソールの 1 行にも、Discord の 1 行にも入れない。
var ctlRE = regexp.MustCompile(`[\p{C}\p{Zl}\p{Zp}]`)

// cleanText は、コンソールや Discord に渡す理由の文字から、制御文字 (改行など) を除いて短くする。
func cleanText(s string, max int) string {
	s = strings.TrimSpace(ctlRE.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// claim は、事件に「いま動いている操作」を付ける。別の操作が動いていれば ErrConflict。
// 通報・ロールバック・却下が、同じ事件で重ならないようにする。
func (g *Guard) claim(id, op string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if b := g.busy[id]; b != "" {
		return fmt.Errorf("%w: いま %s を実行中です。終わってからやり直してください", ErrConflict, b)
	}
	g.busy[id] = op
	return nil
}

func (g *Guard) release(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.busy, id)
}

// Dismiss は、荒らしではないと判断した事件を閉じる。
func (g *Guard) Dismiss(id, by, reason string) (*Record, error) {
	if err := g.claim(id, "dismiss"); err != nil {
		return nil, err
	}
	defer g.release(id)
	return g.update(id, func(r *Record) error {
		if r.State == StatePending {
			// pending であることを確かめた直後なので、この遷移は失敗しない
			if err := r.Transition(StateDismissed); err != nil {
				return err
			}
			r.decide(time.Now().Unix(), by, "dismiss", cleanText(reason, 300))
		}
		return nil
	})
}

// ReportIncident は、事件をフォーラムに通報する。note があれば、本文の頭に足す。
// ブロックの荒らしは、いまのロールバックの状態で、報告文を作り直す。
func (g *Guard) ReportIncident(ctx context.Context, id, by, note string) (*Record, error) {
	if err := g.checkMode(); err != nil {
		return nil, err
	}
	if g.reporter == nil {
		return nil, fmt.Errorf("%w: Discord に投稿する設定 (DiscordSRV のトークンと Discord-MCP) がありません", ErrUnavailable)
	}
	g.opMu.Lock()
	defer g.opMu.Unlock()
	if err := g.claim(id, "report"); err != nil {
		return nil, err
	}
	defer g.release(id)
	rec, err := g.Record(id)
	if err != nil {
		return nil, err
	}
	if rec.State == StateReported {
		return nil, fmt.Errorf("%w: すでに通報しています (投稿 %s)", ErrConflict, rec.Thread)
	}
	if rec.State == StateDismissed {
		return nil, fmt.Errorf("%w: 却下した事件です", ErrConflict)
	}
	dir := g.reportDir(id)
	var files []ReportFile
	for _, n := range rec.Images {
		files = append(files, ReportFile{Name: n, Path: filepath.Join(dir, filepath.Base(n))})
	}

	var text string
	if rec.Kind == "block" {
		user := UserInfo{ID: rec.UserID, Name: rec.Name, UUID: rec.UUID}
		stats, err := g.cp.Stats(ctx, user, rec.Start-2, rec.End)
		if err != nil {
			return nil, fmt.Errorf("stats: %w", err)
		}
		// t: は「実行するときから何秒前まで」なので、検知のときの秒数のままだと、後で実行すると荒らしの始まりに届かない
		now := time.Now().Unix()
		seconds := int(now-rec.Start) + reportCmdMargin
		rec.RollbackCmd, rec.RestoreCmd = RollbackCmd(rec.Name, seconds, rec.World), RestoreCmd(rec.Name, seconds, rec.World)
		out := Outcome{Cmd: rec.RollbackCmd, RestoreCmd: rec.RestoreCmd, RolledBack: rec.RolledBack, AI: rec.AI, Planned: !rec.RolledBack, CmdUntil: now + reportCmdMargin}
		if rec.RolledBack {
			done, total, derr := g.cp.RolledBack(ctx, rec.UserID, rec.World, rec.Start-2, rec.End)
			if derr == nil {
				out.RolledDone, out.RolledTotal = done, total
			}
		}
		text = g.blockReport(stats, rec.Trigger, out, files, CamFor(stats))
	} else {
		b, err := os.ReadFile(filepath.Join(dir, "report.md"))
		if err != nil {
			return nil, err
		}
		text = string(b)
	}
	if n := safeChat(note, 500); n != "" {
		text = "**確認した人のメモ**: " + n + "\n" + text
	}
	if r := []rune(text); len(r) > 1900 {
		text = string(r[:1900])
	}

	now := time.Now().Unix()
	key := threadKey(rec.Name)
	existing := ""
	g.mu.Lock()
	if t, ok := g.store.S.Threads[key]; ok && now-t.At < 24*3600 {
		existing = t.ThreadID
	}
	g.mu.Unlock()
	var tags []string
	if rec.RolledBack && g.reporter.tagID != "" {
		tags = append(tags, g.reporter.tagID)
	}
	if rec.Muted && g.cfg.ChatMuteTagID != "" {
		tags = append(tags, g.cfg.ChatMuteTagID)
	}
	thread, err := g.reporter.PostTags(ctx, existing, safeChat(rec.Name, 90), text, files, tags)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	g.mu.Lock()
	g.store.S.Threads[key] = ThreadRef{ThreadID: thread, At: now}
	_ = g.store.Save()
	g.mu.Unlock()
	return g.update(id, func(r *Record) error {
		if err := r.Transition(StateReported); err != nil {
			return err
		}
		r.Thread = thread
		r.RollbackCmd, r.RestoreCmd = rec.RollbackCmd, rec.RestoreCmd
		r.decide(now, by, "report", cleanText(note, 300))
		return nil
	})
}

// StartRollback は、ブロックの荒らしのロールバックを裏で始める (数分かかることがある)。
// 終わったかは、事件の記録 (busy が消え、rolled_back が立つ) で分かる。
func (g *Guard) StartRollback(ctx context.Context, id, by string) (*Record, error) {
	if err := g.checkMode(); err != nil {
		return nil, err
	}
	if g.ops == nil {
		return nil, fmt.Errorf("%w: Minecraft のコンソールにつなげない設定です", ErrUnavailable)
	}
	if err := g.claim(id, "rollback"); err != nil {
		return nil, err
	}
	// 印を付けてから読むので、確かめたあとに状態が変わることはない
	rec, err := g.rollbackable(id)
	if err != nil {
		g.release(id)
		return nil, err
	}
	go func() {
		defer g.release(id)
		g.opMu.Lock()
		defer g.opMu.Unlock()
		rctx, cancel := context.WithTimeout(ctx, incidentDeadline)
		defer cancel()
		note, err := g.runRollback(rctx, rec)
		if err != nil {
			g.log.Error("ロールバックに失敗", "incident", id, "err", err)
			note = "失敗: " + err.Error()
		}
		_, _ = g.update(id, func(r *Record) error {
			if err == nil {
				r.RolledBack = true
			}
			r.decide(time.Now().Unix(), by, "rollback", note)
			return nil
		})
	}()
	return rec, nil
}

// rollbackable は事件を読み、ロールバックしてよいかを確かめる。
func (g *Guard) rollbackable(id string) (*Record, error) {
	rec, err := g.Record(id)
	if err != nil {
		return nil, err
	}
	switch {
	case rec.Kind != "block":
		return nil, fmt.Errorf("%w: チャットの事件はロールバックできません", ErrForbidden)
	case rec.RolledBack:
		return nil, fmt.Errorf("%w: すでにロールバックしています (やり直すときは、運営がコンソールで %s)", ErrConflict, rec.RestoreCmd)
	case rec.State == StateDismissed:
		return nil, fmt.Errorf("%w: 却下した事件です", ErrConflict)
	case !validName(rec.Name) || !validWorld(rec.World):
		return nil, fmt.Errorf("%w: 名前 %q かワールド %q はコンソールに安全に渡せません。運営が手で行ってください", ErrForbidden, rec.Name, rec.World)
	case g.protected(rec.Name):
		return nil, fmt.Errorf("%w: %s は信頼する人 (または op) です", ErrForbidden, rec.Name)
	case time.Now().Unix()-rec.Start > maxRollbackAge:
		return nil, fmt.Errorf("%w: 事件から 24 時間より長くたっています。ほかの人の作業を巻き込むので、運営が確かめてコンソールで実行してください", ErrConflict)
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
func (g *Guard) confirmRollback(ctx context.Context, consoleErr error, userID int, world string, from, to int64) (done, total int, ok bool, dbErr error) {
	for try := 0; ; try++ {
		done, total, dbErr = g.cp.RolledBack(ctx, userID, world, from, to)
		ok = rollbackVerified(consoleErr, done, total, dbErr)
		if ok || consoleErr != nil || try >= rollbackConfirmTries-1 {
			return
		}
		select {
		case <-time.After(g.rollbackConfirmWait):
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
func (g *Guard) runRollback(ctx context.Context, rec *Record) (string, error) {
	seconds := int(time.Now().Unix()-rec.Start) + 5
	err := g.ops.Rollback(ctx, rec.Name, seconds, rec.World)
	done, total, ok, derr := g.confirmRollback(ctx, err, rec.UserID, rec.World, rec.Start-2, rec.End)
	if !ok {
		return "", rollbackError(err, done, total)
	}
	note := fmt.Sprintf("%d/%d", done, total)
	if derr != nil {
		note = "データベースで確かめられない (コンソールは完了)"
	}

	user := UserInfo{ID: rec.UserID, Name: rec.Name, UUID: rec.UUID}
	stats, serr := g.cp.Stats(ctx, user, rec.Start-2, rec.End)
	if serr != nil || stats.Empty() || g.shooter == nil || g.cfg.MapIDs[stats.World] == "" {
		return note, nil
	}
	out := filepath.Join(g.reportDir(rec.ID), "restored.png")
	got := g.bluemapShot(ctx, g.cfg.MapIDs[stats.World], stats, CamFor(stats), out,
		AnnotFor(stats, fmt.Sprintf("%s  RESTORED (rollback done)", stats.User.Name), "yellow = affected area", false), true)
	if got != "" {
		_, _ = g.update(rec.ID, func(r *Record) error {
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

// Mute は、ミュートのコマンド (CHAT_MUTE_CMD) を送る。急ぐ操作なので、事件の処理 (g.opMu) を待たない。
// コンソールの行は返さない (発言やアドレスが混ざる)。
func (g *Guard) Mute(ctx context.Context, name, by, reason string) error {
	if err := g.checkMode(); err != nil {
		return err
	}
	cmd := MuteCommand(g.cfg.ChatMuteCmd, name)
	switch {
	case g.ops == nil:
		return fmt.Errorf("%w: Minecraft のコンソールにつなげない設定です", ErrUnavailable)
	case strings.TrimSpace(g.cfg.ChatMuteCmd) == "":
		return fmt.Errorf("%w: CHAT_MUTE_CMD が空なので、ミュートできません", ErrUnavailable)
	case cmd == "":
		return fmt.Errorf("%w: 名前 %q はコンソールに安全に渡せません", ErrForbidden, name)
	case g.protected(name):
		return fmt.Errorf("%w: %s は信頼する人 (または op) です", ErrForbidden, name)
	}
	if _, err := g.ops.C.Run(ctx, cmd, sendOnlyTimeout, nil); err != nil {
		return err
	}
	g.log.Warn("ミュートした", "player", name, "by", by, "reason", cleanText(reason, 100))
	return nil
}

// Kick は、プレイヤーを理由つきで退出させる。急ぐ操作なので、事件の処理 (g.opMu) を待たない。
func (g *Guard) Kick(ctx context.Context, name, by, reason string) error {
	if err := g.checkMode(); err != nil {
		return err
	}
	switch {
	case g.ops == nil:
		return fmt.Errorf("%w: Minecraft のコンソールにつなげない設定です", ErrUnavailable)
	case !validName(name):
		return fmt.Errorf("%w: 名前 %q はコンソールに安全に渡せません", ErrForbidden, name)
	case g.protected(name):
		return fmt.Errorf("%w: %s は信頼する人 (または op) です", ErrForbidden, name)
	}
	reason = cleanText(reason, maxKickReason)
	if reason == "" {
		reason = "運営の判断による"
	}
	if err := g.ops.Kick(ctx, name, reason); err != nil {
		return err
	}
	g.log.Warn("キックした", "player", name, "by", by, "reason", reason)
	return nil
}
