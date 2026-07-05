package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// AddResourceCmd builds the `ov add-resource PATH` command.
// It POSTs to /api/v1/resources to register a new resource entry.
func AddResourceCmd(rt *Runtime) *cobra.Command {
	var (
		resourceType string
		parent       string
		metadata     []string
	)
	cmd := &cobra.Command{
		Use:   "add-resource PATH",
		Short: "Add a resource to OpenViking",
		Long:  `Register a viking:// resource entry via POST /api/v1/resources.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			body := map[string]any{
				"path": args[0],
				"type": resourceType,
			}
			if parent != "" {
				body["parent"] = parent
			}
			if md := parseMetadata(metadata); len(md) > 0 {
				body["metadata"] = md
			}
			var out map[string]any
			if err := rt.Client.PostJSON(cmd.Context(), "/api/v1/resources", body, &out); err != nil {
				return err
			}
			fmt.Fprintf(rt.Out, "%s\n", rt.T("ov.add_resource.added", map[string]interface{}{"path": args[0]}))
			if uri, ok := out["uri"].(string); ok {
				fmt.Fprintf(rt.Out, "uri: %s\n", uri)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&resourceType, "type", string(domain.ResourceTypeFile), "resource type (file|dir|url|memory|skill)")
	cmd.Flags().StringVar(&parent, "parent", "", "parent URI")
	cmd.Flags().StringArrayVar(&metadata, "metadata", nil, "metadata key=value (repeatable)")
	return cmd
}

// LsCmd builds the `ov ls [PATH]` command. With no arg it lists the
// root collection; with a path it lists the children of that path.
func LsCmd(rt *Runtime) *cobra.Command {
	var (
		long    bool
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "ls [PATH]",
		Short: "List resources",
		Long:  `List resources under a path (default: /). Issues GET /api/v1/resources or GET /api/v1/fs/ls?path=.`,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			path := "/"
			if len(args) > 0 {
				path = args[0]
			}
			if jsonOut {
				return rt.lsJSON(cmd.Context(), path, long)
			}
			return rt.lsTable(cmd.Context(), path, long)
		},
	}
	cmd.Flags().BoolVarP(&long, "long", "l", false, "long listing (size, modified, type)")
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}

func (r *Runtime) lsTable(ctx context.Context, path string, long bool) error {
	resources, err := r.fetchResources(ctx, path)
	if err != nil {
		return err
	}
	if long {
		headers := []string{"TYPE", "SIZE", "MODIFIED", "URI"}
		var rows [][]string
		for _, res := range resources {
			rows = append(rows, []string{
				string(res.Type),
				fmt.Sprintf("%d", res.Size),
				res.ModifiedAt.Format("2006-01-02 15:04"),
				res.URI,
			})
		}
		r.PrintTable(headers, rows)
		return nil
	}
	for _, res := range resources {
		fmt.Fprintln(r.Out, res.URI)
	}
	return nil
}

func (r *Runtime) lsJSON(ctx context.Context, path string, long bool) error {
	resources, err := r.fetchResources(ctx, path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(r.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(resources)
}

func (r *Runtime) fetchResources(ctx context.Context, path string) ([]domain.Resource, error) {
	// Use the filesystem ls endpoint when a path is supplied; otherwise
	// list all resources via the top-level collection.
	q := ""
	if path != "" && path != "/" {
		q = "?path=" + path
	}
	var resp struct {
		Items []domain.Resource `json:"items"`
	}
	if err := r.Client.GetJSON(ctx, "/api/v1/fs/ls"+q, &resp); err != nil {
		// Fall back to /resources when fs.ls is unavailable.
		var list []domain.Resource
		if err2 := r.Client.GetJSON(ctx, "/api/v1/resources", &list); err2 != nil {
			return nil, err
		}
		return list, nil
	}
	return resp.Items, nil
}

// ReadCmd builds the `ov read PATH` command. It streams the raw
// content from GET /api/v1/content/<uri>.
func ReadCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "read PATH",
		Short: "Read a resource's content",
		Long:  `Fetch the raw content of a resource via GET /api/v1/content/<uri>.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			resp, err := rt.Client.Raw(cmd.Context(), http.MethodGet, "/api/v1/content/"+strings.TrimPrefix(args[0], "/"))
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			_, err = io.Copy(rt.Out, resp.Body)
			return err
		},
	}
	return cmd
}

// parseMetadata converts --metadata key=value flags into a map.
func parseMetadata(pairs []string) map[string]any {
	out := map[string]any{}
	for _, p := range pairs {
		if i := strings.IndexByte(p, '='); i >= 0 {
			out[p[:i]] = p[i+1:]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
