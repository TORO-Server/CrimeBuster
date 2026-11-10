package main

import (
	"context"
	"crime-buster/internal/app"
	"crime-buster/internal/config"
	"crime-buster/internal/detect"
	"crime-buster/internal/notify"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---- scan-chat: 過去の発言に、いまの決まりを当てはめて、誤検知を見積もる (何も書かない・投稿しない) ----
func cmdScanChat(args []string) error {
	days := 30.0
	for i, a := range args {
		if a == "-days" && i+1 < len(args) {
			fmt.Sscan(args[i+1], &days)
		}
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cp, err := detect.OpenCP(cfg)
	if err != nil {
		return err
	}
	defer cp.Close()
	since := time.Now().Add(-time.Duration(days * 24 * float64(time.Hour))).Unix()
	msgs, err := cp.ChatRowsSince(ctx, since)
	if err != nil {
		return err
	}
	det := detect.NewChatDetector(cfg)
	ops := app.LoadOps(cfg.DetectConfig.OpsFile)
	type hit struct {
		T    int64
		Name string
		Trig string
		Why  string
	}
	var hits []hit
	cool := map[int]int64{}
	for _, m := range msgs {
		t := det.Feed(m)
		if t == nil || m.Time < cool[m.User] {
			continue
		}
		u, err := cp.User(ctx, m.User)
		if err != nil {
			continue
		}
		h := hit{T: m.Time, Name: u.Name, Trig: t.String()}
		if cfg.DetectConfig.TrustedPlayers[strings.ToLower(u.Name)] || ops[strings.ToLower(u.Name)] {
			h.Why = "信頼する人"
		}
		cool[m.User] = m.Time + 600
		hits = append(hits, h)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].T < hits[j].T })
	fmt.Printf("見た発言: %d 件 (%.0f 日)  閾値: 連投 %d 件/%s、同文 %d 回/%s、同じ文字 %d、リンク %d 回/5 分\n",
		len(msgs), days, cfg.ChatConfig.ChatRate, cfg.ChatConfig.ChatRateWindow, cfg.ChatConfig.ChatRepeat, cfg.ChatConfig.ChatRepeatWindow, cfg.ChatConfig.ChatRun, cfg.ChatConfig.ChatLinkRepeat)
	n := 0
	for _, h := range hits {
		if h.Why != "" {
			continue
		}
		n++
		fmt.Printf("  %s  %-20s %s\n", time.Unix(h.T, 0).In(notify.JST).Format("01/02 15:04:05"), h.Name, h.Trig)
	}
	fmt.Printf("通報の対象になる検知: %d 件 (対象外 %d 件)\n", n, len(hits)-n)
	return nil
}
