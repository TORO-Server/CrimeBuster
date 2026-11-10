package config

import (
	"strings"
	"testing"
	"time"
)

// configEnvNames は loadConfig が読む環境変数の全て。既定値のテストで空にするために使う。
var configEnvNames = []string{
	"CRIMEBUSTER_MODE", "DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_PASSWORD_FILE", "DB_NAME", "DB_TABLE_PREFIX",
	"BURST_WINDOW", "BREAK_BURST", "PLACE_BURST", "SUSTAINED_WINDOW", "SUSTAINED_BREAKS", "QUIET", "MAX_INCIDENT", "WE_GRACE",
	"TRUSTED_PLAYERS", "OPS_FILE", "POLL_INTERVAL", "MAX_EVENT_AGE",
	"AI_API_KEY", "AI_API_KEY_FILE", "ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY_FILE", "AI_BASE_URL", "AI_MODEL", "AI_MIN_CONFIDENCE", "AI_REQUIRED", "AI_CHAT",
	"CHAT_ENABLED", "CHAT_RATE", "CHAT_RATE_WINDOW", "CHAT_REPEAT", "CHAT_REPEAT_WINDOW", "CHAT_RUN", "CHAT_LINK_REPEAT", "CHAT_BAD_WORDS_FILE", "CHAT_QUIET", "CHAT_MUTE_CMD", "CHAT_MUTE_TAG_ID",
	"DISCORD_BOT_TOKEN", "DISCORD_BOT_TOKEN_FILE", "DISCORDSRV_CONFIG", "REPORT_CHANNEL_ID", "ROLLBACK_TAG_ID", "DISCORD_MCP_URL", "DISCORD_MCP_LAZY", "MCP_PROTOCOL_VERSION",
	"CONSOLE_URL", "KICK_ENABLED", "BLUEMAP_URL", "BROWSER_URL", "BLUEMAP_RESOLVE", "MAP_IDS", "CHROME_PATH", "SHOT_WIDTH", "SHOT_HEIGHT", "SHOT_SETTLE",
	"CONNECTOR_ADDR", "AUTH_INTERNAL_URL", "ADMIN_ROLE_IDS", "CONNECTOR_REQUIRE_LIVE_ROLES", "CONNECTOR_DISCORD_ALLOW", "CONTROL_ADDR", "CONTROL_REQUIRE_LIVE_ROLES", "CONTROL_DISCORD_ALLOW", "WARPS_DIR", "DATA_DIR", "REPORT_RETENTION",
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, n := range configEnvNames {
		t.Setenv(n, "")
	}
}

// validConfig は、検証を通る設定をモードごとに返す (値は loadConfig の既定値に合わせる)。
func validConfig(mode Mode) *Config {
	c := &Config{
		Mode: mode,
		DBConfig: DBConfig{
			DBHost: "tidb", DBPort: "4000", DBUser: "root", DBName: "coreprotect", TablePrefix: "coreprotect_",
		},
		DetectConfig: DetectConfig{
			BurstWindow: 5 * time.Second, BreakBurst: 100, PlaceBurst: 150,
			SustainedWindow: 10 * time.Minute, SustainedBreaks: 1500,
			Quiet: 90 * time.Second, MaxIncident: 10 * time.Minute, WEGrace: 30 * time.Minute,
			PollInterval: 3 * time.Second, MaxEventAge: 15 * time.Minute,
		},
		ChatConfig: ChatConfig{
			ChatEnabled: true, ChatRate: 12, ChatRateWindow: 10 * time.Second,
			ChatRepeat: 5, ChatRepeatWindow: 60 * time.Second, ChatRun: 30, ChatLinkRepeat: 3,
			ChatQuiet: 30 * time.Second,
		},
		AIConfig: AIConfig{
			AIBaseURL: "https://api.anthropic.com", AIModel: "claude-haiku-5-5", AIMinConf: 70, AIChat: true,
		},
		DiscordConfig: DiscordConfig{
			DiscordSRVConfig: "/discordsrv/config.yml",
			DiscordMCPURL:    "http://discord-mcp:8085/mcp", DiscordMCPLazy: true, MCPProtocol: "2025-11-25",
		},
		ConsoleConfig: ConsoleConfig{ConsoleURL: "http://toro-console:8091"},
		ShotConfig: ShotConfig{
			BlueMapURL: "http://3d-map.torosaba.net", ShotWidth: 1280, ShotHeight: 720, ShotSettle: 60 * time.Second,
		},
		ConnectorConfig: ConnectorConfig{
			Addr: ":8090", AuthInternalURL: "http://toro-auth:8081",
			RequireLiveRoles: true, DiscordAllow: DefaultDiscordAllow,
		},
		StorageConfig:   StorageConfig{DataDir: "/data"},
		RetentionConfig: RetentionConfig{ReportRetention: 90 * 24 * time.Hour},
	}
	if caps := CapabilitiesFor(mode); caps.AcceptOps || caps.AutoReport {
		c.ConnectorConfig.AdminRoles = []string{"111111111111111111"}
		c.DiscordConfig.ReportChannelID = "222222222222222222"
	}
	return c
}

