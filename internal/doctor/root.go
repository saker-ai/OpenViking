// Package doctor builds the openviking-doctor CLI: a diagnostic tool that
// inspects config, connectivity (ragfs / vectordb / queuefs / embedder),
// permissions, and disk health, and prints a structured report.
//
// Subcommands:
//   - doctor config        — parse and validate the active config file
//   - doctor connectivity  — ping each configured backend
//   - doctor disk          — stat each configured storage path
//   - doctor all (default) — run every probe in order
//
// Flags:
//   - --config PATH  overrides the config search path
//   - --json         emits a machine-readable JSON report
//   - --timeout D    per-probe context timeout (default 5s)
package doctor

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/version"
)

// defaultProbeTimeout is the per-probe context timeout. Backends that do not
// respond within this window report status fail with a deadline-exceeded
// error.
const defaultProbeTimeout = 5 * time.Second

// configSections is the canonical section list the config probe reports on.
var configSections = []string{"server", "vectordb", "embedder", "rerank", "vlm", "queuefs", "ragfs"}

// NewRoot returns the openviking-doctor CLI root command. Invoking it with
// no subcommand runs `all`.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "openviking-doctor",
		Short:         "OpenViking diagnostic tool",
		Long:          "openviking-doctor inspects config, connectivity, permissions, and disk health.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAll(cmd, args)
		},
	}
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	root.AddCommand(versionCmd())
	root.AddCommand(configCmd())
	root.AddCommand(connectivityCmd())
	root.AddCommand(diskCmd())
	root.AddCommand(allCmd())
	// Root also accepts the common flags so `openviking-doctor --json` and
	// `openviking-doctor --config PATH` work as shorthand for `all`.
	commonFlags(root)
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print openviking-doctor version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return nil
		},
	}
}

// commonFlags installs --config, --json, --timeout on a subcommand.
func commonFlags(c *cobra.Command) *cobra.Command {
	c.Flags().String("config", "", "path to ov.conf (overrides search path)")
	c.Flags().Bool("json", false, "emit machine-readable JSON")
	c.Flags().Duration("timeout", defaultProbeTimeout, "per-probe context timeout")
	return c
}

func configCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Parse and validate the active config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout*2)
			defer cancel()
			results := probeConfig(ctx, path, configSections)
			report := &Report{
				Command:   "config",
				StartedAt: time.Now().UTC(),
				Results:   results,
			}
			if report.Failed() {
				return emitAndFail(cmd, report)
			}
			return emit(cmd, report)
		},
	}
	return commonFlags(c)
}

func connectivityCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "connectivity",
		Short: "Ping each configured backend (vectordb, embedder, rerank, vlm, queuefs, ragfs)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			cfg, err := config.Load(path)
			if err != nil {
				// Surface load failure as a single fail result and exit
				// non-zero. Connectivity cannot be probed without a config.
				report := &Report{
					Command:   "connectivity",
					StartedAt: time.Now().UTC(),
					Results: []ProbeResult{{
						Name:   "connectivity",
						Status: StatusFail,
						Error:  "config load: " + err.Error(),
					}},
				}
				return emitAndFail(cmd, report)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout*2)
			defer cancel()
			results := probeConnectivity(ctx, cfg, timeout)
			report := &Report{
				Command:   "connectivity",
				StartedAt: time.Now().UTC(),
				Results:   results,
			}
			if report.Failed() {
				return emitAndFail(cmd, report)
			}
			return emit(cmd, report)
		},
	}
	return commonFlags(c)
}

func diskCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "disk",
		Short: "Inspect each configured storage path (exists, writable, free space)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("config")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			cfg, err := config.Load(path)
			if err != nil {
				report := &Report{
					Command:   "disk",
					StartedAt: time.Now().UTC(),
					Results: []ProbeResult{{
						Name:   "disk",
						Status: StatusFail,
						Error:  "config load: " + err.Error(),
					}},
				}
				return emitAndFail(cmd, report)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout*2)
			defer cancel()
			results := probeDisk(ctx, cfg)
			report := &Report{
				Command:   "disk",
				StartedAt: time.Now().UTC(),
				Results:   results,
			}
			if report.Failed() {
				return emitAndFail(cmd, report)
			}
			return emit(cmd, report)
		},
	}
	return commonFlags(c)
}

func allCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "all",
		Short: "Run config, connectivity, and disk probes in order",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAll(cmd, args)
		},
	}
	return commonFlags(c)
}

// runAll is the default action when no subcommand is given. It runs each
// probe group in order, merges the results, and exits non-zero if any
// non-skip probe failed.
func runAll(cmd *cobra.Command, args []string) error {
	path, _ := cmd.Flags().GetString("config")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	cfg, cfgErr := config.Load(path)
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout*4)
	defer cancel()

	results := []ProbeResult{}
	results = append(results, probeConfig(ctx, path, configSections)...)
	if cfgErr != nil {
		results = append(results,
			ProbeResult{
				Name:   "connectivity",
				Status: StatusFail,
				Error:  "config load: " + cfgErr.Error(),
			},
			ProbeResult{
				Name:   "disk",
				Status: StatusFail,
				Error:  "config load: " + cfgErr.Error(),
			},
		)
	} else {
		results = append(results, probeConnectivity(ctx, cfg, timeout)...)
		results = append(results, probeDisk(ctx, cfg)...)
	}

	report := &Report{
		Command:   "all",
		StartedAt: time.Now().UTC(),
		Results:   results,
	}
	if report.Failed() {
		return emitAndFail(cmd, report)
	}
	return emit(cmd, report)
}

// emit writes the report to stdout and returns nil so callers can return
// it directly from RunE.
func emit(cmd *cobra.Command, report *Report) error {
	jsonOut, _ := cmd.Flags().GetBool("json")
	if jsonOut {
		return report.PrintJSON(cmd.OutOrStdout())
	}
	report.Print(cmd.OutOrStdout())
	return nil
}

// emitAndFail writes the report and returns errProbeFailed so cobra exits
// non-zero. Used by RunE handlers when the report contains failures.
func emitAndFail(cmd *cobra.Command, report *Report) error {
	_ = emit(cmd, report)
	return errProbeFailed
}

// errProbeFailed is a sentinel returned by RunE handlers when the report
// contains at least one failed probe. cobra converts a non-nil RunE error
// into a non-zero process exit.
var errProbeFailed = fmt.Errorf("doctor: one or more probes failed")

// Execute runs root with the given args.
func Execute(root *cobra.Command, args []string) error {
	root.SetArgs(args)
	return root.Execute()
}
