// Package hooks dispatches outbound webhooks and in-process event
// handlers. The bot calls Dispatch when a notable event occurs
// (incoming message, agent reply, tool call). Each registered URL
// receives a JSON POST with the event payload; each registered Hook
// runs in-process and can call internal APIs directly.
//
// Dispatch is fire-and-forget: failures are logged but do not block
// the bot's main loop. URL POSTs use a 10-second timeout per request
// (overridable via config). Hook handlers run with the same ctx as
// Dispatch; hooks should respect ctx.Done() for cancellation.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// Event is the payload POSTed to each hook URL and passed to each Hook.
type Event struct {
	Type      string    `json:"type"` // "incoming", "reply", "tool_call"
	Channel   string    `json:"channel"`
	ChatID    string    `json:"chat_id"`
	UserID    string    `json:"user_id,omitempty"`
	Text      string    `json:"text,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Metadata  any       `json:"metadata,omitempty"`
}

// Hook is an in-process event handler. Unlike URL-based hooks, Hooks
// run inside the bot process and can call internal APIs (memory, skill
// registry, observability) directly. Implementations must be safe for
// concurrent use.
//
// Handle is called once per event. The returned error is logged but
// does not stop dispatch — every Hook and URL is attempted.
//
// Name returns a stable identifier for logging and status reporting.
// It should be lowercase, hyphen-separated, and unique across the
// registered hooks (e.g. "auto-memory", "auto-skill").
type Hook interface {
	Handle(ctx context.Context, ev Event) error
	Name() string
}

// HookFunc lets a plain function satisfy Hook. The Name defaults to
// "func" — wrap in a named struct when a meaningful name is needed.
type HookFunc func(ctx context.Context, ev Event) error

// Handle implements Hook.
func (f HookFunc) Handle(ctx context.Context, ev Event) error { return f(ctx, ev) }

// Name implements Hook — returns "func" since functions have no name.
func (f HookFunc) Name() string { return "func" }

// Dispatcher dispatches Events to a list of URLs and in-process Hooks.
// It is safe for concurrent use.
type Dispatcher struct {
	urls    []string
	hooks   []Hook
	timeout time.Duration
	client  *http.Client
	wg      sync.WaitGroup
}

// New returns a Dispatcher from config.
func New(cfg config.HooksConfig) *Dispatcher {
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Dispatcher{
		urls:    append([]string(nil), cfg.URLs...),
		timeout: timeout,
		client:  &http.Client{Timeout: timeout},
	}
}

// RegisterHook adds an in-process Hook. Hooks are dispatched
// concurrently with URL POSTs. Panics on nil hook to fail fast on
// misconfiguration at startup.
func (d *Dispatcher) RegisterHook(h Hook) {
	if h == nil {
		panic("hooks: RegisterHook called with nil Hook")
	}
	d.hooks = append(d.hooks, h)
}

// Dispatch POSTs ev as JSON to every configured URL and invokes every
// registered Hook. It blocks until all POSTs and hooks complete (or
// time out / ctx cancels). Errors are aggregated but do not stop the
// dispatch — every URL and hook is attempted.
func (d *Dispatcher) Dispatch(ctx context.Context, ev Event) {
	if len(d.urls) == 0 && len(d.hooks) == 0 {
		return
	}
	body, _ := json.Marshal(ev)
	for _, url := range d.urls {
		d.wg.Add(1)
		go func(url string) {
			defer d.wg.Done()
			d.post(ctx, url, body)
		}(url)
	}
	for _, h := range d.hooks {
		d.wg.Add(1)
		go func(h Hook) {
			defer d.wg.Done()
			_ = h.Handle(ctx, ev)
		}(h)
	}
	d.wg.Wait()
}

func (d *Dispatcher) post(ctx context.Context, url string, body []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}

// URLs returns the configured hook URLs (for status reporting).
func (d *Dispatcher) URLs() []string {
	out := make([]string, len(d.urls))
	copy(out, d.urls)
	return out
}

// HookNames returns the names of the registered in-process hooks.
func (d *Dispatcher) HookNames() []string {
	out := make([]string, len(d.hooks))
	for i, h := range d.hooks {
		out[i] = h.Name()
	}
	return out
}

// String returns a compact description for logging.
func (d *Dispatcher) String() string {
	return fmt.Sprintf("hooks.Dispatcher{urls: %d, hooks: %d, timeout: %s}", len(d.urls), len(d.hooks), d.timeout)
}
