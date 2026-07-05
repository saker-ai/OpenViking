package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/version"
)

// DoctorCmd builds `ov doctor`. It probes the configured server's
// /healthz and /readyz endpoints, prints version info, and exits 0
// only when both probes return 200.
func DoctorCmd(rt *Runtime) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose OpenViking connectivity and version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := rt.EnsureClient(cmd.Context()); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			fmt.Fprintf(rt.Out, "ov version: %s\n", version.String())
			fmt.Fprintf(rt.Out, "server:     %s\n", rt.Client.BaseURL())
			fmt.Fprintf(rt.Out, "account:    %s\n", rt.Client.Account())
			fmt.Fprintln(rt.Out, "---")

			ok := true
			for _, path := range []string{"/healthz", "/readyz", "/version"} {
				if err := probe(ctx, rt, path); err != nil {
					fmt.Fprintf(rt.Err, "%-12s FAIL: %v\n", path, err)
					ok = false
					continue
				}
			}
			if !ok {
				return fmt.Errorf("doctor: one or more probes failed")
			}
			fmt.Fprintln(rt.Out, "all probes OK")
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "probe timeout")
	return cmd
}

func probe(ctx context.Context, rt *Runtime, path string) error {
	resp, err := rt.Client.Raw(ctx, http.MethodGet, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	label := string(body)
	if len(label) > 60 {
		label = label[:60]
	}
	if path == "/version" {
		// Try to decode version JSON for nicer output.
		var v map[string]any
		if json.Unmarshal(body, &v) == nil {
			label = fmt.Sprintf("version=%v commit=%v", v["version"], v["commit"])
		}
	}
	fmt.Fprintf(rt.Out, "%-12s OK   %s\n", path, label)
	return nil
}
