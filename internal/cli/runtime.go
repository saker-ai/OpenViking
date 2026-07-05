package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/saker-ai/ctxhub/internal/bot/cron"
	"github.com/saker-ai/ctxhub/pkg/i18n"
)

// Runtime carries the shared state for a command execution. The root
// command builds one before invoking RunE; tests can construct a
// Runtime directly with stubbed fields.
type Runtime struct {
	Config      *CLIConfig
	Client      *Client
	TokenSource *TokenSource
	Out         io.Writer
	Err         io.Writer
	In_         io.Reader
	Locale      string

	// CronStore, when non-nil, is the cron job store used by the
	// `ov cron` subcommands. When nil, the cron commands construct a
	// default FileStore at $OV_DATA_DIR/cron/jobs.json. Tests inject a
	// MemoryStore to avoid touching the filesystem.
	CronStore cron.JobStore

	// Feedback, when non-nil, is the feedback store used by `ov
	// feedback-stats`. When nil, the command constructs a
	// StubFeedbackStore that always returns ErrNoFeedbackData.
	Feedback FeedbackStore

	// OAuthURLBuilder, when non-nil, overrides the default OAuth URL
	// builder used by `ov channels login`. Tests inject a stub to
	// assert on the URL without loading a bot config.
	OAuthURLBuilder OAuthURLBuilder
}

// Translator returns the i18n locale for this runtime, falling back to
// the config's locale then the empty default (which lets the i18n
// package detect from env).
func (r *Runtime) Translator() string {
	if r.Locale != "" {
		return r.Locale
	}
	if r.Config != nil {
		return r.Config.Output.Locale
	}
	return ""
}

// T translates a message id with optional template data.
func (r *Runtime) T(id string, data ...map[string]interface{}) string {
	return i18n.Translate(r.Translator(), id, data...)
}

// EnsureClient lazily builds a Client when one is not yet attached.
// Used by commands that need an HTTP connection but were constructed
// before the root flag-parsing completes.
func (r *Runtime) EnsureClient(ctx context.Context) error {
	if r.Client != nil {
		return nil
	}
	cfg := r.Config
	if cfg == nil {
		return fmt.Errorf("cli: no config loaded; run `ov init`")
	}
	r.Client = NewClient(cfg.Server.BaseURL,
		WithAccount(cfg.Account.Name),
		WithUser(cfg.Account.User),
	)
	if cfg.Auth.ClientID != "" && cfg.Auth.ClientSecret != "" {
		if r.TokenSource == nil {
			r.TokenSource = NewTokenSource(cfg.Server.BaseURL, cfg.Auth)
		}
		tok, err := r.TokenSource.Token(ctx)
		if err != nil {
			fmt.Fprintf(r.Err, "warning: %v\n", err)
		} else {
			r.Client.SetToken(tok)
		}
	}
	return nil
}

// PrintTable renders rows as columns. The first row is the header.
// Width is computed per column; cells are padded with spaces.
func (r *Runtime) PrintTable(headers []string, rows [][]string) {
	out := r.Out
	if out == nil {
		return
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	fmt.Fprintln(out, formatRow(headers, widths))
	for _, row := range rows {
		for len(row) < len(widths) {
			row = append(row, "")
		}
		fmt.Fprintln(out, formatRow(row, widths))
	}
}

func formatRow(cells []string, widths []int) string {
	parts := make([]string, len(cells))
	for i, c := range cells {
		w := 0
		if i < len(widths) {
			w = widths[i]
		}
		parts[i] = c + strings.Repeat(" ", w-len(c))
	}
	return strings.Join(parts, "  ")
}

// In returns the input stream for the runtime. Defaults to os.Stdin
// when rt.In_ is nil. Used by crypto commands and the TUI.
func (r *Runtime) In() io.Reader {
	if r.In_ != nil {
		return r.In_
	}
	return os.Stdin
}
