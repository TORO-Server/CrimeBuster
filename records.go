package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 事件の状態。コネクタ (control) は、pending のものを「確認待ち」として見せる。
const (
	StatePending   = "pending"   // 確認待ち (通報も却下もまだ)
	StateReported  = "reported"  // フォーラムに通報した
	StateDismissed = "dismissed" // 却下した (荒らしではなかった、など)
)

// Decision は、事件に対して誰が何をしたかの記録。
type Decision struct {
	At     int64  `json:"at"`
	By     string `json:"by"`
	Action string `json:"action"` // report / rollback / dismiss / mute
	Note   string `json:"note,omitempty"`
}

// Record は、reports/<id>/record.json に置く、事件 1 件の状態。画像と報告文は同じフォルダにある。
type Record struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // "block" か "chat"
	Name    string `json:"name"`
	UUID    string `json:"uuid"`
	UserID  int    `json:"user_id"`
	Created int64  `json:"created"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	World   string `json:"world,omitempty"`
	Trigger string `json:"trigger"`

	State      string `json:"state"`
	Busy       string `json:"busy,omitempty"` // 裏で動いている操作 (rollback)
	Reported   bool   `json:"reported"`
	Thread     string `json:"thread,omitempty"`
	RolledBack bool   `json:"rolled_back"`
	Muted      bool   `json:"muted,omitempty"`

	Images      []string `json:"images,omitempty"` // 同じフォルダの中のファイル名
	Breaks      int      `json:"breaks,omitempty"`
	Places      int      `json:"places,omitempty"`
	Messages    int      `json:"messages,omitempty"`
	RollbackCmd string   `json:"rollback_cmd,omitempty"`
	RestoreCmd  string   `json:"restore_cmd,omitempty"`
	AI          *Verdict `json:"ai,omitempty"`

	History []Decision `json:"history,omitempty"`
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,100}$`)

// validID は事件の ID として安全か (フォルダ名に使うので、区切りや .. を通さない)。
func validID(id string) bool { return idRE.MatchString(id) && !strings.Contains(id, "..") }

func (g *Guard) reportDir(id string) string { return filepath.Join(g.cfg.DataDir, "reports", id) }

func recordPath(dir string) string { return filepath.Join(dir, "record.json") }

func loadRecord(dir string) (*Record, error) {
	b, err := os.ReadFile(recordPath(dir))
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", recordPath(dir), err)
	}
	return &r, nil
}

// saveRecord は一時ファイルに書いてから置き換える。
func saveRecord(dir string, r *Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := recordPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, recordPath(dir))
}

// ErrNoRecord は、その ID の事件が無いとき。
var ErrNoRecord = errors.New("incident not found")

// Record は ID の事件を読む。Busy は、このプロセスの中で動いている操作を重ねる。
func (g *Guard) Record(id string) (*Record, error) {
	if !validID(id) {
		return nil, ErrNoRecord
	}
	r, err := loadRecord(g.reportDir(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoRecord
	}
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	r.Busy = g.busy[id]
	g.mu.Unlock()
	return r, nil
}

// Records は、新しい順の事件の一覧。state が空なら、すべて。
func (g *Guard) Records(state string, limit int) ([]*Record, error) {
	entries, err := os.ReadDir(filepath.Join(g.cfg.DataDir, "reports"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		r, err := loadRecord(g.reportDir(e.Name()))
		if err != nil {
			continue // record.json の無い古いフォルダ (observe のときのものなど)
		}
		if state != "" && r.State != state {
			continue
		}
		g.mu.Lock()
		r.Busy = g.busy[r.ID]
		g.mu.Unlock()
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// update は、事件の記録を読み直して f で書き換えて保存する (同じ事件への操作が重ならないように g.recMu を持つ)。
func (g *Guard) update(id string, f func(*Record)) (*Record, error) {
	if !validID(id) {
		return nil, ErrNoRecord
	}
	g.recMu.Lock()
	defer g.recMu.Unlock()
	r, err := loadRecord(g.reportDir(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoRecord
	}
	if err != nil {
		return nil, err
	}
	f(r)
	return r, saveRecord(g.reportDir(id), r)
}

func (r *Record) decide(at int64, by, action, note string) {
	r.History = append(r.History, Decision{At: at, By: by, Action: action, Note: note})
}

// shotRetention は、コネクタ経由の撮影の結果を残す時間 (job は 1 時間で捨てる。再起動で job を忘れた分も、ここで消える)。
const shotRetention = 2 * time.Hour

// sweepLoop は、起動したときと 1 時間ごとに、古いデータを消す。
func (g *Guard) sweepLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		g.sweepData(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sweepData は、保持期限を過ぎた事件のフォルダと、撮影の結果を消す (ディスクを使い切らないように)。
// 消すのは reports/ と shots/ の中だけ。いま動いている事件は消さない。REPORT_RETENTION が 0 なら、事件は消さない。
func (g *Guard) sweepData(now time.Time) {
	if keep := g.cfg.ReportRetention; keep > 0 {
		root := filepath.Join(g.cfg.DataDir, "reports")
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if !e.IsDir() || !validID(e.Name()) || g.isBusy(e.Name()) {
				continue
			}
			if at, ok := incidentTime(g.reportDir(e.Name())); ok && now.Sub(at) > keep {
				if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
					g.log.Warn("古い事件を消せない", "incident", e.Name(), "err", err)
					continue
				}
				g.log.Info("保持期限を過ぎた事件を消した", "incident", e.Name())
			}
		}
	}
	dir := filepath.Join(g.cfg.DataDir, "shots")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > shotRetention {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func (g *Guard) isBusy(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.busy[id] != ""
}

// incidentTime は、事件ができた時刻。記録が無いフォルダ (observe のときのものなど) は、フォルダの更新時刻。
func incidentTime(dir string) (time.Time, bool) {
	if r, err := loadRecord(dir); err == nil && r.Created > 0 {
		return time.Unix(r.Created, 0), true
	}
	info, err := os.Stat(dir)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}
