// Package cli wires vikingbot's cobra subcommands: start, channels,
// status, console. Each command is a thin wrapper around the bot's
// internal packages (channels, agent, etc.).
package cli

import (
	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/bot/channels"
	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/bot/console"
)

// NewConsoleCmd returns the `vikingbot console` cobra command —
// launches an interactive TUI for inspecting live channel state.
//
// The console is optional; operators run it alongside `vikingbot start`
// to peek at inbound messages and agent replies without tailing JSON
// logs. It requires a config only to discover the dispatcher shape
// (though it can run against an empty dispatcher for demo purposes).
func NewConsoleCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "console",
		Short: "Launch the vikingbot interactive TUI",
		Long:  "Console opens a bubbletea-based TUI that shows live channel state, recent inbound messages, and agent replies. Quit with q or Ctrl+C.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			dispatcher := channels.NewDispatcher()
			for name, chCfg := range cfg.Channels {
				if !chCfg.Enabled {
					continue
				}
				ch, err := channels.New(name, chCfg)
				if err != nil {
					continue
				}
				dispatcher.Register(ch)
			}
			return console.Run(cmd.Context(), dispatcher, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to vikingbot.conf")
	return cmd
}
