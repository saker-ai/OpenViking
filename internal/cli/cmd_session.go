package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// SessionCmd builds `ov session` with `create` and `commit` subcommands.
// The server exposes POST /api/v1/sessions (create) and
// POST /api/v1/sessions/:id/commit (commit).
func SessionCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage OpenViking sessions",
	}
	cmd.AddCommand(sessionCreateCmd(rt))
	cmd.AddCommand(sessionCommitCmd(rt))
	return cmd
}

func sessionCreateCmd(rt *Runtime) *cobra.Command {
	var (
		title   string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			body := map[string]any{}
			if title != "" {
				body["title"] = title
			}
			var sess domain.Session
			if err := rt.Client.PostJSON(cmd.Context(), "/api/v1/sessions", body, &sess); err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(rt.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(sess)
			}
			fmt.Fprintf(rt.Out, "session: %s\n", sess.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "session title")
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}

func sessionCommitCmd(rt *Runtime) *cobra.Command {
	var (
		message string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "commit SESSION_ID",
		Short: "Commit pending session turns into a memory diff",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			body := map[string]any{}
			if message != "" {
				body["message"] = message
			}
			var out map[string]any
			if err := rt.Client.PostJSON(cmd.Context(), "/api/v1/sessions/"+args[0]+"/commit", body, &out); err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(rt.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			fmt.Fprintf(rt.Out, "committed session: %s\n", args[0])
			if diff, ok := out["memory_diff"].(map[string]any); ok {
				if added, ok := diff["added"].([]any); ok && len(added) > 0 {
					fmt.Fprintf(rt.Out, "memories added: %d\n", len(added))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "commit message")
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}
