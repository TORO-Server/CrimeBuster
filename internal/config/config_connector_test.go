package config

import (
	"testing"
)

// TestLoadConfigConnectorEnv は、コネクタの環境変数が新しい名前 (CONNECTOR_) だけで読まれ、
// 旧名 (CONTROL_) は読まれないことを確かめる。
func TestLoadConfigConnectorEnv(t *testing.T) {
	// 新しい名前だけ。
	clearConfigEnv(t)
	t.Setenv("CONNECTOR_ADDR", ":9999")
	t.Setenv("CONNECTOR_REQUIRE_LIVE_ROLES", "false")
	t.Setenv("CONNECTOR_DISCORD_ALLOW", "^discord_get")
	cNew, err := LoadConfig()
	if err != nil {
		t.Fatalf("new names must load: %v", err)
	}
	if cNew.ConnectorConfig.Addr != ":9999" || cNew.ConnectorConfig.RequireLiveRoles ||
		cNew.ConnectorConfig.DiscordAllow != "^discord_get" {
		t.Errorf("new names: %+v", cNew.ConnectorConfig)
	}

	// 旧名だけでは、既定値のまま (旧名は読まれない)。
	clearConfigEnv(t)
	t.Setenv("CONTROL_ADDR", ":9999")
	t.Setenv("CONTROL_REQUIRE_LIVE_ROLES", "false")
	t.Setenv("CONTROL_DISCORD_ALLOW", "^discord_get")
	cLegacy, err := LoadConfig()
	if err != nil {
		t.Fatalf("legacy names must load: %v", err)
	}
	if cLegacy.ConnectorConfig.Addr != ":8090" || !cLegacy.ConnectorConfig.RequireLiveRoles ||
		cLegacy.ConnectorConfig.DiscordAllow != DefaultDiscordAllow {
		t.Errorf("legacy names must be ignored: %+v", cLegacy.ConnectorConfig)
	}

	// 両方ある場合は、新しい名前の値になる。
	clearConfigEnv(t)
	t.Setenv("CONNECTOR_ADDR", ":1111")
	t.Setenv("CONTROL_ADDR", ":2222")
	t.Setenv("CONNECTOR_REQUIRE_LIVE_ROLES", "true")
	t.Setenv("CONTROL_REQUIRE_LIVE_ROLES", "false")
	cBoth, err := LoadConfig()
	if err != nil {
		t.Fatalf("both names must load: %v", err)
	}
	if cBoth.ConnectorConfig.Addr != ":1111" || !cBoth.ConnectorConfig.RequireLiveRoles {
		t.Errorf("new names must win: %+v", cBoth.ConnectorConfig)
	}

	// どちらも無ければ、既定値のまま。
	clearConfigEnv(t)
	cDef, err := LoadConfig()
	if err != nil {
		t.Fatalf("defaults must load: %v", err)
	}
	if cDef.ConnectorConfig.Addr != ":8090" || !cDef.ConnectorConfig.RequireLiveRoles ||
		cDef.ConnectorConfig.DiscordAllow != DefaultDiscordAllow {
		t.Errorf("defaults: %+v", cDef.ConnectorConfig)
	}
}
