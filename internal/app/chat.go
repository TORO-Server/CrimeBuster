package app

import (
	"context"
	"crime-buster/internal/config"
	"crime-buster/internal/domain"
	"crime-buster/internal/notify"
	"crime-buster/internal/relay"
	"crime-buster/internal/store"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

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
func (g *Guard) ProcessChat(ctx context.Context, inc *store.Incident, end int64) (*ChatResult, error) {
	if !domain.ValidID(inc.ID) {
		return nil, fmt.Errorf("事件の ID %q はフォルダに使えません", inc.ID)
	}
	msgs, err := g.cp.ChatFor(ctx, inc.UserID, inc.Start-60, end)
	if err != nil {
		return nil, fmt.Errorf("chat: %w", err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no chat found for %s", inc.Name)
	}
	dir := filepath.Join(g.cfg.StorageConfig.DataDir, "reports", inc.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	res := &ChatResult{Dir: dir}
	if config.CapabilitiesFor(g.cfg.Mode).RecordOnly {
		return res, writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "messages": len(msgs)})
	}

	in := notify.ChatReportInput{Name: inc.Name, UUID: inc.UUID, Msgs: msgs, Trigger: inc.Trigger, Mode: g.cfg.Mode}
	if !inc.AIOnly {
		in.MuteCmd = relay.MuteCommand(g.cfg.ChatConfig.ChatMuteCmd, inc.Name)
	}
	if config.CapabilitiesFor(g.cfg.Mode).AutoMute && in.MuteCmd != "" && g.ops != nil {
		if _, err := g.ops.C.Run(ctx, in.MuteCmd, relay.SendOnlyTimeout, nil); err != nil {
			in.MuteErr = err.Error()
			g.log.Error("ミュートに失敗", "player", inc.Name, "err", err)
		} else {
			in.Muted, res.Muted = true, true
		}
	}
	res.Report = notify.BuildChatReport(in)
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(res.Report), 0o644); err != nil {
		return res, err
	}
	_ = writeJSON(filepath.Join(dir, "incident.json"), map[string]any{"incident": inc, "messages": len(msgs), "muted": res.Muted})

	if config.CapabilitiesFor(g.cfg.Mode).AutoReport && g.reporter != nil {
		now := time.Now().Unix()
		key := store.ThreadKey(inc.Name)
		existing := ""
		g.mu.Lock()
		if t, ok := g.store.S.Threads[key]; ok && now-t.At < 24*3600 {
			existing = t.ThreadID
		}
		g.mu.Unlock()
		var tags []string
		if res.Muted && g.cfg.ChatConfig.ChatMuteTagID != "" {
			tags = append(tags, g.cfg.ChatConfig.ChatMuteTagID)
		}
		id, err := g.reporter.PostTags(ctx, existing, domain.SafeChat(inc.Name, 90), res.Report, nil, tags)
		if err != nil {
			return res, fmt.Errorf("post: %w", err)
		}
		res.ThreadID = id
		g.mu.Lock()
		g.store.S.Threads[key] = store.ThreadRef{ThreadID: id, At: now}
		_ = g.store.Save()
		g.mu.Unlock()
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
		g.log.Warn("事件の記録が書けない", "incident", inc.ID, "err", err)
	}
	return res, nil
}
