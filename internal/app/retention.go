package app

import (
	"context"
	"crime-buster/internal/domain"
	"os"
	"path/filepath"
	"time"
)

// Retention は、保持期間を過ぎたデータの削除を担う。
// Guard のループからは呼び出すだけで、削除の条件と実行はここが持つ。
type Retention struct {
	g *Guard
}

// Loop は、起動したときと 1 時間ごとに Sweep を呼ぶ。
func (r *Retention) Loop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		r.Sweep(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep は、保持期限を過ぎた事件のフォルダと、撮影の結果を消す (ディスクを使い切らないように)。
// 消すのは reports/ と shots/ の中だけ。いま動いている事件は消さない。REPORT_RETENTION が 0 なら、事件は消さない。
func (r *Retention) Sweep(now time.Time) {
	if keep := r.g.cfg.RetentionConfig.ReportRetention; keep > 0 {
		root := filepath.Join(r.g.cfg.StorageConfig.DataDir, "reports")
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if !e.IsDir() || !domain.ValidID(e.Name()) || r.g.IsBusy(e.Name()) {
				continue
			}
			if at, ok := incidentTime(r.g.ReportDir(e.Name())); ok && now.Sub(at) > keep {
				if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
					r.g.log.Warn("古い事件を消せない", "incident", e.Name(), "err", err)
					continue
				}
				r.g.log.Info("保持期限を過ぎた事件を消した", "incident", e.Name())
			}
		}
	}
	dir := filepath.Join(r.g.cfg.StorageConfig.DataDir, "shots")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > shotRetention {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
