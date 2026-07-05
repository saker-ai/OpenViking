package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// SkillsCmd builds `ov skills` with the `list` subcommand. The server
// exposes GET /api/v1/skills for collection listing.
func SkillsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Manage OpenViking skills",
	}
	cmd.AddCommand(skillsListCmd(rt))
	return cmd
}

func skillsListCmd(rt *Runtime) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered skills",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			var skills []domain.Skill
			if err := rt.Client.GetJSON(cmd.Context(), "/api/v1/skills", &skills); err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(rt.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(skills)
			}
			headers := []string{"URI", "NAME", "STEPS"}
			var rows [][]string
			for _, s := range skills {
				rows = append(rows, []string{
					s.URI,
					s.Name,
					fmt.Sprintf("%d", len(s.Steps)),
				})
			}
			rt.PrintTable(headers, rows)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}
