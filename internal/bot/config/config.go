// Package config loads vikingbot configuration. It mirrors the load
// order used by internal/config (defaults, files, env, flags) but is
// scoped to the bot surface: channels, providers, agent, heartbeat,
// cron, sandbox, observability, ovmount, langfuse, hooks.
//
// The package depends on internal/config.BotConfig for the channel
// definitions (so a single ov.conf can drive both server and bot), and
// adds bot-specific knobs not covered by the server config.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/spf13/viper"
)

// Config is the top-level vikingbot configuration object.
type Config struct {
	// Enabled gates the bot process. When false, `vikingbot start` is a
	// no-op (it logs and exits 0). This mirrors config.BotConfig.Enabled
	// in the server config; the field is duplicated here so the bot can
	// be driven by a standalone config file.
	Enabled bool `mapstructure:"enabled"`

	// ServerURL is the base URL of the ctxhub-server the bot mounts
	// (e.g. https://openviking.example.com). Used by ovmount and
	// heartbeat. Required when Enabled is true.
	ServerURL string `mapstructure:"server_url" validate:"required_if=Enabled true"`

	// Bot identity propagated to the server in X-OpenViking-* headers.
	Account   string `mapstructure:"account" validate:"required_if=Enabled true"`
	User      string `mapstructure:"user"`
	ActorPeer string `mapstructure:"actor_peer"`

	// Channels maps channel name -> channel config. Provider field on
	// each entry selects the adapter (telegram/feishu/dingtalk/slack/
	// qq/websocket).
	Channels map[string]ChannelConfig `mapstructure:"channels"`

	// Provider configures the LLM provider used by the agent loop.
	Provider ProviderConfig `mapstructure:"provider"`

	// Agent configures the agent loop (system prompt, max iterations).
	Agent AgentConfig `mapstructure:"agent"`

	// Heartbeat configures liveness pings to ctxhub-server.
	Heartbeat HeartbeatConfig `mapstructure:"heartbeat"`

	// Cron configures the periodic scheduler.
	Cron CronConfig `mapstructure:"cron"`

	// Sandbox configures the command executor.
	Sandbox SandboxConfig `mapstructure:"sandbox"`

	// OTEL configures bot-side tracing/logging.
	OTEL OTELConfig `mapstructure:"otel"`

	// Ovmount configures the HTTP client to ctxhub-server.
	Ovmount OvmountConfig `mapstructure:"ovmount"`

	// Langfuse configures the optional Langfuse HTTP integration.
	Langfuse LangfuseConfig `mapstructure:"langfuse"`

	// Hooks configures outbound webhook dispatch.
	Hooks HooksConfig `mapstructure:"hooks"`

	// Bus configures the in-process event bus.
	Bus BusConfig `mapstructure:"bus"`
}

// ChannelConfig describes one bot channel. Provider is one of
// telegram, feishu, dingtalk, slack, qq, websocket, discord, email,
// openapi, whatsapp.
type ChannelConfig struct {
	Provider  string `mapstructure:"provider"  validate:"omitempty,oneof=telegram feishu dingtalk slack qq websocket discord email openapi whatsapp"`
	Token     string `mapstructure:"token"`
	AppID     string `mapstructure:"app_id"`
	AppSecret string `mapstructure:"app_secret"`
	// Endpoint is the channel-specific endpoint: the webhook listen
	// address for QQ (e.g. ":8081"), or the API base URL for channels
	// that need an explicit override (otherwise the SDK default applies).
	Endpoint string `mapstructure:"endpoint"`
	// BaseURL is the API base URL for channels that talk HTTP to a
	// platform API (currently QQ: https://api.sgroup.qq.com).
	BaseURL string `mapstructure:"base_url"`
	// WebhookSecret is the HMAC secret used to verify inbound webhook
	// signatures (Slack signing secret, Feishu encrypt key, etc.).
	WebhookSecret string `mapstructure:"webhook_secret"`
	// Mode selects the inbound transport for channels that support more
	// than one. QQ Bot accepts "webhook" (default; QQ calls our /qq/callback
	// endpoint) or "websocket" (we dial QQ's gateway and consume events).
	Mode string `mapstructure:"mode" validate:"omitempty,oneof=webhook websocket"`
	// Enabled gates the channel within `vikingbot start`.
	Enabled bool `mapstructure:"enabled"`
	// Extra captures channel-specific keys not covered by the typed
	// fields above (e.g. email's imap_host/smtp_host, discord's intents,
	// openapi's listen address). The mapstructure ",remain" tag pulls
	// every unmatched key from the source YAML into this map so each
	// adapter can decode its own sub-schema without bloating the shared
	// struct. Use the channelConfig* helpers in internal/bot/channels
	// to read typed values out of Extra.
	Extra map[string]any `mapstructure:",remain"`
}

