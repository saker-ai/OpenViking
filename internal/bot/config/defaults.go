package config

import "github.com/spf13/viper"

// setDefaults installs the built-in default values for every bot config key.
func setDefaults(v *viper.Viper) {
	v.SetDefault("enabled", false)
	v.SetDefault("server_url", "")
	v.SetDefault("account", "")
	v.SetDefault("user", "")
	v.SetDefault("actor_peer", "")

	v.SetDefault("provider.backend", "stub")
	v.SetDefault("provider.base_url", "")
	v.SetDefault("provider.api_key", "")
	v.SetDefault("provider.model", "gpt-4o-mini")
	v.SetDefault("provider.system_prompt", defaultSystemPrompt)
	v.SetDefault("provider.max_iterations", 1)

	v.SetDefault("agent.mcp_url", "")
	v.SetDefault("agent.mcp_basic_user", "")
	v.SetDefault("agent.mcp_basic_pass", "")

	v.SetDefault("heartbeat.interval", 30)
	v.SetDefault("heartbeat.path", "/api/v1/observer")

	v.SetDefault("cron.enabled", false)
	v.SetDefault("cron.location", "UTC")

	v.SetDefault("sandbox.backend", "exec")
	v.SetDefault("sandbox.work_dir", "/tmp/vikingbot-sandbox")
	v.SetDefault("sandbox.timeout", 30)

	v.SetDefault("otel.exporter", "memory")
	v.SetDefault("otel.service_name", "vikingbot")
	v.SetDefault("otel.sample_rate", 1.0)
	v.SetDefault("otel.log_level", "info")

	v.SetDefault("ovmount.timeout", 30)
	v.SetDefault("ovmount.api_key", "")

	v.SetDefault("langfuse.enabled", false)
	v.SetDefault("langfuse.base_url", "https://cloud.langfuse.com")
	v.SetDefault("langfuse.public_key", "")
	v.SetDefault("langfuse.secret_key", "")
	v.SetDefault("langfuse.flush_interval", 10)

	v.SetDefault("hooks.timeout", 10)

	v.SetDefault("bus.queue_size", 1024)
}

// defaultSystemPrompt is prepended to every agent conversation when the
// user does not provide one. It is intentionally short — the OpenViking
// MCP tools describe themselves, so the prompt just sets the persona.
const defaultSystemPrompt = `You are vikingbot, the OpenViking multi-channel assistant.
You answer user questions concisely and call MCP tools when needed to
read, search, or remember information on the OpenViking mount.`
