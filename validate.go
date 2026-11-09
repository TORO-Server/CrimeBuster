package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Validate は設定の検証を1箇所で行う。問題は最初の1件で止めず、全件を集めて返す
// (起動時にまとめて分かるようにするため)。
// 検証の厳しさは「そのモードで実際に使う機能」にだけ必須項目を課す方針で、
// observe / dry-run の既定の設定が通ることはテスト (config_validate_test.go) で固定している。
// モードごとの必須項目は、モードの意味そのものではなく CapabilitiesFor の能力で判定する
// (能力の定義と検証で二重にモードを解釈しないため)。
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	positiveInt := func(name string, v int) {
		if v <= 0 {
			add("%s must be > 0 (got %d)", name, v)
		}
	}
	nonNegativeInt := func(name string, v int) {
		if v < 0 {
			add("%s must be >= 0 (got %d)", name, v)
		}
	}
	positiveDur := func(name string, d time.Duration) {
		if d <= 0 {
			add("%s must be > 0 (got %s)", name, d)
		}
	}
	nonNegativeDur := func(name string, d time.Duration) {
		if d < 0 {
			add("%s must be >= 0 (got %s)", name, d)
		}
	}
	checkURL := func(name, raw string, schemes ...string) {
		if raw == "" {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			add("%s is not a valid URL: %q", name, raw)
			return
		}
		ok := false
		for _, s := range schemes {
			if u.Scheme == s {
				ok = true
			}
		}
		if !ok || u.Host == "" {
			add("%s must be a %s URL with a host: %q", name, strings.Join(schemes, " or "), raw)
		}
	}

	// モード (従来 loadConfig の末尾にあった検査)。
	switch c.Mode {
	case ModeObserve, ModeDryRun, ModeReview, ModeLive:
	default:
		add("CRIMEBUSTER_MODE must be observe, dry-run, review or live: %q", string(c.Mode))
	}
	caps := CapabilitiesFor(c.Mode)

	// DB: ポートは数値で範囲内 (DSN の組み立てで使う)。
	if p, err := strconv.Atoi(c.DBConfig.DBPort); err != nil || p < 1 || p > 65535 {
		add("DB_PORT must be a port number between 1 and 65535: %q", c.DBConfig.DBPort)
	}

	// 検知の閾値と時間窓。0 や負では検知が無効化したり、逆に暴走したりする。
	// PollInterval は time.NewTicker に渡すため、0 以下は panic になる。
	positiveDur("BURST_WINDOW", c.DetectConfig.BurstWindow)
	positiveInt("BREAK_BURST", c.DetectConfig.BreakBurst)
	positiveInt("PLACE_BURST", c.DetectConfig.PlaceBurst)
	positiveDur("SUSTAINED_WINDOW", c.DetectConfig.SustainedWindow)
	positiveInt("SUSTAINED_BREAKS", c.DetectConfig.SustainedBreaks)
	positiveDur("QUIET", c.DetectConfig.Quiet)
	positiveDur("MAX_INCIDENT", c.DetectConfig.MaxIncident)
	positiveDur("POLL_INTERVAL", c.DetectConfig.PollInterval)
	nonNegativeDur("WE_GRACE", c.DetectConfig.WEGrace)
	nonNegativeDur("MAX_EVENT_AGE", c.DetectConfig.MaxEventAge)

	// チャットの閾値。回数の閾値はコード側で「0 ならその検知をしない」扱いのため 0 を許し、負だけを弾く。
	// 時間窓と ChatQuiet は、チャット検知が有効なときに正であること。
	nonNegativeInt("CHAT_RATE", c.ChatConfig.ChatRate)
	nonNegativeInt("CHAT_REPEAT", c.ChatConfig.ChatRepeat)
	nonNegativeInt("CHAT_RUN", c.ChatConfig.ChatRun)
	nonNegativeInt("CHAT_LINK_REPEAT", c.ChatConfig.ChatLinkRepeat)
	if c.ChatConfig.ChatEnabled {
		if c.ChatConfig.ChatRate > 0 {
			positiveDur("CHAT_RATE_WINDOW", c.ChatConfig.ChatRateWindow)
		}
		if c.ChatConfig.ChatRepeat > 0 {
			positiveDur("CHAT_REPEAT_WINDOW", c.ChatConfig.ChatRepeatWindow)
		}
		positiveDur("CHAT_QUIET", c.ChatConfig.ChatQuiet)
	}

	// AI の確からしさの基準は 0〜100。
	if c.AIConfig.AIMinConf < 0 || c.AIConfig.AIMinConf > 100 {
		add("AI_MIN_CONFIDENCE must be between 0 and 100 (got %d)", c.AIConfig.AIMinConf)
	}

	// URL 形式の項目 (空なら使わない、または既定値が入る)。
	checkURL("AI_BASE_URL", c.AIConfig.AIBaseURL, "http", "https")
	checkURL("BLUEMAP_URL", c.ShotConfig.BlueMapURL, "http", "https")
	checkURL("CONSOLE_URL", c.ConsoleConfig.ConsoleURL, "http", "https")
	checkURL("AUTH_INTERNAL_URL", c.ConnectorConfig.AuthInternalURL, "http", "https")
	checkURL("DISCORD_MCP_URL", c.DiscordConfig.DiscordMCPURL, "http", "https")
	checkURL("BROWSER_URL", c.ShotConfig.BrowserURL, "ws", "wss")

	// コネクタの待ち受けアドレスと、許可リストの正規表現。
	if c.ConnectorConfig.ControlAddr != "" {
		_, port, err := net.SplitHostPort(c.ConnectorConfig.ControlAddr)
		if err != nil {
			add("CONTROL_ADDR must be in host:port form: %q", c.ConnectorConfig.ControlAddr)
		} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			add("CONTROL_ADDR must have a port number between 1 and 65535: %q", c.ConnectorConfig.ControlAddr)
		}
		if _, err := regexp.Compile(c.ConnectorConfig.DiscordAllow); err != nil {
			add("CONTROL_DISCORD_ALLOW must be a valid regular expression: %v", err)
		}
	}

	// 撮影を行うモードでは、撮影サイズが正であること。
	if caps.Capture {
		positiveInt("SHOT_WIDTH", c.ShotConfig.ShotWidth)
		positiveInt("SHOT_HEIGHT", c.ShotConfig.ShotHeight)
	}
	nonNegativeDur("SHOT_SETTLE", c.ShotConfig.ShotSettle)
	nonNegativeDur("REPORT_RETENTION", c.RetentionConfig.ReportRetention)

	// 能力に応じた必須項目。
	// コネクタで操作を受けるモードでは、操作できる人のロールが決まらなければ誰も操作できない
	// (空の ADMIN_ROLE_IDS は「全員拒否」が安全側の意味づけのため、気付かずに置かれやすい)。
	if caps.AcceptOps && c.ConnectorConfig.ControlAddr != "" && len(c.ConnectorConfig.AdminRoles) == 0 {
		add("ADMIN_ROLE_IDS is required when the mode accepts operations through the connector (CRIMEBUSTER_MODE=%s)", string(c.Mode))
	}
	// 投稿を行うモード (自動投稿、またはコネクタ経由の操作による投稿) では通報先が要る。
	if (caps.AutoReport || caps.AcceptOps) && c.DiscordConfig.ReportChannelID == "" {
		add("REPORT_CHANNEL_ID is required when the mode can post reports (CRIMEBUSTER_MODE=%s)", string(c.Mode))
	}

	return errors.Join(errs...)
}