// ProviderConfig configures the LLM provider.
type ProviderConfig struct {
	// Backend selects the provider implementation. "stub" returns a
	// canned response and is the default for tests. "openai" issues
	// HTTP calls to an OpenAI-compatible endpoint. "vlm" delegates to
	// internal/models/vlm (wired in P12).
	Backend string `mapstructure:"backend" validate:"omitempty,oneof=stub openai vlm"`
	// BaseURL is the OpenAI-compatible endpoint (used when Backend
	// is "openai").
	BaseURL string `mapstructure:"base_url"`
	// APIKey is the bearer token for the LLM provider.
	APIKey string `mapstructure:"api_key"`
	// Model is the model name (e.g. "gpt-4o-mini").
	Model string `mapstructure:"model"`
	// SystemPrompt is prepended to every conversation. When empty a
	// default helpful-assistant prompt is used.
	SystemPrompt string `mapstructure:"system_prompt"`
	// MaxIterations caps the agent loop's tool-call rounds. Zero means 1.
	MaxIterations int `mapstructure:"max_iterations"`
}

// AgentConfig configures the agent loop. Most knobs live under
// Provider; this section is reserved for future per-agent overrides.
type AgentConfig struct {
	// MCPURL is the ctxhub-server MCP endpoint, e.g.
	// https://openviking.example.com/mcp. Required when Enabled is true.
	MCPURL string `mapstructure:"mcp_url" validate:"required_if=Enabled true"`
	// MCPBasicUser / MCPBasicPass are optional HTTP Basic credentials
	// for the MCP endpoint.
	MCPBasicUser string `mapstructure:"mcp_basic_user"`
	MCPBasicPass string `mapstructure:"mcp_basic_pass"`
}

// HeartbeatConfig configures liveness pings.
type HeartbeatConfig struct {
	// Interval is the ping cadence in seconds. Zero disables pings.
	Interval int `mapstructure:"interval"`
	// Path is the server endpoint to POST to (default /api/v1/observer).
	Path string `mapstructure:"path"`
}

// CronConfig configures the periodic scheduler.
type CronConfig struct {
	// Enabled gates the cron loop.
	Enabled bool `mapstructure:"enabled"`
	// Location is the IANA timezone name (default UTC).
	Location string `mapstructure:"location"`
}

// SandboxConfig configures the sandbox executor.
type SandboxConfig struct {
	// Backend is one of "exec" (os/exec), "containerd"
	// (github.com/containerd/containerd/v2), "srt"
	// (@anthropic-ai/sandbox-runtime via Node.js wrapper), or
	// "opensandbox" (HTTP client to opensandbox-server).
	Backend string `mapstructure:"backend" validate:"omitempty,oneof=exec containerd srt opensandbox"`
	// WorkDir is the working directory for executed commands. For srt
	// and opensandbox this is also the host path mounted into the
	// sandbox as the writable workspace.
	WorkDir string `mapstructure:"work_dir"`
	// Timeout is the per-command timeout in seconds.
	Timeout int `mapstructure:"timeout"`
	// SRT holds backend-specific options for Backend="srt".
	SRT SRTConfig `mapstructure:"srt"`
	// OpenSandbox holds backend-specific options for Backend="opensandbox".
	OpenSandbox OpenSandboxConfig `mapstructure:"opensandbox"`
}

// SRTConfig configures the SRT (@anthropic-ai/sandbox-runtime) backend.
// SRT runs a Node.js wrapper process that enforces network/filesystem
// policy via the sandbox-runtime npm package. The wrapper path must
// point at an ES module (.mjs) that reads a JSON settings path from
// argv[1], the workspace path from argv[2], and communicates via
// newline-delimited JSON on stdin/stdout.
type SRTConfig struct {
	// NodePath is the node binary used to launch the wrapper. Defaults
	// to "node" (resolved via PATH).
	NodePath string `mapstructure:"node_path"`
	// WrapperPath is the absolute path to the srt-wrapper.mjs script.
	// Required when Backend="srt".
	WrapperPath string `mapstructure:"wrapper_path" validate:"required_if=Backend srt"`
	// AllowedDomains / DeniedDomains are the network egress lists
	// passed to the wrapper's network policy.
	AllowedDomains []string `mapstructure:"allowed_domains"`
	DeniedDomains  []string `mapstructure:"denied_domains"`
	// AllowLocalBinding permits the sandbox to bind local ports
	// (sandbox-runtime's network.allowLocalBinding).
	AllowLocalBinding bool `mapstructure:"allow_local_binding"`
	// DenyRead / DenyWrite are filesystem deny lists. The workspace
	// directory and /tmp are always added to allowWrite.
	DenyRead  []string `mapstructure:"deny_read"`
	DenyWrite []string `mapstructure:"deny_write"`
}

