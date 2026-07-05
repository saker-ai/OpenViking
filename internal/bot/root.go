// Package bot builds the vikingbot CLI: multi-channel bot runtime
// (Telegram / Feishu / DingTalk / Slack / QQ).
package bot

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/bot/cli"
	"github.com/saker-ai/ctxhub/internal/version"
)

// NewRoot returns the vikingbot CLI root command.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "vikingbot",
		Short:         "OpenViking multi-channel bot",
		Long:          "vikingbot bridges the OpenViking agent to popular chat platforms.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.AddCommand(versionCmd())
	root.AddCommand(cli.NewStartCmd())
	root.AddCommand(cli.NewChannelsCmd())
	root.AddCommand(cli.NewStatusCmd())
	root.AddCommand(cli.NewConsoleCmd())
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print vikingbot version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return nil
		},
	}
}

// Execute runs root with the given args.
func Execute(root *cobra.Command, args []string) error {
	root.SetArgs(args)
	return root.Execute()
}
