package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// FeedbackStatsCmd builds `ov feedback-stats`. It aggregates user
// feedback (thumbs up/down) over a lookback window and prints a table
// with columns: period, positive, negative, net_score.
//
// When no feedback data is available (the default stub store returns
// ErrNoFeedbackData), the command prints "no feedback data" and exits 0.
func FeedbackStatsCmd(rt *Runtime) *cobra.Command {
	var (
		sinceStr string
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:   "feedback-stats",
		Short: "Aggregate user feedback (thumbs up/down)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			since, err := parseSince(sinceStr)
			if err != nil {
				return err
			}
			store := rt.Feedback
			if store == nil {
				store = NewStubFeedbackStore()
			}
			rows, err := store.Stats(cmd.Context(), since)
			if err != nil {
				if errors.Is(err, ErrNoFeedbackData) {
					fmt.Fprintln(rt.Out, "no feedback data")
					return nil
				}
				return err
			}
			if len(rows) == 0 {
				fmt.Fprintln(rt.Out, "no feedback data")
				return nil
			}
			if jsonOut {
				enc := json.NewEncoder(rt.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			headers := []string{"PERIOD", "POSITIVE", "NEGATIVE", "NET_SCORE"}
			tableRows := make([][]string, 0, len(rows))
			for _, r := range rows {
				period := r.Period
				if period == "" {
					period = "all"
				}
				tableRows = append(tableRows, []string{
					period,
					strconv.Itoa(r.Positive),
					strconv.Itoa(r.Negative),
					strconv.Itoa(r.NetScore),
				})
			}
			rt.PrintTable(headers, tableRows)
			return nil
		},
	}
	cmd.Flags().StringVar(&sinceStr, "since", "7d", "lookback window (e.g. 7d, 24h, 30m)")
	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "raw JSON output")
	return cmd
}

// parseSince parses a Go duration string with day suffix support ("7d").
// Empty string means "all time" (zero duration).
func parseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("cli: --since: invalid days %q: %w", s, err)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("cli: --since: %w", err)
	}
	return d, nil
}
