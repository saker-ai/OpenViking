package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/bot/cron"
)

// CronCmd builds `ov cron` with list/add/remove/enable/disable/run
// subcommands. The commands operate on a cron.JobStore; when the
// runtime's CronStore is nil, a default FileStore at
// $OV_DATA_DIR/cron/jobs.json (or ~/.openviking/cron/jobs.json) is used.
func CronCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cron",
		Short: "Manage scheduled cron jobs",
	}
	cmd.AddCommand(cronListCmd(rt))
	cmd.AddCommand(cronAddCmd(rt))
	cmd.AddCommand(cronRemoveCmd(rt))
	cmd.AddCommand(cronEnableCmd(rt))
	cmd.AddCommand(cronDisableCmd(rt))
	cmd.AddCommand(cronRunCmd(rt))
	return cmd
}

// resolveCronStore returns the runtime's CronStore when set, else a
// default FileStore at DefaultStorePath(). The default is constructed
// lazily so tests that inject a MemoryStore don't touch the filesystem.
func resolveCronStore(rt *Runtime) (cron.JobStore, error) {
	if rt.CronStore != nil {
		return rt.CronStore, nil
	}
	path, err := cron.DefaultStorePath()
	if err != nil {
		return nil, err
	}
	return cron.NewFileStore(path), nil
}

func cronListCmd(rt *Runtime) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List scheduled cron jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := resolveCronStore(rt)
			if err != nil {
				return err
			}
			jobs, err := store.List(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(rt.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(jobs)
			}
			if len(jobs) == 0 {
				fmt.Fprintln(rt.Out, "No scheduled jobs.")
				return nil
			}
			headers := []string{"ID", "SCHEDULE", "NEXT_RUN", "ENABLED"}
			rows := make([][]string, 0, len(jobs))
			for _, j := range jobs {
				nextRun := "-"
				if !j.NextRun.IsZero() {
					nextRun = j.NextRun.Local().Format(time.RFC3339)
				}
				enabled := "false"
				if j.Enabled {
					enabled = "true"
				}
				rows = append(rows, []string{j.ID, j.Schedule, nextRun, enabled})
			}
			rt.PrintTable(headers, rows)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}

func cronAddCmd(rt *Runtime) *cobra.Command {
	var (
		schedule string
		typ      string
		payload  string
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a scheduled cron job",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if schedule == "" {
				return fmt.Errorf("cron: --schedule is required")
			}
			if typ == "" {
				return fmt.Errorf("cron: --type is required")
			}
			store, err := resolveCronStore(rt)
			if err != nil {
				return err
			}
			var p []byte
			if payload != "" {
				p = []byte(payload)
			}
			id, err := store.Add(cmd.Context(), schedule, typ, p)
			if err != nil {
				return err
			}
			fmt.Fprintf(rt.Out, "added job: %s\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&schedule, "schedule", "", `cron expression (e.g. "0 9 * * *")`)
	cmd.Flags().StringVar(&typ, "type", "", "task type (e.g. reminder, cleanup)")
	cmd.Flags().StringVar(&payload, "payload", "", "opaque JSON payload passed to the task handler")
	return cmd
}

func cronRemoveCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "remove ID",
		Short: "Remove a scheduled cron job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := resolveCronStore(rt)
			if err != nil {
				return err
			}
			if err := store.Remove(cmd.Context(), args[0]); err != nil {
				if errors.Is(err, cron.ErrJobNotFound) {
					fmt.Fprintf(rt.Out, "job %s not found\n", args[0])
					return nil
				}
				return err
			}
			fmt.Fprintf(rt.Out, "removed job: %s\n", args[0])
			return nil
		},
	}
}

func cronEnableCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "enable ID",
		Short: "Enable a scheduled cron job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cronToggle(rt, cmd.Context(), args[0], true)
		},
	}
}

func cronDisableCmd(rt *Runtime) *cobra.Command {
	return &cobra.Command{
		Use:   "disable ID",
		Short: "Disable a scheduled cron job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cronToggle(rt, cmd.Context(), args[0], false)
		},
	}
}

func cronToggle(rt *Runtime, ctx context.Context, id string, enabled bool) error {
	store, err := resolveCronStore(rt)
	if err != nil {
		return err
	}
	if err := store.SetEnabled(ctx, id, enabled); err != nil {
		if errors.Is(err, cron.ErrJobNotFound) {
			fmt.Fprintf(rt.Out, "job %s not found\n", id)
			return nil
		}
		return err
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	fmt.Fprintf(rt.Out, "%s job: %s\n", state, id)
	return nil
}

func cronRunCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run ID",
		Short: "Trigger a cron job immediately (one-shot)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := resolveCronStore(rt)
			if err != nil {
				return err
			}
			if err := store.Run(cmd.Context(), args[0]); err != nil {
				switch {
				case errors.Is(err, cron.ErrJobNotFound):
					fmt.Fprintf(rt.Out, "job %s not found\n", args[0])
					return nil
				case errors.Is(err, cron.ErrJobDisabled):
					fmt.Fprintf(rt.Out, "job %s is disabled; enable it first\n", args[0])
					return nil
				}
				return err
			}
			fmt.Fprintf(rt.Out, "triggered job: %s\n", args[0])
			return nil
		},
	}
	return cmd
}
