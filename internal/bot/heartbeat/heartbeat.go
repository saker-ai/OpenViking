// Package heartbeat pings openviking-server periodically to signal
// that the bot is alive. The ping target is /api/v1/observer (the
// observer router on the server side), and the body carries the bot's
// identity and channel list so the server can surface bot health in
// its observer dashboard.
package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/channels"
	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// Pinger sends liveness pings. The default implementation is HTTP;
// tests inject a stub.
type Pinger interface {
	Ping(ctx context.Context, payload Payload) error
}

// Payload is the ping body. The server's observer router records the
// timestamp and channel list.
type Payload struct {
	Bot      string   `json:"bot"`
	Account  string   `json:"account"`
	User     string   `json:"user,omitempty"`
	Peer     string   `json:"peer,omitempty"`
	Version  string   `json:"version,omitempty"`
	Channels []string `json:"channels"`
}

// Heartbeat runs the ping loop.
type Heartbeat struct {
	cfg      config.HeartbeatConfig
	identity Payload
	pinger   Pinger
	ticker   *time.Ticker
	stopCh   chan struct{}
	mu       sync.Mutex
	stopped  bool
}

// New returns a Heartbeat that pings pinger (or httpPinger when nil)
// every cfg.Interval seconds. When interval is 0 the loop is disabled.
//
// serverURL is the base URL of the openviking-server (e.g.
// https://openviking.example.com). It is only used by the default
// httpPinger; a custom pinger ignores it.
func New(cfg config.HeartbeatConfig, serverURL string, identity Payload, pinger Pinger) *Heartbeat {
	if pinger == nil {
		pinger = newHTTPPinger(serverURL, cfg.Path)
	}
	return &Heartbeat{
		cfg:      cfg,
		identity: identity,
		pinger:   pinger,
		stopCh:   make(chan struct{}),
	}
}

// SetChannels updates the channel list reported in each ping. Safe to
// call concurrently with Start.
func (h *Heartbeat) SetChannels(dispatcher *channels.Dispatcher) {
	if dispatcher == nil {
		return
	}
	h.identity.Channels = dispatcher.Names()
}

// Start launches the ping loop. It blocks until ctx is canceled or
// Stop is called. When cfg.Interval is 0 it returns immediately.
func (h *Heartbeat) Start(ctx context.Context) error {
	if h.cfg.Interval <= 0 {
		return nil
	}
	interval := time.Duration(h.cfg.Interval) * time.Second
	h.ticker = time.NewTicker(interval)
	defer h.ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.stopCh:
			return nil
		case <-h.ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = h.pinger.Ping(pingCtx, h.identity)
			cancel()
		}
	}
}

// Stop signals the loop to exit. It is idempotent.
func (h *Heartbeat) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	h.stopped = true
	close(h.stopCh)
}

// httpPinger POSTs the payload as JSON to the configured path on the
// openviking-server.
type httpPinger struct {
	serverURL string
	path      string
}

func newHTTPPinger(serverURL, path string) Pinger {
	if path == "" {
		path = "/api/v1/observer"
	}
	return &httpPinger{serverURL: serverURL, path: path}
}

// Ping implements Pinger.
func (p *httpPinger) Ping(ctx context.Context, payload Payload) error {
	url := p.serverURL + p.path
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("heartbeat: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat: ping: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("heartbeat: http %d", resp.StatusCode)
	}
	return nil
}
