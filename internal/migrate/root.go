// Package migrate builds the ctxhub-migrate CLI: schema migrations
// for ragfs / vectordb / queuefs and the Python -> Go data bridge.
//
// Subcommands:
//   - version        : print ctxhub-migrate version
//   - ovpack         : pack/unpack ovpack offline archives
//   - ragfs          : apply ragfs schema migrations (SQLite metadata store)
//   - vectordb       : ensure vectordb collection exists with current schema
//   - queuefs        : verify queuefs Redis state and report stale queues
//   - all            : run ragfs, vectordb, queuefs in order (default)
package migrate

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/version"
)

// NewRoot returns the ctxhub-migrate CLI root command.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "ctxhub-migrate",
		Short:         "OpenViking schema / data migration tool",
		Long:          "ctxhub-migrate applies ragfs / vectordb / queuefs schema migrations and bridges Python -> Go data.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.AddCommand(
		versionCmd(),
		ovpackCmd(),
		ragfsCmd(),
		vectordbCmd(),
		queuefsCmd(),
		allCmd(),
	)
	// `migrate` with no subcommand runs `all` so operators can type
	// `ctxhub-migrate --dry-run` for a quick plan.
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return runAllMigrate(cmd.Context(), cmd.OutOrStdout(), allOptions{})
	}
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print ctxhub-migrate version",
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