func TestValidateAcceptsValidConfigs(t *testing.T) {
	for _, mode := range []Mode{ModeObserve, ModeDryRun, ModeReview, ModeLive} {
		if err := validConfig(mode).Validate(); err != nil {
			t.Errorf("mode %s: valid config must pass: %v", mode, err)
		}
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"mode", func(c *Config) { c.Mode = "nope" }, "CRIMEBUSTER_MODE"},
		{"db port text", func(c *Config) { c.DBConfig.DBPort = "abc" }, "DB_PORT"},
		{"db port zero", func(c *Config) { c.DBConfig.DBPort = "0" }, "DB_PORT"},
		{"db port too large", func(c *Config) { c.DBConfig.DBPort = "70000" }, "DB_PORT"},
		{"burst window", func(c *Config) { c.DetectConfig.BurstWindow = 0 }, "BURST_WINDOW"},
		{"break burst", func(c *Config) { c.DetectConfig.BreakBurst = 0 }, "BREAK_BURST"},
		{"place burst", func(c *Config) { c.DetectConfig.PlaceBurst = -1 }, "PLACE_BURST"},
		{"sustained window", func(c *Config) { c.DetectConfig.SustainedWindow = -time.Second }, "SUSTAINED_WINDOW"},
		{"sustained breaks", func(c *Config) { c.DetectConfig.SustainedBreaks = 0 }, "SUSTAINED_BREAKS"},
		{"quiet", func(c *Config) { c.DetectConfig.Quiet = 0 }, "QUIET"},
		{"max incident", func(c *Config) { c.DetectConfig.MaxIncident = 0 }, "MAX_INCIDENT"},
		{"poll interval", func(c *Config) { c.DetectConfig.PollInterval = 0 }, "POLL_INTERVAL"},
		{"we grace", func(c *Config) { c.DetectConfig.WEGrace = -time.Minute }, "WE_GRACE"},
		{"max event age", func(c *Config) { c.DetectConfig.MaxEventAge = -time.Minute }, "MAX_EVENT_AGE"},
		{"chat rate", func(c *Config) { c.ChatConfig.ChatRate = -1 }, "CHAT_RATE"},
		{"chat rate window", func(c *Config) { c.ChatConfig.ChatRateWindow = 0 }, "CHAT_RATE_WINDOW"},
		{"chat repeat window", func(c *Config) { c.ChatConfig.ChatRepeatWindow = 0 }, "CHAT_REPEAT_WINDOW"},
		{"chat quiet", func(c *Config) { c.ChatConfig.ChatQuiet = 0 }, "CHAT_QUIET"},
		{"ai min confidence high", func(c *Config) { c.AIConfig.AIMinConf = 101 }, "AI_MIN_CONFIDENCE"},
		{"ai min confidence low", func(c *Config) { c.AIConfig.AIMinConf = -1 }, "AI_MIN_CONFIDENCE"},
		{"ai base url", func(c *Config) { c.AIConfig.AIBaseURL = "example.com" }, "AI_BASE_URL"},
		{"bluemap url", func(c *Config) { c.ShotConfig.BlueMapURL = "ftp://example.com" }, "BLUEMAP_URL"},
		{"console url", func(c *Config) { c.ConsoleConfig.ConsoleURL = "://bad" }, "CONSOLE_URL"},
		{"auth url", func(c *Config) { c.ConnectorConfig.AuthInternalURL = "toro-auth:8081" }, "AUTH_INTERNAL_URL"},
		{"discord mcp url", func(c *Config) { c.DiscordConfig.DiscordMCPURL = "ws://example.com" }, "DISCORD_MCP_URL"},
		{"browser url", func(c *Config) { c.ShotConfig.BrowserURL = "http://example.com" }, "BROWSER_URL"},
		{"connector addr no port", func(c *Config) { c.ConnectorConfig.Addr = "8090" }, "CONNECTOR_ADDR"},
		{"connector addr port zero", func(c *Config) { c.ConnectorConfig.Addr = ":0" }, "CONNECTOR_ADDR"},
		{"discord allow", func(c *Config) { c.ConnectorConfig.DiscordAllow = "[invalid" }, "CONNECTOR_DISCORD_ALLOW"},
		{"shot settle", func(c *Config) { c.ShotConfig.ShotSettle = -time.Second }, "SHOT_SETTLE"},
		{"report retention", func(c *Config) { c.RetentionConfig.ReportRetention = -time.Hour }, "REPORT_RETENTION"},
	}
	for _, tc := range cases {
		c := validConfig(ModeObserve)
		tc.mutate(c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestValidateCapabilityRequirements(t *testing.T) {
	// 撮影サイズは、撮影するモード (Capture) でのみ必須。
	c := validConfig(ModeObserve)
	c.ShotConfig.ShotWidth = 0
	if err := c.Validate(); err != nil {
		t.Errorf("observe must not require shot size: %v", err)
	}
	c = validConfig(ModeDryRun)
	c.ShotConfig.ShotWidth = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SHOT_WIDTH") {
		t.Errorf("dry-run must require SHOT_WIDTH: %v", err)
	}
	c = validConfig(ModeLive)
	c.ShotConfig.ShotHeight = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SHOT_HEIGHT") {
		t.Errorf("live must require SHOT_HEIGHT: %v", err)
	}

	// 操作を受けるモードでは ADMIN_ROLE_IDS が必須。コネクタを開かなければ不要。
	for _, mode := range []Mode{ModeReview, ModeLive} {
		c = validConfig(mode)
		c.ConnectorConfig.AdminRoles = nil
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ADMIN_ROLE_IDS") {
			t.Errorf("%s must require ADMIN_ROLE_IDS: %v", mode, err)
		}
		c = validConfig(mode)
		c.ConnectorConfig.AdminRoles = nil
		c.ConnectorConfig.Addr = ""
		if err := c.Validate(); err != nil {
			t.Errorf("%s without the connector must not require ADMIN_ROLE_IDS: %v", mode, err)
		}
	}

	// 投稿を行うモード (review の操作経由、live の自動) では REPORT_CHANNEL_ID が必須。
	for _, mode := range []Mode{ModeReview, ModeLive} {
		c = validConfig(mode)
		c.DiscordConfig.ReportChannelID = ""
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "REPORT_CHANNEL_ID") {
			t.Errorf("%s must require REPORT_CHANNEL_ID: %v", mode, err)
		}
	}
	for _, mode := range []Mode{ModeObserve, ModeDryRun} {
		c = validConfig(mode)
		if err := c.Validate(); err != nil {
			t.Errorf("%s must not require posting settings: %v", mode, err)
		}
	}
}

