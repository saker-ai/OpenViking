// Package cli - CLI runtime configuration.
//
// CLIConfig is the loaded ~/.ov/config.yaml plus env vars. It is kept
// separate from the server's internal/config.Config because the CLI
// only needs the connection + auth sections.
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// CLIConfig is the ov CLI's runtime configuration. It mirrors the
// sections of ~/.ov/config.yaml that the CLI consumes.
type CLIConfig struct {
	Server   CLIConfigServer   `mapstructure:"server"`
	Account  CLIConfigAccount  `mapstructure:"account"`
	Auth     CLIConfigAuth     `mapstructure:"auth"`
	Output   CLIConfigOutput   `mapstructure:"output"`
	Defaults CLIConfigDefaults `mapstructure:"defaults"`
}

// CLIConfigServer identifies the OpenViking server endpoint.
type CLIConfigServer struct {
	BaseURL string `mapstructure:"base_url"`
	Timeout int    `mapstructure:"timeout"` // seconds; 0 = default 30
}

// CLIConfigAccount holds the default account/user identity headers.
type CLIConfigAccount struct {
	Name string `mapstructure:"name"` // X-OpenViking-Account (default: "default")
	User string `mapstructure:"user"` // X-OpenViking-User
}

// CLIConfigAuth holds OAuth client_credentials parameters used by the
// `ov` CLI to acquire a bearer token via POST /api/v1/oauth/token.
type CLIConfigAuth struct {
	ClientID     string   `mapstructure:"client_id"`
	ClientSecret string   `mapstructure:"client_secret"`
	TokenURL     string   `mapstructure:"token_url"` // override; defaults to <base>/api/v1/oauth/token
	Scopes       []string `mapstructure:"scopes"`
}

// CLIConfigOutput configures CLI output (format + locale).
type CLIConfigOutput struct {
	Format string `mapstructure:"format"` // "table" (default) | "json" | "yaml"
	Locale string `mapstructure:"locale"` // e.g. "en", "zh-CN"
}

// CLIConfigDefaults holds default values for commands that accept args.
type CLIConfigDefaults struct {
	SearchLimit int `mapstructure:"search_limit"`
	ListLimit   int `mapstructure:"list_limit"`
}

// DefaultBaseURL is used when no config is present.
const DefaultBaseURL = "http://127.0.0.1:8000"

// LoadConfig reads the CLI config from the search path:
//
//  1. /etc/openviking/ov.yaml
//  2. ~/.ov/config.yaml
//  3. $OV_CONFIG_PATH
//  4. Environment variables (prefix OV_, dots -> underscores)
//
// Missing files are not an error: a zero Config is returned and the
// caller falls back to defaults.
func LoadConfig() (*CLIConfig, error) {
	v := viper.New()
	v.SetEnvPrefix("OV")
	v.SetEnvKeyReplacer(replacer())
	v.AutomaticEnv()
	v.SetConfigType("yaml")

	setDefaults(v)

	// Search paths. Empty result is fine.
	paths := []string{"/etc/openviking/ov.yaml"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".ov", "config.yaml"))
	}
	if env := os.Getenv("OV_CONFIG_PATH"); env != "" {
		paths = append(paths, env)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		v.SetConfigFile(p)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("cli: read %s: %w", p, err)
		}
		break
	}

	cfg := &CLIConfig{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("cli: decode config: %w", err)
	}
	applyDefaults(cfg)
	return cfg, nil
}

// ConfigDir returns the user's CLI config directory (~/.ov) and creates
// it when missing. Used by auth (credentials cache) and wizard.
func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cli: home dir: %w", err)
	}
	dir := filepath.Join(home, ".ov")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cli: mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// CredentialsPath returns the path to the cached OAuth credentials file.
func CredentialsPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// ConfigPath returns the path to the CLI config file (~/.ov/config.yaml).
// The file may not exist yet; callers should handle os.IsNotExist.
func ConfigPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// SaveConfig writes the YAML representation of cfg to ~/.ov/config.yaml
// with mode 0o600. Used by the wizard.
func SaveConfig(cfg *CLIConfig) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	v := viper.New()
	v.SetConfigType("yaml")
	v.Set("server", cfg.Server)
	v.Set("account", cfg.Account)
	v.Set("auth", cfg.Auth)
	v.Set("output", cfg.Output)
	v.Set("defaults", cfg.Defaults)
	path := filepath.Join(dir, "config.yaml")
	if err := v.SafeWriteConfigAs(path); err != nil {
		// SafeWriteConfigAs refuses to overwrite; for the wizard we
		// replace the existing file when it already exists.
		if _, ok := err.(viper.ConfigFileAlreadyExistsError); ok {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("cli: remove stale config: %w", err)
			}
			if err := v.WriteConfigAs(path); err != nil {
				return fmt.Errorf("cli: write config: %w", err)
			}
		} else {
			return fmt.Errorf("cli: write config: %w", err)
		}
	}
	// Restrict permissions on the file (Viper creates with 0o644).
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("cli: chmod config: %w", err)
	}
	return nil
}

// setDefaults populates Viper defaults before any file is loaded.
func setDefaults(v *viper.Viper) {
	v.SetDefault("server.base_url", DefaultBaseURL)
	v.SetDefault("server.timeout", 30)
	v.SetDefault("account.name", "default")
	v.SetDefault("output.format", "table")
	v.SetDefault("output.locale", "")
	v.SetDefault("defaults.search_limit", 10)
	v.SetDefault("defaults.list_limit", 50)
}

// applyDefaults re-applies defaults to fields left empty after decode,
// since viper.Unmarshal may zero them out.
func applyDefaults(cfg *CLIConfig) {
	if cfg.Server.BaseURL == "" {
		cfg.Server.BaseURL = DefaultBaseURL
	}
	if cfg.Server.Timeout == 0 {
		cfg.Server.Timeout = 30
	}
	if cfg.Account.Name == "" {
		cfg.Account.Name = "default"
	}
	if cfg.Output.Format == "" {
		cfg.Output.Format = "table"
	}
	if cfg.Defaults.SearchLimit == 0 {
		cfg.Defaults.SearchLimit = 10
	}
	if cfg.Defaults.ListLimit == 0 {
		cfg.Defaults.ListLimit = 50
	}
}
