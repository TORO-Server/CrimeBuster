package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// cmdScan は、過去の N 日の記録に、いまの閾値と対象外の決まりを当てはめて、誤検知を見積もる。
// 何も書かず、何も投稿しない。閾値を決めるとき、動かす前に確かめるために使う。
func cmdScan(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	days := fs.Float64("days", 7, "さかのぼる日数")
	_ = fs.Parse(args)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	cp, err := OpenCP(cfg)
	if err != nil {
		return err
	}
	defer cp.Close()

	since := time.Now().Add(-time.Duration(*days * 24 * float64(time.Hour))).Unix()
	rid, err := cp.RowIDAtTime(ctx, since)
	if err != nil {
		return err
	}
	fmt.Printf("開始: %s (rowid %d)  閾値: 壊 %d / 置 %d / %s、長い窓 壊 %d / %s\n",
		time.Unix(since, 0).In(jst).Format("01/02 15:04"), rid, cfg.BreakBurst, cfg.PlaceBurst, cfg.BurstWindow, cfg.SustainedBreaks, cfg.SustainedWindow)

	type hit struct {
		Name    string
		At      int64
		Trigger string
		Why     string // 空なら、通報の対象
	}
	var hits []hit
	det := NewDetector(cfg)
	ops := LoadOps(cfg.OpsFile)
	suppressed := map[int]int64{}
	total := 0
	cursor := rid - 1
	for {
		events, err := cp.Poll(ctx, cursor, 20000)
		if err != nil {
			return err
		}
		for _, e := range events {
			cursor = e.RowID
			total++
			t := det.Feed(e)
			if t == nil || e.Time < suppressed[e.User] {
				continue
			}
			u, err := cp.User(ctx, t.User)
			if err != nil {
				continue
			}
			h := hit{Name: u.Name, At: e.Time, Trigger: t.String()}
			switch {
			case strings.HasPrefix(u.Name, "#"):
				h.Why = "人ではない"
				suppressed[t.User] = e.Time + 3600
			case cfg.TrustedPlayers[strings.ToLower(u.Name)] || ops[strings.ToLower(u.Name)]:
				h.Why = "信頼する人"
				suppressed[t.User] = e.Time + 3600
			default:
				cmd, _ := cp.RecentEditCommand(ctx, t.User, e.Time-int64(cfg.WEGrace.Seconds()))
				if cmd != "" {
					h.Why = "編集コマンド " + cmd
					suppressed[t.User] = e.Time + 600
				} else {
					suppressed[t.User] = e.Time + int64(cfg.MaxIncident.Seconds()) // 事件として 1 回数える
				}
			}
			hits = append(hits, h)
		}
		if total%500000 < 20000 {
			fmt.Printf("  ... %d 行 (%s)\n", total, time.Unix(lastTime(events), 0).In(jst).Format("01/02 15:04"))
		}
		if len(events) < 20000 {
			break
		}
	}
	fmt.Printf("見た行: %d\n", total)

	byWhy := map[string]int{}
	var reportable []hit
	for _, h := range hits {
		if h.Why == "" {
			reportable = append(reportable, h)
		} else {
			key := h.Why
			if i := strings.Index(key, " /"); i > 0 {
				key = key[:i]
			}
			byWhy[key]++
		}
	}
	keys := make([]string, 0, len(byWhy))
	for k := range byWhy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("対象外にした検知: %s ×%d\n", k, byWhy[k])
	}
	fmt.Printf("通報の対象になる検知: %d 件\n", len(reportable))
	for _, h := range reportable {
		fmt.Printf("  %s  %-20s %s\n", time.Unix(h.At, 0).In(jst).Format("01/02 15:04:05"), h.Name, h.Trigger)
	}
	return nil
}

func lastTime(ev []Event) int64 {
	if len(ev) == 0 {
		return 0
	}
	return ev[len(ev)-1].Time
}