func TestValidateAggregatesErrors(t *testing.T) {
	c := validConfig(ModeReview)
	c.DetectConfig.BreakBurst = 0
	c.DBConfig.DBPort = "x"
	c.AIConfig.AIMinConf = 200
	c.ConnectorConfig.AdminRoles = nil
	c.DiscordConfig.ReportChannelID = ""
	err := c.Validate()
	if err == nil {
		t.Fatal("must fail")
	}
	for _, want := range []string{"BREAK_BURST", "DB_PORT", "AI_MIN_CONFIDENCE", "ADMIN_ROLE_IDS", "REPORT_CHANNEL_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error must contain %q: %v", want, err)
		}
	}
	if n := strings.Count(err.Error(), "\n"); n < 4 {
		t.Errorf("errors must be joined into one error with all items (got %d lines): %v", n+1, err)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearConfigEnv(t)
	c, err := LoadConfig()
	if err != nil {
		t.Fatalf("defaults must load: %v", err)
	}
	if c.Mode != ModeObserve {
		t.Errorf("Mode = %q", c.Mode)
	}
	if c.DBConfig.DBHost != "tidb" || c.DBConfig.DBPort != "4000" || c.DBConfig.DBUser != "root" ||
		c.DBConfig.DBName != "coreprotect" || c.DBConfig.TablePrefix != "coreprotect_" {
		t.Errorf("DB defaults: %+v", c.DBConfig)
	}
	if c.DetectConfig.BreakBurst != 100 || c.DetectConfig.PlaceBurst != 150 || c.DetectConfig.SustainedBreaks != 1500 ||
		c.DetectConfig.BurstWindow != 5*time.Second || c.DetectConfig.PollInterval != 3*time.Second {
		t.Errorf("detect defaults: %+v", c.DetectConfig)
	}
	if !c.ChatConfig.ChatEnabled || c.ChatConfig.ChatRate != 12 || c.ChatConfig.ChatRepeat != 5 || c.ChatConfig.ChatRun != 30 {
		t.Errorf("chat defaults: %+v", c.ChatConfig)
	}
	if c.AIConfig.AIBaseURL != "https://api.anthropic.com" || c.AIConfig.AIModel != "claude-haiku-5-5" || c.AIConfig.AIMinConf != 70 {
		t.Errorf("AI defaults: %+v", c.AIConfig)
	}
	if c.DiscordConfig.ReportChannelID != "" || c.DiscordConfig.RollbackTagID != "" ||
		c.DiscordConfig.DiscordMCPURL != "http://discord-mcp:8085/mcp" || !c.DiscordConfig.DiscordMCPLazy {
		t.Errorf("discord defaults: %+v", c.DiscordConfig)
	}
	if c.ConsoleConfig.ConsoleURL != "http://toro-console:8091" {
		t.Errorf("console defaults: %+v", c.ConsoleConfig)
	}
	if c.ShotConfig.ShotWidth != 1280 || c.ShotConfig.ShotHeight != 720 || c.ShotConfig.MapIDs["main"] != "overworld" {
		t.Errorf("shot defaults: %+v", c.ShotConfig)
	}
	if c.ConnectorConfig.Addr != ":8090" || len(c.ConnectorConfig.AdminRoles) != 0 || !c.ConnectorConfig.RequireLiveRoles {
		t.Errorf("connector defaults: %+v", c.ConnectorConfig)
	}
	if c.StorageConfig.DataDir != "/data" || c.RetentionConfig.ReportRetention != 90*24*time.Hour {
		t.Errorf("storage defaults: %q %s", c.StorageConfig.DataDir, c.RetentionConfig.ReportRetention)
	}
}

func TestLoadConfigCapabilityRequirements(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRIMEBUSTER_MODE", "review")
	if _, err := LoadConfig(); err == nil ||
		!strings.Contains(err.Error(), "ADMIN_ROLE_IDS") || !strings.Contains(err.Error(), "REPORT_CHANNEL_ID") {
		t.Fatalf("review without IDs must fail with both items: %v", err)
	}
	t.Setenv("ADMIN_ROLE_IDS", "111111111111111111")
	t.Setenv("REPORT_CHANNEL_ID", "222222222222222222")
	c, err := LoadConfig()
	if err != nil || c.Mode != ModeReview {
		t.Fatalf("review with IDs must load: %v %v", c, err)
	}
}
