package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// AdminCmd builds `ov admin` with `account`, `user`, and `key`
// subcommands. Each maps onto the corresponding /api/v1/admin/* route.
func AdminCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administrative operations",
	}
	cmd.AddCommand(adminAccountCmd(rt))
	cmd.AddCommand(adminUserCmd(rt))
	cmd.AddCommand(adminKeyCmd(rt))
	return cmd
}

func adminAccountCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "Manage OpenViking accounts",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List accounts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			var resp struct {
				Items []map[string]any `json:"items"`
			}
			if err := rt.Client.GetJSON(cmd.Context(), "/api/v1/admin/accounts", &resp); err != nil {
				return err
			}
			enc := json.NewEncoder(rt.Out)
			enc.SetIndent("", "  ")
			return enc.Encode(resp.Items)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "create NAME",
		Short: "Create an account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			var out map[string]any
			if err := rt.Client.PostJSON(cmd.Context(), "/api/v1/admin/accounts",
				map[string]any{"name": args[0]}, &out); err != nil {
				return err
			}
			fmt.Fprintf(rt.Out, "account: %v\n", out["id"])
			return nil
		},
	})
	return cmd
}

func adminUserCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage OpenViking users",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List users",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			var resp struct {
				Items []map[string]any `json:"items"`
			}
			if err := rt.Client.GetJSON(cmd.Context(), "/api/v1/admin/users", &resp); err != nil {
				return err
			}
			enc := json.NewEncoder(rt.Out)
			enc.SetIndent("", "  ")
			return enc.Encode(resp.Items)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "create NAME",
		Short: "Create a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			var out map[string]any
			if err := rt.Client.PostJSON(cmd.Context(), "/api/v1/admin/users",
				map[string]any{"name": args[0]}, &out); err != nil {
				return err
			}
			fmt.Fprintf(rt.Out, "user: %v\n", out["id"])
			return nil
		},
	})
	return cmd
}

func adminKeyCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage OpenViking API keys",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "create ACCOUNT_ID",
		Short: "Issue a new API key for an account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			var out map[string]any
			if err := rt.Client.PostJSON(cmd.Context(),
				fmt.Sprintf("/api/v1/admin/accounts/%s/api-keys", args[0]),
				map[string]any{}, &out); err != nil {
				return err
			}
			fmt.Fprintf(rt.Out, "api-key: %v\n", out["api_key"])
			return nil
		},
	})
	return cmd
}
