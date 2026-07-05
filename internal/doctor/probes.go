// Package doctor builds the openviking-doctor CLI: a diagnostic tool that
// inspects config, connectivity (ragfs / vectordb / queuefs / embedder),
// permissions, and disk health, and prints a structured report.
//
// Probes return a ProbeResult describing one check. Subcommands (config,
// connectivity, disk, all) assemble ProbeResults into a Report and print it
// either human-readable or JSON (--json).
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ProbeStatus enumerates the outcomes a single probe can report.
//
// "skip" is used when a probe cannot run because a dependency is unconfigured
// (e.g. no embedder provider set) or because the target backend does not
// expose a ping method. Skipped probes never fail the overall doctor run.
type ProbeStatus string

const (
	// StatusOK indicates the probe succeeded.
	StatusOK ProbeStatus = "ok"
	// StatusFail indicates the probe ran but failed.
	StatusFail ProbeStatus = "fail"
	// StatusSkip indicates the probe was skipped (e.g. missing config).
	StatusSkip ProbeStatus = "skip"
)

// ProbeResult is the structured outcome of a single diagnostic probe.
//
// Fields are JSON-tagged so a Report can be serialized with encoding/json for
// machine-readable output. LatencyMS is zero for probes that do not measure
// time (e.g. config parse). Detail carries a human-readable summary.
type ProbeResult struct {
	Name      string      `json:"name"`
	Status    ProbeStatus `json:"status"`
	LatencyMS int64       `json:"latency_ms,omitempty"`
	Detail    string      `json:"detail,omitempty"`
	Error     string      `json:"error,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// Report aggregates ProbeResults from one doctor subcommand run.
type Report struct {
	Command   string            `json:"command"`
	StartedAt time.Time         `json:"started_at"`
	Results   []ProbeResult     `json:"results"`
	Extra     map[string]any    `json:"extra,omitempty"`
}

// Failed reports whether any non-skip probe failed.
func (r *Report) Failed() bool {
	for _, p := range r.Results {
		if p.Status == StatusFail {
			return true
		}
	}
	return false
}

// Print writes a human-readable report to w.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "openviking-doctor %s\n", r.Command)
	fmt.Fprintf(w, "started: %s\n", r.StartedAt.Format(time.RFC3339))
	for _, p := range r.Results {
		fmt.Fprintf(w, "  [%s] %s", strings.ToUpper(string(p.Status)), p.Name)
		if p.LatencyMS > 0 {
			fmt.Fprintf(w, " (%dms)", p.LatencyMS)
		}
		if p.Detail != "" {
			fmt.Fprintf(w, ": %s", p.Detail)
		}
		if p.Error != "" {
			fmt.Fprintf(w, " — %s", p.Error)
		}
		fmt.Fprintln(w)
	}
}

// PrintJSON writes the report as a single line of JSON to w.
func (r *Report) PrintJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// sortResults sorts a copy of results by name for stable output.
func sortResults(rs []ProbeResult) []ProbeResult {
	out := append([]ProbeResult(nil), rs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// runWithTimeout wraps fn in a context that cancels after the given timeout.
// fn receives a child context. The latency is measured around fn's execution.
func runWithTimeout(parent context.Context, timeout time.Duration, fn func(ctx context.Context) error) (int64, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	start := time.Now()
	err := fn(ctx)
	return time.Since(start).Milliseconds(), err
}
