package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// TaskCmd builds `ov task` with the `list` subcommand. The server
// exposes GET /api/v1/tasks for the task queue.
func TaskCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Manage OpenViking async tasks",
	}
	cmd.AddCommand(taskListCmd(rt))
	return cmd
}

func taskListCmd(rt *Runtime) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List queued tasks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			var resp struct {
				Items []map[string]any `json:"items"`
			}
			if err := rt.Client.GetJSON(cmd.Context(), "/api/v1/tasks", &resp); err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(rt.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp.Items)
			}
			headers := []string{"ID", "TYPE", "STATUS"}
			var rows [][]string
			for _, it := range resp.Items {
				rows = append(rows, []string{
					fmt.Sprintf("%v", it["id"]),
					fmt.Sprintf("%v", it["type"]),
					fmt.Sprintf("%v", it["status"]),
				})
			}
			rt.PrintTable(headers, rows)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}

// ObserverCmd builds `ov observer` with the `list` subcommand.
// The server exposes GET /api/v1/observer.
func ObserverCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "observer",
		Short: "Manage OpenViking observers",
	}
	cmd.AddCommand(observerListCmd(rt))
	return cmd
}

func observerListCmd(rt *Runtime) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered observers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			var resp struct {
				Items []map[string]any `json:"items"`
			}
			if err := rt.Client.GetJSON(cmd.Context(), "/api/v1/observer", &resp); err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(rt.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp.Items)
			}
			headers := []string{"ID", "TARGET", "EVENT"}
			var rows [][]string
			for _, it := range resp.Items {
				rows = append(rows, []string{
					fmt.Sprintf("%v", it["id"]),
					fmt.Sprintf("%v", it["target"]),
					fmt.Sprintf("%v", it["event"]),
				})
			}
			rt.PrintTable(headers, rows)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}

// SnapshotCmd builds `ov snapshot` with `create` and `restore` subcommands.
// The server exposes POST /api/v1/snapshot (create) and
// POST /api/v1/snapshot/:id/restore (restore).
func SnapshotCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Create or restore OpenViking snapshots",
	}
	cmd.AddCommand(snapshotCreateCmd(rt))
	cmd.AddCommand(snapshotRestoreCmd(rt))
	return cmd
}

func snapshotCreateCmd(rt *Runtime) *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Capture a snapshot of the current store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			body := map[string]any{}
			if label != "" {
				body["label"] = label
			}
			var out map[string]any
			if err := rt.Client.PostJSON(cmd.Context(), "/api/v1/snapshot", body, &out); err != nil {
				return err
			}
			fmt.Fprintf(rt.Out, "snapshot: %v\n", out["id"])
			return nil
		},
	}
	cmd.Flags().StringVarP(&label, "label", "l", "", "snapshot label")
	return cmd
}

func snapshotRestoreCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restore SNAPSHOT_ID",
		Short: "Restore the store from a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			var out map[string]any
			if err := rt.Client.PostJSON(cmd.Context(), "/api/v1/snapshot/"+args[0]+"/restore", map[string]any{}, &out); err != nil {
				return err
			}
			fmt.Fprintf(rt.Out, "restored: %s\n", args[0])
			return nil
		},
	}
	return cmd
}

// PrivacyCmd builds `ov privacy` with `set` and `get` subcommands.
// The server exposes PUT /api/v1/privacy-configs/:id and
// GET /api/v1/privacy-configs/:id.
func PrivacyCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "privacy",
		Short: "Manage privacy configuration",
	}
	cmd.AddCommand(privacySetCmd(rt))
	cmd.AddCommand(privacyGetCmd(rt))
	return cmd
}

func privacySetCmd(rt *Runtime) *cobra.Command {
	var (
		level    string
		patterns []string
	)
	cmd := &cobra.Command{
		Use:   "set [ID]",
		Short: "Set privacy configuration",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			body := map[string]any{}
			if level != "" {
				body["level"] = level
			}
			if len(patterns) > 0 {
				body["patterns"] = patterns
			}
			path := "/api/v1/privacy-configs"
			if len(args) == 1 {
				path += "/" + args[0]
			}
			var out map[string]any
			if err := rt.Client.PutJSON(cmd.Context(), path, body, &out); err != nil {
				return err
			}
			fmt.Fprintf(rt.Out, "privacy config: %v\n", out["id"])
			return nil
		},
	}
	cmd.Flags().StringVar(&level, "level", "", "privacy level (redact|mask|off)")
	cmd.Flags().StringArrayVar(&patterns, "pattern", nil, "glob pattern (repeatable)")
	return cmd
}

func privacyGetCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get [ID]",
		Short: "Show privacy configuration",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			path := "/api/v1/privacy-configs"
			if len(args) == 1 {
				path += "/" + args[0]
			}
			var out map[string]any
			if err := rt.Client.GetJSON(cmd.Context(), path, &out); err != nil {
				return err
			}
			enc := json.NewEncoder(rt.Out)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		},
	}
	return cmd
}
