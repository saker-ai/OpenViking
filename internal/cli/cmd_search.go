package cli

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

// FindCmd builds `ov find QUERY` — retrieves related resources by
// relation traversal. GET /api/v1/relations?uri=<q> or ?q=<q>.
func FindCmd(rt *Runtime) *cobra.Command {
	var (
		relationType string
		limit        int
		jsonOut      bool
	)
	cmd := &cobra.Command{
		Use:   "find QUERY",
		Short: "Find related resources via relation traversal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			q := url.Values{"q": {args[0]}}
			if relationType != "" {
				q.Set("relation", relationType)
			}
			if limit > 0 {
				q.Set("limit", fmt.Sprintf("%d", limit))
			}
			var resp struct {
				Items []map[string]any `json:"items"`
			}
			if err := rt.Client.GetJSON(cmd.Context(), "/api/v1/relations?"+q.Encode(), &resp); err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(rt.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp.Items)
			}
			headers := []string{"URI", "TYPE", "SCORE"}
			var rows [][]string
			for _, it := range resp.Items {
				rows = append(rows, []string{
					fmt.Sprintf("%v", it["uri"]),
					fmt.Sprintf("%v", it["type"]),
					fmt.Sprintf("%v", it["score"]),
				})
			}
			rt.PrintTable(headers, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&relationType, "relation", "", "filter by relation type")
	cmd.Flags().IntVarP(&limit, "limit", "n", 0, "max results (0 = server default)")
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}

// SearchCmd builds `ov search QUERY` — semantic search via the
// vector index. POST /api/v1/search with {"query": ..., "limit": ...}.
func SearchCmd(rt *Runtime) *cobra.Command {
	var (
		limit   int
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "search QUERY",
		Short: "Semantic search across the corpus",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			body := map[string]any{"query": args[0]}
			if limit > 0 {
				body["limit"] = limit
			}
			var resp struct {
				Items []map[string]any `json:"items"`
			}
			if err := rt.Client.PostJSON(cmd.Context(), "/api/v1/search", body, &resp); err != nil {
				return err
			}
			if len(resp.Items) == 0 {
				fmt.Fprintln(rt.Out, rt.T("ov.search.no_results"))
				return nil
			}
			if jsonOut {
				enc := json.NewEncoder(rt.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp.Items)
			}
			headers := []string{"SCORE", "URI", "SNIPPET"}
			var rows [][]string
			for _, it := range resp.Items {
				snippet := fmt.Sprintf("%v", it["snippet"])
				if len(snippet) > 60 {
					snippet = snippet[:60] + "..."
				}
				rows = append(rows, []string{
					formatScore(it["score"]),
					fmt.Sprintf("%v", it["uri"]),
					snippet,
				})
			}
			rt.PrintTable(headers, rows)
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 0, "max results (0 = server default)")
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}

// formatScore renders a search score (which may arrive as float64,
// json.Number, or int) as a 3-decimal-place string.
func formatScore(v any) string {
	switch n := v.(type) {
	case float64:
		return fmt.Sprintf("%.3f", n)
	case float32:
		return fmt.Sprintf("%.3f", n)
	case int:
		return fmt.Sprintf("%.3f", float64(n))
	case int64:
		return fmt.Sprintf("%.3f", float64(n))
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return n.String()
		}
		return fmt.Sprintf("%.3f", f)
	default:
		return fmt.Sprintf("%v", v)
	}
}
