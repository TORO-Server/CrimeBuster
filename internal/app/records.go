package app

import (
	"crime-buster/internal/domain"
	"crime-buster/internal/store"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (g *Guard) ReportDir(id string) string {
	return filepath.Join(g.cfg.StorageConfig.DataDir, "reports", id)
}

// Record は ID の事件を読む。Busy は、このプロセスの中で動いている操作を重ねる。
func (g *Guard) Record(id string) (*domain.Record, error) {
	if !domain.ValidID(id) {
		return nil, store.ErrNoRecord
	}
	r, err := store.LoadRecord(g.ReportDir(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, store.ErrNoRecord
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
func (g *Guard) Records(state domain.IncidentState, limit int) ([]*domain.Record, error) {
	entries, err := os.ReadDir(filepath.Join(g.cfg.StorageConfig.DataDir, "reports"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*domain.Record
	for _, e := range entries {
		if !e.IsDir() || !domain.ValidID(e.Name()) {
			continue
		}
		r, err := store.LoadRecord(g.ReportDir(e.Name()))
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
// f がエラーを返したら、保存せずにそのエラーを返す。
func (g *Guard) update(id string, f func(*domain.Record) error) (*domain.Record, error) {
	if !domain.ValidID(id) {
		return nil, store.ErrNoRecord
	}
	g.recMu.Lock()
	defer g.recMu.Unlock()
	r, err := store.LoadRecord(g.ReportDir(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, store.ErrNoRecord
	}
	if err != nil {
		return nil, err
	}
	if err := f(r); err != nil {
		return nil, err
	}
	return r, store.SaveRecord(g.ReportDir(id), r)
}

// shotRetention は、コネクタ経由の撮影の結果を残す時間 (job は 1 時間で捨てる。再起動で job を忘れた分も、ここで消える)。
const shotRetention = 2 * time.Hour

// sweepData は、保持期間の処理 (Retention) への委譲。
func (g *Guard) sweepData(now time.Time) { g.retention.Sweep(now) }

func (g *Guard) IsBusy(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.busy[id] != ""
}

// incidentTime は、事件ができた時刻。記録が無いフォルダ (observe のときのものなど) は、フォルダの更新時刻。
func incidentTime(dir string) (time.Time, bool) {
	if r, err := store.LoadRecord(dir); err == nil && r.Created > 0 {
		return time.Unix(r.Created, 0), true
	}
	info, err := os.Stat(dir)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}
