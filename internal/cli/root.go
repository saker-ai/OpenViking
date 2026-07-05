// Package cli builds the ov command-line interface: viking:// URI browsing,
// ragfs admin, ingest, session, mcp, and config subcommands.
//
// The root command owns the shared Runtime (config + HTTP client +
// token source) and attaches every subcommand defined in design §7.10.2.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/cli/tui"
	"github.com/saker-ai/ctxhub/internal/cli/wizard"
	"github.com/saker-ai/ctxhub/internal/version"
)

// NewRoot returns the ov CLI root command. out/err writers default to
// os.Stdout / os.Stderr but can be replaced by tests via SetOut/SetErr.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "ov",
		Short:         "OpenViking CLI",
		Long:          "ov is the command-line client for the OpenViking context database.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.PersistentFlags().BoolP("verbose", "v", false, "verbose output")
	root.PersistentFlags().String("locale", "", "override output locale (e.g. en, zh-CN)")

	// Build the runtime lazily on first RunE so persistent flags are
	// already parsed when we read them.
	rt := newRuntime()
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		return rt.load(root)
	}

	root.AddCommand(versionCmd())
	root.AddCommand(wizard.InitCmd(root.OutOrStdout(), root.ErrOrStderr(), saveWizardConfig))
	root.AddCommand(AddResourceCmd(rt))
	root.AddCommand(LsCmd(rt))
	root.AddCommand(FindCmd(rt))
	root.AddCommand(SearchCmd(rt))
	root.AddCommand(ReadCmd(rt))
	root.AddCommand(SkillsCmd(rt))
	root.AddCommand(SessionCmd(rt))
	root.AddCommand(TaskCmd(rt))
	root.AddCommand(ObserverCmd(rt))
	root.AddCommand(SnapshotCmd(rt))
	root.AddCommand(PrivacyCmd(rt))
	root.AddCommand(CryptoCmd(rt))
	root.AddCommand(AdminCmd(rt))
	root.AddCommand(DoctorCmd(rt))
	root.AddCommand(ChannelsCmd(rt))
	root.AddCommand(CronCmd(rt))
	root.AddCommand(FeedbackStatsCmd(rt))
	root.AddCommand(EvalCmd(rt))
	root.AddCommand(tui.NewCmd(func(cmd *cobra.Command) (tui.Client, error) {
		if err := rt.EnsureClient(cmd.Context()); err != nil {
			return nil, err
		}
		return rt.Client, nil
	}, root.OutOrStdout()))
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print ov version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return nil
		},
	}
}

// Execute runs root with the given args and returns any error.
func Execute(root *cobra.Command, args []string) error {
	root.SetArgs(args)
	return root.Execute()
}

// newRuntime constructs a Runtime bound to this CLI process's
// stdout/stderr. Config is loaded lazily inside load() so that flag
// overrides (e.g. --locale) are applied first.
func newRuntime() *Runtime {
	return &Runtime{
		Out: os.Stdout,
		Err: os.Stderr,
	}
}

// load resolves the runtime's Config from disk + flags. It is invoked
// by root.PersistentPreRunE before any subcommand runs. Errors are
// non-fatal: a missing config or unreadable file is logged to stderr
// and the command continues with defaults so that `ov init` and
// `ov doctor` work pre-config.
func (r *Runtime) load(root *cobra.Command) error {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(r.Err, "warning: %v; using defaults\n", err)
		cfg = &CLIConfig{}
		applyDefaults(cfg)
	}
	r.Config = cfg
	if root != nil {
		if locale, _ := root.Flags().GetString("locale"); locale != "" {
			cfg.Output.Locale = locale
			r.Locale = locale
		}
	}
	return nil
}
