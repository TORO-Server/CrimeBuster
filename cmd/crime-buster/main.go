// crime-buster: TORO サーバーの荒らしを監視し、証拠の画像を付けて通報し、ロールバックする。
//
//	crime-buster run                    監視を続ける (CRIMEBUSTER_MODE=observe | dry-run | live)
//	crime-buster console-relay          コンソールの中継を開く (Docker のソケットを持つ別のコンテナで動かす)
//	crime-buster check [-console]       設定・データベース・Discord のトークンとチャンネルを確かめる (何も投稿しない)
//	crime-buster replay -user NAME -from RFC3339 -to RFC3339 [-out DIR] [-no-shot]
//	                                  過去の記録に閾値を当てはめ、画像と報告文を DIR に作る (投稿・ロールバックはしない)
package main

import (
	"context"
	"crime-buster/internal/app"
	"crime-buster/internal/capture"
	"crime-buster/internal/config"
	"crime-buster/internal/connector"
	"crime-buster/internal/detect"
	"crime-buster/internal/notify"
	"crime-buster/internal/relay"
	"crime-buster/internal/store"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: crime-buster run | console-relay | check | replay | healthcheck")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(log)
	case "console-relay":
		err = relay.CmdConsoleRelay(log)
	case "check":
		err = cmdCheck(log, os.Args[2:])
	case "replay":
		err = cmdReplay(log, os.Args[2:])
	case "scan":
		err = cmdScan(log, os.Args[2:])
	case "scan-chat":
		err = cmdScanChat(log, os.Args[2:])
	case "healthcheck":
		err = cmdHealth(log, os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Error("失敗", "err", err)
		os.Exit(1)
	}
}

// loadConfig は設定を読み、旧名 (互換) で読んだ環境変数があれば、起動のログに1回出す。
func loadConfig(log *slog.Logger) (*config.Config, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, err
	}
	if names := cfg.LegacyEnvNames(); len(names) > 0 {
		log.Warn("環境変数を旧名で読んだ (新しい名前への移行を推奨)", "names", strings.Join(names, ", "))
	}
	return cfg, nil
}

