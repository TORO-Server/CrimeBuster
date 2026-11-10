package app

import (
	"context"
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

// ChatService は、チャットの検知と、チャットの事件の処理を担う。
// 共有する状態 (開いている事件、抑止、記録) は Guard が持ち、依存はポートで受ける。
type ChatService struct {
	g        *Guard
	cp       Reader
	ai       Judge
	console  Console
	reporter Reporter
}

// ChatResult は、チャットの事件 1 件の処理の結果。
type ChatResult struct {
	Dir      string
	Report   string
	ThreadID string
	Muted    bool
}

// ProcessChat は、チャットのスパム 1 件を、mode に合わせて処理する。
//
//	observe: まとめを記録するだけ
//	dry-run: 報告文をフォルダに置くだけ (投稿もミュートもしない)
//	live:    (設定があれば) ミュート → 通報。AI の判定だけの事件は、ミュートしない
func (s *ChatService) ProcessChat(ctx context.Context, inc *store.Incident, end int64) (*ChatResult, error) {
	if !domain.ValidID(inc.ID) {
		return nil, fmt.Errorf("事件の ID %q はフォルダに使えません", inc.ID)
	}
	msgs, err := s.cp.ChatFor(ctx, inc.UserID, inc.Start-60, end)
	if err != nil {
		return nil, fmt.Errorf("chat: %w", err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no chat found for %s", inc.Name)
	}
	dir := filepath.Join(s.g.cfg.StorageConfig.DataDir, "reports", inc.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	res := &ChatResult{Dir: dir}
	if config.CapabilitiesFor(s.g.cfg.Mode).RecordOnly {
		return res, writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "messages": len(msgs)})
	}

	in := notify.ChatReportInput{Name: inc.Name, UUID: inc.UUID, Msgs: msgs, Trigger: inc.Trigger, Mode: s.g.cfg.Mode}
	if !inc.AIOnly {
		in.MuteCmd = relay.MuteCommand(s.g.cfg.ChatConfig.ChatMuteCmd, inc.Name)
	}
	if config.CapabilitiesFor(s.g.cfg.Mode).AutoMute && in.MuteCmd != "" && s.console != nil {
		if _, err := s.console.Run(ctx, in.MuteCmd, relay.SendOnlyTimeout, nil); err != nil {
			in.MuteErr = err.Error()
			s.g.log.Error("ミュートに失敗", "player", inc.Name, "err", err)
		} else {
			in.Muted, res.Muted = true, true
		}
	}
	res.Report = notify.BuildChatReport(in)
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(res.Report), 0o644); err != nil {
		return res, err
	}
	_ = writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "messages": len(msgs), "muted": res.Muted})

	if config.CapabilitiesFor(s.g.cfg.Mode).AutoReport && s.reporter != nil {
		now := time.Now().Unix()
		key := store.ThreadKey(inc.Name)
		existing := ""
		s.g.mu.Lock()
		if t, ok := s.g.store.S.Threads[key]; ok && now-t.At < 24*3600 {
			existing = t.ThreadID
		}
		s.g.mu.Unlock()
		var tags []string
		if res.Muted && s.g.cfg.ChatConfig.ChatMuteTagID != "" {
			tags = append(tags, s.g.cfg.ChatConfig.ChatMuteTagID)
		}
		id, err := s.reporter.PostTags(ctx, existing, domain.SafeChat(inc.Name, 90), res.Report, nil, tags)
		if err != nil {
			return res, fmt.Errorf("post: %w", err)
		}
		res.ThreadID = id
		s.g.mu.Lock()
		s.g.store.S.Threads[key] = store.ThreadRef{ThreadID: id, At: now}
		_ = s.g.store.Save()
		s.g.mu.Unlock()
	}
	rec := &domain.Record{
		ID: inc.ID, Kind: "chat", Name: inc.Name, UUID: inc.UUID, UserID: inc.UserID,
		Created: time.Now().Unix(), Start: inc.Start, End: end, Trigger: inc.Trigger,
		State: domain.StatePending, Messages: len(msgs), Muted: res.Muted,
	}
	if res.ThreadID != "" {
		// 作った直後で状態は pending。遷移できなければ、記録を書かずに中断する
		if err := rec.Transition(domain.StateReported); err != nil {
			return res, err
		}
		rec.Thread = res.ThreadID
		rec.Decide(rec.Created, "crime-buster", "report", "")
	}
	if res.Muted {
		rec.Decide(rec.Created, "crime-buster", "mute", "")
	}
	if err := store.SaveRecord(dir, rec); err != nil {
		s.g.log.Warn("事件の記録が書けない", "incident", inc.ID, "err", err)
	}
	return res, nil
}

