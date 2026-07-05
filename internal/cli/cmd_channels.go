package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	botconfig "github.com/saker-ai/ctxhub/internal/bot/config"
)

// ChannelsCmd builds `ov channels` with the `login` subcommand. Other
// channel management subcommands (status, list) live in the bot CLI;
// `ov channels` here is scoped to OAuth login flows.
func ChannelsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channels",
		Short: "Manage channel OAuth credentials",
	}
	cmd.AddCommand(channelsLoginCmd(rt))
	return cmd
}

// supportedChannels is the set of channel names that `ov channels login`
// accepts. It mirrors the providers wired in internal/bot/channels.
var supportedChannels = map[string]struct{}{
	"feishu":    {},
	"slack":     {},
	"dingtalk":  {},
	"telegram":  {},
}

// ErrUnsupportedChannel is returned when the channel name is not in
// supportedChannels.
var ErrUnsupportedChannel = errors.New("cli: unsupported channel")

// ChannelTokensDir returns the directory where per-channel OAuth
// refresh tokens are stored. It honours $OV_CHANNEL_TOKENS_DIR and
// falls back to ~/.openviking/channels.
func ChannelTokensDir() (string, error) {
	if v := os.Getenv("OV_CHANNEL_TOKENS_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cli: home dir: %w", err)
	}
	return filepath.Join(home, ".openviking", "channels"), nil
}

// OAuthURLBuilder constructs the authorization URL the user must visit
// to start the OAuth flow for the given channel. The builder reads
// channel credentials from the bot config when available.
type OAuthURLBuilder interface {
	Build(ctx context.Context, channel, configPath string) (string, error)
}

// channelsLoginCmd builds the `ov channels login <channel>` subcommand.
// It prints the OAuth URL the user should visit; the actual token
// exchange happens at the server's /oauth/authorize endpoint. The
// resulting refresh_token is written to <tokens_dir>/<channel>.json by
// the server (the CLI does not perform the token exchange).
func channelsLoginCmd(rt *Runtime) *cobra.Command {
	var (
		configPath  string
		redirectURL string
		state       string
	)
	cmd := &cobra.Command{
		Use:   "login CHANNEL",
		Short: "Initiate OAuth login for a channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			channel := strings.ToLower(args[0])
			if _, ok := supportedChannels[channel]; !ok {
				return fmt.Errorf("%w: %s (supported: feishu, slack, dingtalk, telegram)", ErrUnsupportedChannel, channel)
			}
			builder := rt.OAuthURLBuilder
			if builder == nil {
				builder = defaultOAuthURLBuilder{}
			}
			u, err := builder.Build(cmd.Context(), channel, configPath)
			if err != nil {
				return err
			}
			if redirectURL != "" {
				// Allow callers to override the redirect_uri the server
				// will use; append as a query param if the builder did
				// not already include one.
				if !hasQuery(u, "redirect_uri") {
					u = appendQuery(u, "redirect_uri", redirectURL)
				}
			}
			if state != "" && !hasQuery(u, "state") {
				u = appendQuery(u, "state", state)
			}
			tokensDir, err := ChannelTokensDir()
			if err != nil {
				fmt.Fprintf(rt.Err, "warning: %v\n", err)
			} else {
				fmt.Fprintf(rt.Out, "Tokens will be stored at: %s/%s.json\n", tokensDir, channel)
			}
			fmt.Fprintln(rt.Out, u)
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to vikingbot.conf")
	cmd.Flags().StringVar(&redirectURL, "redirect-uri", "", "OAuth redirect URI (defaults to server's /oauth/authorize)")
	cmd.Flags().StringVar(&state, "state", "", "OAuth state parameter for CSRF protection")
	return cmd
}

// defaultOAuthURLBuilder loads the bot config to construct the OAuth
// URL. Feishu uses the open-apis/authen/v1/authorize endpoint; other
// channels are stubbed with TODO until their OAuth flows are wired.
type defaultOAuthURLBuilder struct{}

// Build returns the OAuth URL for the given channel.
func (defaultOAuthURLBuilder) Build(_ context.Context, channel, configPath string) (string, error) {
	switch channel {
	case "feishu":
		return feishuOAuthURL(configPath)
	case "slack", "dingtalk", "telegram":
		// TODO: OAuth URL construction for non-feishu channels is not
		// yet wired. The feishu_watch_auth work covers feishu; the
		// remaining channels will be implemented alongside their
		// respective OAuth integrations.
		return "", fmt.Errorf("cli: channels login %s: OAuth URL construction not yet implemented; see TODO in cmd_channels.go", channel)
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedChannel, channel)
	}
}

// feishuOAuthURL builds the Feishu user-access OAuth URL using app_id
// from the bot config. The redirect_uri defaults to
// <server_url>/oauth/authorize; the server's authorize endpoint
// exchanges the code for a refresh_token and writes it to
// <tokens_dir>/feishu.json.
func feishuOAuthURL(configPath string) (string, error) {
	cfg, err := botconfig.Load(configPath)
	if err != nil {
		return "", fmt.Errorf("cli: load bot config: %w", err)
	}
	var appID string
	for _, ch := range cfg.Channels {
		if ch.Provider == "feishu" {
			appID = ch.AppID
			break
		}
	}
	if appID == "" {
		return "", fmt.Errorf("cli: no feishu channel with app_id configured; set channels.<name>.app_id in vikingbot.conf")
	}
	redirect := strings.TrimRight(cfg.ServerURL, "/") + "/oauth/authorize"
	q := url.Values{}
	q.Set("app_id", appID)
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	// state is added by the caller; use a placeholder so the URL parses.
	q.Set("state", "ov-channels-login")
	return "https://open.feishu.cn/open-apis/authen/v1/authorize?" + q.Encode(), nil
}

// hasQuery reports whether u already contains the given query key.
func hasQuery(rawURL, key string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return parsed.Query().Has(key)
}

// appendQuery returns rawURL with key=val appended to its query string.
func appendQuery(rawURL, key, val string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := parsed.Query()
	q.Set(key, val)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// SaveChannelToken writes the given token (refresh_token + access_token
// + expiry) to <tokens_dir>/<channel>.json with mode 0600. It is
// exported so the server's /oauth/authorize callback can call it after
// completing the token exchange; the CLI itself does not perform the
// exchange.
func SaveChannelToken(channel string, token map[string]any) error {
	tokensDir, err := ChannelTokensDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(tokensDir, 0o700); err != nil {
		return fmt.Errorf("cli: mkdir %s: %w", tokensDir, err)
	}
	path := filepath.Join(tokensDir, channel+".json")
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("cli: marshal token: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("cli: write %s: %w", path, err)
	}
	return nil
}
