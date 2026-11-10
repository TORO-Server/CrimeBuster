package config

import (
	"slices"
	"testing"
)

// TestLoadConfigConnectorEnvCompat は、コネクタの環境変数が、新しい名前 (CONNECTOR_) を優先し、
// 旧名 (CONTROL_) だけでも同じ結果になり、両方ある場合は新しい名前が優先することを確かめる。
func TestLoadConfigConnectorEnvCompat(t *testing.T) {
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
	if names := cNew.LegacyEnvNames(); len(names) != 0 {
		t.Errorf("new names must not be recorded as legacy: %v", names)
	}

	// 旧名だけでも、読み取り結果は同じになり、旧名で読んだことが記録される。
	clearConfigEnv(t)
	t.Setenv("CONTROL_ADDR", ":9999")
	t.Setenv("CONTROL_REQUIRE_LIVE_ROLES", "false")
	t.Setenv("CONTROL_DISCORD_ALLOW", "^discord_get")
	cLegacy, err := LoadConfig()
	if err != nil {
		t.Fatalf("legacy names must load: %v", err)
	}
	if cLegacy.ConnectorConfig.Addr != cNew.ConnectorConfig.Addr ||
		cLegacy.ConnectorConfig.RequireLiveRoles != cNew.ConnectorConfig.RequireLiveRoles ||
		cLegacy.ConnectorConfig.DiscordAllow != cNew.ConnectorConfig.DiscordAllow {
		t.Errorf("legacy names must give the same config: %+v vs %+v", cLegacy.ConnectorConfig, cNew.ConnectorConfig)
	}
	want := []string{"CONTROL_ADDR", "CONTROL_REQUIRE_LIVE_ROLES", "CONTROL_DISCORD_ALLOW"}
	if names := cLegacy.LegacyEnvNames(); !slices.Equal(names, want) {
		t.Errorf("legacy names read: got %v, want %v", names, want)
	}

	// 両方ある場合は、新しい名前が優先し、旧名は読んだことにしない。
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
	if names := cBoth.LegacyEnvNames(); len(names) != 0 {
		t.Errorf("legacy names must not be recorded when new names win: %v", names)
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
	if names := cDef.LegacyEnvNames(); len(names) != 0 {
		t.Errorf("defaults must not record legacy names: %v", names)
	}
}