// stepChat は新しい発言を読んで、スパムを事件にする。
func (s *ChatService) stepChat(ctx context.Context) error {
	for {
		msgs, err := s.cp.PollChat(ctx, s.g.store.S.ChatCur, 5000)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		flags := s.aiChat(ctx, msgs, now) // 通信があるので、ロックの外で
		s.g.mu.Lock()
		for _, m := range msgs {
			s.g.store.S.ChatCur = m.RowID
			if now-m.Time > int64(s.g.cfg.DetectConfig.MaxEventAge.Seconds()) {
				continue
			}
			s.onChat(ctx, m, flags[m.RowID])
		}
		s.g.mu.Unlock()
		if len(msgs) < 5000 {
			return nil
		}
	}
}

// maxAIChatBatch は、AI に 1 度に見せる発言の数の上限。
const maxAIChatBatch = 40

// aiChat は、新しい発言を AI に見せて、対応すべき発言 (rowid → 判定) を返す。AI を使わない・失敗したときは空。
// 発言が多い (起動直後に溜まっていたときなど) ときは、新しいほうから maxAIChatBatch 件だけ見せる。
func (s *ChatService) aiChat(ctx context.Context, msgs []detect.ChatMsg, now int64) map[int64]*notify.ChatFlag {
	if s.ai == nil || !s.g.cfg.AIConfig.AIChat {
		return nil
	}
	var fresh []detect.ChatMsg
	for _, m := range msgs {
		if now-m.Time <= int64(s.g.cfg.DetectConfig.MaxEventAge.Seconds()) && strings.TrimSpace(m.Msg) != "" {
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
			if u, err := s.cp.User(ctx, m.User); err == nil {
				names[m.User] = u.Name
			}
		}
	}
	// 1 人ずつ尋ねる (ほかの人の発言の文字が、その人の判定に影響しないように)
	out := map[int64]*notify.ChatFlag{}
	for _, part := range chatByUser(fresh) {
		list, err := s.ai.ReviewChat(ctx, part, names)
		if err != nil {
			s.g.log.Warn("AI のチャットの判定が取れない (決まりだけで見張る)", "err", err)
			continue
		}
		for i := range list {
			f := list[i]
			if f.Confidence < s.g.cfg.AIConfig.AIMinConf {
				continue
			}
			out[part[f.ID].RowID] = &f
		}
	}
	return out
}

// chatByUser は、発言を人ごとに分ける。人の順は最初の発言の順、発言の順はそのまま。
func chatByUser(msgs []detect.ChatMsg) [][]detect.ChatMsg {
	index := map[int]int{}
	var out [][]detect.ChatMsg
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

// onChat は s.g.mu を持って呼ぶ。flag は、AI がこの発言を対応すべきと判定したとき (無ければ nil)。
func (s *ChatService) onChat(ctx context.Context, m detect.ChatMsg, flag *notify.ChatFlag) {
	key := store.ChatKey(m.User)
	open := s.g.store.S.Incidents[key]
	if open != nil && m.Time > open.Last {
		open.Last = m.Time
	}
	t := s.g.cdet.Feed(m)
	// 決まりの検知が重なったら、AI の判定だけの事件ではなくなる
	if t != nil && open != nil && open.AIOnly {
		open.AIOnly, open.Trigger = false, t.String()
	}
	if t == nil && flag != nil {
		t = &detect.ChatTrigger{User: m.User, Time: m.Time, Kind: "ai", Detail: fmt.Sprintf("%s (確からしさ %d%%) %s", flag.Category, flag.Confidence, flag.Summary)}
	}
	if t == nil || open != nil || m.Time < s.g.suppressed[-m.User] {
		return
	}
	u, err := s.cp.User(ctx, m.User)
	if err != nil {
		s.g.log.Error("ユーザーが引けない", "user", m.User, "err", err)
		return
	}
	lname := strings.ToLower(u.Name)
	if s.g.cfg.DetectConfig.TrustedPlayers[lname] || LoadOps(s.g.cfg.DetectConfig.OpsFile)[lname] {
		s.g.suppressed[-m.User] = m.Time + 3600 // チャットは、ブロックと別に数える (鍵を負の数にする)
		s.g.log.Info("チャットのスパムを検知したが対象外", "player", u.Name, "trigger", t.String())
		return
	}
	inc := &store.Incident{
		ID:     incidentID("chat-", m.Time, u.Name, m.User),
		UserID: m.User, Name: u.Name, UUID: u.UUID, Kind: "chat", AIOnly: t.Kind == "ai",
		Start: m.Time, Last: m.Time, Trigger: t.String(),
	}
	s.g.store.S.Incidents[key] = inc
	s.g.log.Warn("チャットのスパムを検知", "player", u.Name, "trigger", t.String(), "incident", inc.ID, "mode", s.g.cfg.Mode)
}

// ---- Guard からの委譲 ----

// ProcessChat は、チャットの事件の処理を ChatService へ委譲する。
func (g *Guard) ProcessChat(ctx context.Context, inc *store.Incident, end int64) (*ChatResult, error) {
	return g.chat.ProcessChat(ctx, inc, end)
}

// onChat は、チャットの検知を ChatService へ委譲する。
func (g *Guard) onChat(ctx context.Context, m detect.ChatMsg, flag *notify.ChatFlag) {
	g.chat.onChat(ctx, m, flag)
}
