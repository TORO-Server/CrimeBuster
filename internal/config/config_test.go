package config

import (
	"crime-buster/internal/testutil"
	"path/filepath"
	"testing"
)

func TestConfigMode(t *testing.T) {
	t.Setenv("CRIMEBUSTER_MODE", "nope")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("bad mode must fail")
	}
	t.Setenv("CRIMEBUSTER_MODE", "")
	c, err := LoadConfig()
	if err != nil || c.Mode != ModeObserve {
		t.Fatalf("default must be observe: %v %v", c, err)
	}
}

func TestConfigSecretsFromFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) string {
		p := filepath.Join(dir, name)
		if err := testutil.WriteFile(p, value); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("CRIMEBUSTER_MODE", "")
	t.Setenv("DB_PASSWORD", "from-env")
	t.Setenv("AI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "key-env")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	c, err := LoadConfig()
	if err != nil || c.DBPassword != "from-env" || c.AIKey != "key-env" || c.BotToken != "" {
		t.Fatalf("env: %+v %v", c, err)
	}
	t.Setenv("DB_PASSWORD_FILE", write("db", "from-file\n"))
	t.Setenv("AI_API_KEY_FILE", write("ai", "key-file\n"))
	t.Setenv("DISCORD_BOT_TOKEN_FILE", write("bot", "bot-file\n"))
	c, err = LoadConfig()
	if err != nil || c.DBPassword != "from-file" || c.AIKey != "key-file" || c.BotToken != "bot-file" {
		t.Fatalf("file: %+v %v", c, err)
	}
	// ファイルが読めないのに、空のまま動かない
	t.Setenv("DB_PASSWORD_FILE", filepath.Join(dir, "none"))
	if _, err := LoadConfig(); err == nil {
		t.Fatal("読めない秘密のファイルは、エラー")
	}
}
