package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// Ensure no env override leaks from the host.
	t.Setenv("VIKINGBOT_CONFIG_PATH", "")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Enabled {
		t.Errorf("Enabled default should be false")
	}
	if cfg.Provider.Backend != "stub" {
		t.Errorf("Provider.Backend default = %q, want stub", cfg.Provider.Backend)
	}
	if cfg.OTEL.ServiceName != "vikingbot" {
		t.Errorf("OTEL.ServiceName default = %q, want vikingbot", cfg.OTEL.ServiceName)
	}
	if cfg.Heartbeat.Interval != 30 {
		t.Errorf("Heartbeat.Interval default = %d, want 30", cfg.Heartbeat.Interval)
	}
	if cfg.Heartbeat.Path != "/api/v1/observer" {
		t.Errorf("Heartbeat.Path default = %q, want /api/v1/observer", cfg.Heartbeat.Path)
	}
	if cfg.Sandbox.Backend != "exec" {
		t.Errorf("Sandbox.Backend default = %q, want exec", cfg.Sandbox.Backend)
	}
	if cfg.Bus.QueueSize != 1024 {
		t.Errorf("Bus.QueueSize default = %d, want 1024", cfg.Bus.QueueSize)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vikingbot.conf")
	const yaml = `enabled: true
server_url: https://openviking.example.com
account: acme
user: bot
actor_peer: vikingbot
channels:
  telegram:
    provider: telegram
    token: "123:abc"
    enabled: true
provider:
  backend: openai
  base_url: https://api.openai.com/v1
  api_key: sk-test
  model: gpt-4o-mini
agent:
  mcp_url: https://openviking.example.com/mcp
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Enabled {
		t.Errorf("Enabled = false, want true")
	}
	if cfg.ServerURL != "https://openviking.example.com" {
		t.Errorf("ServerURL = %q", cfg.ServerURL)
	}
	if cfg.Channels["telegram"].Token != "123:abc" {
		t.Errorf("telegram token = %q", cfg.Channels["telegram"].Token)
	}
	if cfg.Provider.Backend != "openai" {
		t.Errorf("Provider.Backend = %q", cfg.Provider.Backend)
	}
	if cfg.Agent.MCPURL != "https://openviking.example.com/mcp" {
		t.Errorf("Agent.MCPURL = %q", cfg.Agent.MCPURL)
	}
}

func TestLoadValidationFailsWhenEnabledWithoutServerURL(t *testing.T) {
	t.Setenv("VIKINGBOT_CONFIG_PATH", "")
	t.Setenv("VIKINGBOT_ENABLED", "true")
	// ServerURL is empty -> validation should fail.
	_, err := Load("")
	if err == nil {
		t.Fatalf("Load should have failed validation")
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("VIKINGBOT_ENABLED", "true")
	t.Setenv("VIKINGBOT_SERVER_URL", "https://env.example.com")
	t.Setenv("VIKINGBOT_ACCOUNT", "env-acct")
	t.Setenv("VIKINGBOT_AGENT_MCP_URL", "https://env.example.com/mcp")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ServerURL != "https://env.example.com" {
		t.Errorf("ServerURL = %q, want https://env.example.com", cfg.ServerURL)
	}
	if cfg.Account != "env-acct" {
		t.Errorf("Account = %q, want env-acct", cfg.Account)
	}
}
