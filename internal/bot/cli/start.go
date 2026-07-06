// Package cli wires vikingbot's cobra subcommands: start, channels,
// status, console. Each command is a thin wrapper around the bot's
// internal packages (channels, agent, etc.).
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/bot/agent"
	"github.com/saker-ai/ctxhub/internal/bot/bus"
	"github.com/saker-ai/ctxhub/internal/bot/channels"
	botconfig "github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/bot/heartbeat"
	"github.com/saker-ai/ctxhub/internal/bot/observability"
	"github.com/saker-ai/ctxhub/internal/bot/providers"
	"github.com/saker-ai/ctxhub/internal/bot/session"
	isession "github.com/saker-ai/ctxhub/internal/session"
)

// NewStartCmd returns the `vikingbot start` cobra command. It wires
// config -> channels -> agent -> bus and runs the bot until SIGINT.
func NewStartCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the vikingbot runtime",
		Long:  "Start loads the bot config, connects all enabled channels, and runs the agent loop until SIGINT/SIGTERM.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := botconfig.Load(configPath)
			if err != nil {
				return err
			}
			if !cfg.Enabled {
				fmt.Fprintln(cmd.OutOrStdout(), "vikingbot: disabled by config; nothing to do")
				return nil
			}
			return Run(cmd.Context(), cfg)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to vikingbot.conf")
	return cmd
}

// Run wires the bot runtime and blocks until ctx is canceled. It is
// exported so tests can drive it with a stub config.
func Run(ctx context.Context, cfg *botconfig.Config) error {
	obs := observability.New(cfg.OTEL)
	logger := obs.Logger()
	logger.Info("vikingbot starting", "channels", len(cfg.Channels))

	// Provider (LLM).
	p, err := providers.New(cfg.Provider)
	if err != nil {
		return fmt.Errorf("provider: %w", err)
	}

	// MCP client (optional).
	var mcp agent.MCPClient
	if cfg.Agent.MCPURL != "" {
		c, err := agent.NewMCPHTTPClient(cfg.Agent)
		if err != nil {
			logger.Warn("mcp client init failed; running in chat-only mode", "err", err)
		} else {
			if err := c.Init(ctx); err != nil {
				logger.Warn("mcp init failed; running in chat-only mode", "err", err)
			} else {
				mcp = c
				defer c.Close()
			}
		}
	}

	// Session adapter.
	store := isession.NewMemoryStore(isession.StoreConfig{})
	sess := session.New(store)

	// Agent.
	a := agent.New(cfg.Provider, p, mcp, sess, obs)

	// Channels.
	dispatcher := channels.NewDispatcher()
	for name, chCfg := range cfg.Channels {
		if !chCfg.Enabled {
			continue
		}
		ch, err := channels.New(name, chCfg)
		if err != nil {
			logger.Warn("channel skipped", "name", name, "err", err)
			continue
		}
		// Connect channels that support it.
		switch c := ch.(type) {
		case *channels.Telegram:
			if err := c.Connect(ctx, nil); err != nil {
				logger.Warn("channel connect failed", "name", name, "err", err)
				continue
			}
		case *channels.Feishu:
			if err := c.Connect(ctx, nil); err != nil {
				logger.Warn("channel connect failed", "name", name, "err", err)
				continue
			}
		case *channels.DingTalk:
			if err := c.Connect(ctx, nil); err != nil {
				logger.Warn("channel connect failed", "name", name, "err", err)
				continue
			}
		case *channels.Slack:
			if err := c.Connect(ctx, nil); err != nil {
				logger.Warn("channel connect failed", "name", name, "err", err)
				continue
			}
		case *channels.QQ:
			if err := c.Connect(ctx, nil, nil); err != nil {
				logger.Warn("channel connect failed", "name", name, "err", err)
				continue
			}
		case *channels.WebSocket:
			if err := c.Connect(ctx, nil); err != nil {
				logger.Warn("channel connect failed", "name", name, "err", err)
				continue
			}
		}
		dispatcher.Register(ch)
	}

	// Bus.
	b := bus.New(cfg.Bus.QueueSize)

	// Heartbeat.
	hbPayload := heartbeat.Payload{
		Bot:     "vikingbot",
		Account: cfg.Account,
		User:    cfg.User,
		Peer:    cfg.ActorPeer,
	}
	hb := heartbeat.New(cfg.Heartbeat, cfg.ServerURL, hbPayload, nil)
	hb.SetChannels(dispatcher)

	// Signal handling.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("vikingbot shutting down")
		cancel()
	}()

	// Launch channels.
	errCh := make(chan error, 1)
	go func() {
		errCh <- dispatcher.StartAll(ctx, func(ctx context.Context, msg channels.IncomingMessage) error {
			// Stamp identity if missing.
			if msg.Identity.Account == "" {
				msg.Identity.Account = cfg.Account
				msg.Identity.User = cfg.User
				msg.Identity.ActorPeer = msg.ChannelName
			}
			reply, err := a.Handle(ctx, msg)
			if err != nil {
				logger.Error("agent handle failed", "err", err)
				return err
			}
			if reply == "" {
				return nil
			}
			out := channels.OutgoingMessage{ChatID: msg.ChatID, Text: reply}
			return dispatcher.Send(ctx, out, msg.ChannelName)
		})
	}()

	// Heartbeat loop.
	go func() { _ = hb.Start(ctx) }()

	// Bus subscriber (drains the in-process queue; agent also runs
	// synchronously in the dispatcher callback, so this is a backup
	// path for durable redelivery).
	go func() {
		for m := range b.Subscribe(ctx) {
			logger.Info("bus redelivered", "chat_id", m.ChatID)
		}
	}()

	defer func() {
		_ = b.Shutdown(context.Background())
		hb.Stop()
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// NewChannelsCmd returns the `vikingbot channels` cobra command —
// lists the channels configured in the bot config.
func NewChannelsCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "channels",
		Short: "List configured channels",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := botconfig.Load(configPath)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(cfg.Channels) == 0 {
				fmt.Fprintln(out, "(no channels configured)")
				return nil
			}
			for name, ch := range cfg.Channels {
				state := "disabled"
				if ch.Enabled {
					state = "enabled"
				}
				fmt.Fprintf(out, "%s\t%s\t%s\n", name, ch.Provider, state)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to vikingbot.conf")
	return cmd
}

// NewStatusCmd returns the `vikingbot status` cobra command — pings
// the configured ctxhub-server and reports reachability.
func NewStatusCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report vikingbot status (config + server reachability)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := botconfig.Load(configPath)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "enabled: %v\n", cfg.Enabled)
			fmt.Fprintf(out, "server_url: %s\n", cfg.ServerURL)
			fmt.Fprintf(out, "account: %s\n", cfg.Account)
			fmt.Fprintf(out, "channels: %d\n", len(cfg.Channels))
			fmt.Fprintf(out, "provider: %s\n", cfg.Provider.Backend)
			fmt.Fprintf(out, "mcp_url: %s\n", cfg.Agent.MCPURL)
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to vikingbot.conf")
	return cmd
}