func cmdRun(log *slog.Logger) error {
	cfg, err := loadConfig(log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cp, err := detect.OpenCP(cfg)
	if err != nil {
		return err
	}
	defer cp.Close()
	if err := cp.Ping(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if cfg.DBConfig.DBUser == "root" {
		log.Warn("データベースに root でつないでいます。SELECT だけの専用のユーザーにしてください (DB_USER、DB_PASSWORD_FILE)")
	}
	st, err := store.OpenStore(cfg.StorageConfig.DataDir)
	if err != nil {
		return err
	}

	var ops *relay.Ops
	var sh *capture.Shooter
	var rep *notify.Reporter
	caps := config.CapabilitiesFor(cfg.Mode)
	if caps.Capture {
		ops = &relay.Ops{C: relay.NewRelayConsole(cfg.ConsoleConfig.ConsoleURL)}
		sh = capture.NewShooter(cfg)
	}
	// 通報の部品は、自動で投稿する動かし方 (AutoReport) と、コネクタ経由の操作で投稿を受ける動かし方 (AcceptOps) で要る。
	if caps.AcceptOps || caps.AutoReport {
		rep, err = notify.ReporterFor(cfg)
		if err != nil {
			return err
		}
		info, err := rep.Check(ctx)
		if err != nil {
			return fmt.Errorf("discord: %w", err)
		}
		log.Info("Discord に接続できる", "info", info)
	}
	g := app.NewGuard(cfg, cp, st, ops, sh, rep, log)
	if cfg.ConnectorConfig.Addr != "" {
		connector.Version = version
		conn, err := connector.NewConnector(ctx, g)
		if err != nil {
			return err
		}
		go func() {
			if err := conn.Serve(ctx, cfg.ConnectorConfig.Addr); err != nil {
				log.Error("コネクタが止まった", "err", err)
			}
		}()
	} else {
		log.Info("コネクタは開かない (CONNECTOR_ADDR が空)")
	}
	log.Info("起動", "version", version, "mode", cfg.Mode,
		"burst", fmt.Sprintf("壊 %d / 置 %d / %s", cfg.DetectConfig.BreakBurst, cfg.DetectConfig.PlaceBurst, cfg.DetectConfig.BurstWindow),
		"sustained", fmt.Sprintf("壊 %d / %s", cfg.DetectConfig.SustainedBreaks, cfg.DetectConfig.SustainedWindow),
		"trusted", len(cfg.DetectConfig.TrustedPlayers), "warps", g.WarpCount())
	app.TouchHealth(cfg)
	return g.Run(ctx)
}

func cmdCheck(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	console := fs.Bool("console", false, "Minecraft のコンソールに `bluemap` (状態の表示だけ) を送って確かめる")
	_ = fs.Parse(args)
	cfg, err := loadConfig(log)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cp, err := detect.OpenCP(cfg)
	if err != nil {
		return err
	}
	defer cp.Close()
	if err := cp.Ping(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	n, err := cp.MaxRowID(ctx)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	fmt.Printf("database: ok (%s:%s/%s, 最後の行 %d)\n", cfg.DBConfig.DBHost, cfg.DBConfig.DBPort, cfg.DBConfig.DBName, n)

	rep, err := notify.ReporterFor(cfg)
	if err != nil {
		return err
	}
	info, err := rep.Check(ctx)
	if err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	fmt.Println("discord: ok (" + info + ")  ※何も投稿していません")

	if ai := notify.NewAI(cfg); ai == nil {
		fmt.Println("ai: 使わない (AI_API_KEY が無い)。ロールバックは、閾値の決まりだけで決まる")
	} else {
		// 無害な 1 件の発言を見せて、答えが JSON で返ってくるかだけ確かめる
		if _, err := ai.ReviewChat(ctx, []detect.ChatMsg{{Time: time.Now().Unix(), User: 1, Msg: "こんにちは"}}, map[int]string{1: "test"}); err != nil {
			return fmt.Errorf("ai: %w", err)
		}
		fmt.Printf("ai: ok (%s)\n", cfg.AIConfig.AIModel)
	}

	if *console {
		ops := relay.Ops{C: relay.NewRelayConsole(cfg.ConsoleConfig.ConsoleURL)}
		for _, mapID := range cfg.ShotConfig.MapIDs {
			idle, err := ops.BlueMapIdle(ctx, mapID)
			if err != nil {
				return fmt.Errorf("console: %w", err)
			}
			fmt.Printf("console: ok (bluemap %s: 更新済みで待機中=%v)\n", mapID, idle)
		}
	}
	fmt.Printf("mode=%s  warps=%d\n", cfg.Mode, len(store.LoadWarps(cfg.StorageConfig.WarpsDir)))
	return nil
}

func cmdReplay(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	user := fs.String("user", "", "プレイヤー名")
	from := fs.String("from", "", "開始 (RFC3339。例 2026-11-04T10:50:00+09:00)")
	to := fs.String("to", "", "終了 (RFC3339)")
	out := fs.String("out", "replay-out", "出力先のフォルダ")
	noShot := fs.Bool("no-shot", false, "BlueMap の画面を撮らない (真上の図だけ)")
	_ = fs.Parse(args)
	if *user == "" || *from == "" || *to == "" {
		return fmt.Errorf("-user, -from, -to are required")
	}
	t0, err := time.Parse(time.RFC3339, *from)
	if err != nil {
		return err
	}
	t1, err := time.Parse(time.RFC3339, *to)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(log)
	if err != nil {
		return err
	}
	cfg.Mode = config.ModeDryRun // replay は、どんなときも何も投稿せず、ロールバックもしない
	abs, _ := filepath.Abs(*out)
	cfg.StorageConfig.DataDir = abs
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cp, err := detect.OpenCP(cfg)
	if err != nil {
		return err
	}
	defer cp.Close()
	u, err := cp.UserByName(ctx, *user)
	if err != nil {
		return fmt.Errorf("user %s: %w", *user, err)
	}
	events, err := cp.EventsFor(ctx, u.ID, t0.Unix(), t1.Unix())
	if err != nil {
		return err
	}
	det := detect.NewDetector(cfg)
	var first *detect.Trigger
	for _, e := range events {
		if t := det.Feed(e); t != nil {
			first = t
			break
		}
	}
	fmt.Printf("events=%d  user=%s uuid=%s\n", len(events), u.Name, u.UUID)
	if first == nil {
		fmt.Println("閾値を超えなかった (検知されない)")
		return nil
	}
	fmt.Printf("検知: %s の %s  %s\n", u.Name, time.Unix(first.Time, 0).In(notify.JST).Format("15:04:05"), first.String())

	st, err := store.OpenStore(abs)
	if err != nil {
		return err
	}
	var sh *capture.Shooter
	if !*noShot {
		sh = capture.NewShooter(cfg)
	}
	g := app.NewGuard(cfg, cp, st, nil, sh, nil, log)
	inc := &store.Incident{
		ID:     fmt.Sprintf("replay-%s-%s", time.Unix(first.Time, 0).In(notify.JST).Format("20060102-150405"), u.Name),
		UserID: u.ID, Name: u.Name, UUID: u.UUID,
		Start: first.Time - int64(first.Window), Last: t1.Unix(), Trigger: first.String(),
	}
	res, err := g.ProcessWindow(ctx, inc, t1.Unix())
	if err != nil {
		return err
	}
	fmt.Println("出力:", res.Dir)
	fmt.Println("-----")
	fmt.Println(res.Report)
	return nil
}

// healthcheck: 監視の 1 回が成功していれば、DataDir/health の更新時刻が新しい。-relay は、コンソールの中継を確かめる。
func cmdHealth(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	useRelay := fs.Bool("relay", false, "コンソールの中継 (console-relay) が答えるかを確かめる")
	_ = fs.Parse(args)
	if *useRelay {
		return relay.RelayHealth()
	}
	cfg, err := loadConfig(log)
	if err != nil {
		return err
	}
	return app.CheckHealthFile(cfg.StorageConfig.DataDir, time.Now())
}