// OpenSandboxConfig configures the OpenSandbox backend
// (opensandbox-server HTTP API). The server must already be running at
// ServerURL; the backend creates a sandbox per Executor instance and
// tears it down on Close().
type OpenSandboxConfig struct {
	// ServerURL is the base URL of the opensandbox-server
	// (e.g. http://opensandbox-server:8080). Required when
	// Backend="opensandbox".
	ServerURL string `mapstructure:"server_url" validate:"required_if=Backend opensandbox"`
	// APIKey is the bearer token sent in the Authorization header.
	APIKey string `mapstructure:"api_key"`
	// DefaultImage is the OCI image ref used as the sandbox base
	// (e.g. "docker.io/library/busybox:latest"). Required.
	DefaultImage string `mapstructure:"default_image"`
	// RuntimeTimeout is the sandbox wall-clock lifetime in seconds.
	// Defaults to 300 (5 minutes).
	RuntimeTimeout int `mapstructure:"runtime_timeout"`
}

// OTELConfig configures bot-side tracing/logging.
type OTELConfig struct {
	Exporter    string  `mapstructure:"exporter"   validate:"omitempty,oneof=memory otlp"`
	ServiceName string  `mapstructure:"service_name"`
	SampleRate  float64 `mapstructure:"sample_rate" validate:"gte=0,lte=1"`
	LogLevel    string  `mapstructure:"log_level"   validate:"omitempty,oneof=debug info warn warning error"`
}

// OvmountConfig configures the HTTP client to ctxhub-server.
type OvmountConfig struct {
	// Timeout is the HTTP client timeout in seconds.
	Timeout int `mapstructure:"timeout"`
	// APIKey is the bearer token for the server.
	APIKey string `mapstructure:"api_key"`
}

// LangfuseConfig configures the optional Langfuse HTTP integration.
type LangfuseConfig struct {
	// Enabled gates the integration.
	Enabled bool `mapstructure:"enabled"`
	// BaseURL is the Langfuse ingest endpoint (default https://cloud.langfuse.com).
	BaseURL string `mapstructure:"base_url"`
	// PublicKey / SecretKey are the project credentials.
	PublicKey string `mapstructure:"public_key"`
	SecretKey string `mapstructure:"secret_key"`
	// FlushInterval is the batch flush cadence in seconds.
	FlushInterval int `mapstructure:"flush_interval"`
}

// HooksConfig configures outbound webhook dispatch.
type HooksConfig struct {
	// URLs are the HTTP endpoints to POST events to.
	URLs []string `mapstructure:"urls"`
	// Timeout is the per-request timeout in seconds.
	Timeout int `mapstructure:"timeout"`
}

// BusConfig configures the in-process event bus.
type BusConfig struct {
	// QueueSize is the buffered channel size for the in-memory bus.
	QueueSize int `mapstructure:"queue_size"`
}

// Load resolves configuration from defaults, files, env, and flags.
//
// path is an optional override pointing at a single vikingbot.conf file.
// When non-empty it is consulted in addition to the standard search
// order (system, user, $VIKINGBOT_CONFIG_PATH); env vars (prefix
// VIKINGBOT_) still apply on top.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetEnvPrefix("VIKINGBOT")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	setDefaults(v)

	for _, p := range candidatePaths(path) {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		v.SetConfigFile(p)
		// Force YAML; the canonical vikingbot config uses the .conf
		// extension which viper cannot infer a type from.
		v.SetConfigType("yaml")
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("bot config: read %s: %w", p, err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("bot config: unmarshal: %w", err)
	}
	if err := validator.New().Struct(&cfg); err != nil {
		return nil, fmt.Errorf("bot config: validate: %w", err)
	}
	return &cfg, nil
}

func candidatePaths(override string) []string {
	paths := []string{"/etc/openviking/vikingbot.conf"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "openviking", "vikingbot.conf"))
	}
	paths = append(paths, os.Getenv("VIKINGBOT_CONFIG_PATH"))
	if override != "" {
		paths = append(paths, override)
	}
	return paths
}
