package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// WebSocket is the browser-facing WebSocket channel adapter. It runs
// an HTTP server that upgrades inbound connections to WebSocket and
// treats each connection as a chat session.
//
// Message framing: each inbound frame is a JSON object
// {"text":"..."}; each outbound reply is the same shape. This is a
// deliberately minimal protocol — richer framing can be layered on top
// by the agent.
type WebSocket struct {
	name string
	cfg  config.ChannelConfig
	api  webSocketAPI
}

// webSocketAPI is the subset of the WebSocket server the adapter uses.
// Real implementation wraps a net/http server with coder/websocket
// upgrade; tests inject a stub.
type webSocketAPI interface {
	Start(ctx context.Context, handler func(ctx context.Context, conn webSocketConn, text string) error) error
	Send(ctx context.Context, conn webSocketConn, text string) error
}

// webSocketConn is the minimal connection interface both *websocket.Conn
// and the test stub satisfy.
type webSocketConn interface {
	Write(ctx context.Context, typ websocket.MessageType, data []byte) error
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Close() error
}

// wsServer is the real webSocketAPI implementation.
type wsServer struct {
	cfg     config.ChannelConfig
	handler func(ctx context.Context, conn webSocketConn, text string) error
	srv     *http.Server
	addr    string

	mu    sync.Mutex
	conns map[webSocketConn]struct{}
}

func newWSServer(cfg config.ChannelConfig) *wsServer {
	addr := cfg.Endpoint
	if addr == "" {
		addr = ":0"
	}
	return &wsServer{addr: addr, conns: make(map[webSocketConn]struct{})}
}

// Start launches the HTTP server. It blocks until ctx is canceled.
func (s *wsServer) Start(ctx context.Context, handler func(ctx context.Context, conn webSocketConn, text string) error) error {
	s.handler = handler
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		opts := s.acceptOptions()
		c, err := websocket.Accept(w, r, opts)
		if err != nil {
			return
		}
		conn := &wsConn{c: c}
		s.track(conn)
		defer s.untrack(conn)
		defer c.CloseNow()
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var msg struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				_ = s.handler(r.Context(), conn, string(data))
				continue
			}
			if err := s.handler(r.Context(), conn, msg.Text); err != nil {
				return
			}
		}
	})
	s.srv = &http.Server{Addr: s.addr, Handler: mux}
	go func() {
		<-ctx.Done()
		s.shutdown()
	}()
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("websocket: server: %w", err)
	}
	return nil
}

// acceptOptions builds the coder/websocket AcceptOptions from cfg.
// When cfg.WebhookSecret is set we treat it as a comma-separated list
// of allowed origin hosts; otherwise we accept any origin (the browser
// protocol is permissive by design and auth is via the bot token in
// the query string).
func (s *wsServer) acceptOptions() *websocket.AcceptOptions {
	opts := &websocket.AcceptOptions{}
	if s.cfg.WebhookSecret != "" {
		for _, h := range splitCSV(s.cfg.WebhookSecret) {
			if h = strings.TrimSpace(h); h != "" {
				opts.OriginPatterns = append(opts.OriginPatterns, h)
			}
		}
	}
	if len(opts.OriginPatterns) == 0 {
		// No explicit allowlist — accept any origin. The bot token in
		// the query string is the auth gate.
		opts.InsecureSkipVerify = true
	}
	return opts
}

// splitCSV splits a comma-separated string. Empty input yields nil.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// Send implements webSocketAPI by writing a JSON frame to the conn.
func (s *wsServer) Send(ctx context.Context, conn webSocketConn, text string) error {
	body, _ := json.Marshal(map[string]string{"text": text})
	return conn.Write(ctx, websocket.MessageText, body)
}

func (s *wsServer) track(c webSocketConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[c] = struct{}{}
}

func (s *wsServer) untrack(c webSocketConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

func (s *wsServer) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		_ = c.Close()
	}
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	}
}

// wsConn wraps *websocket.Conn to satisfy webSocketConn.
type wsConn struct {
	c *websocket.Conn
}

func (w *wsConn) Write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	return w.c.Write(ctx, typ, data)
}

func (w *wsConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	return w.c.Read(ctx)
}

func (w *wsConn) Close() error {
	return w.c.CloseNow()
}

// NewWebSocket constructs a WebSocket adapter.
func NewWebSocket(name string, cfg config.ChannelConfig) *WebSocket {
	return &WebSocket{name: name, cfg: cfg}
}

// Connect initializes the WebSocket server.
func (w *WebSocket) Connect(ctx context.Context, api webSocketAPI) error {
	if api != nil {
		w.api = api
		return nil
	}
	w.api = newWSServer(w.cfg)
	return nil
}

// Name implements Channel.
func (w *WebSocket) Name() string { return w.name }

// Start implements Channel.
func (w *WebSocket) Start(ctx context.Context, handler Handler) error {
	if w.api == nil {
		return fmt.Errorf("websocket: not connected")
	}
	return w.api.Start(ctx, func(ctx context.Context, conn webSocketConn, text string) error {
		im := IncomingMessage{
			ChannelName: w.name,
			ChatID:      connID(conn),
			UserID:      connID(conn),
			Text:        text,
			Identity: domain.Identifier{
				Account:   w.cfg.AppID,
				ActorPeer: "websocket",
			},
			Raw: conn,
		}
		if im.Text == "" {
			return nil
		}
		return handler(ctx, im)
	})
}

// Send implements Channel. The WebSocket adapter is connection-oriented:
// replies are written to the originating connection (kept in
// IncomingMessage.Raw) via SendConn. Send without a conn reference
// cannot route the reply, so it returns a descriptive error rather
// than silently dropping the message.
//
// Callers that want to broadcast to all active connections should use
// Broadcast (below).
func (w *WebSocket) Send(ctx context.Context, msg OutgoingMessage) error {
	if w.api == nil {
		return fmt.Errorf("websocket: not connected")
	}
	if msg.ChatID == "" {
		return fmt.Errorf("websocket: Send requires a conn reference; use SendConn with the IncomingMessage.Raw conn, or Broadcast to fan out")
	}
	// ChatID is connID(conn) — we look it up in the active conn set.
	conn := w.lookupConn(msg.ChatID)
	if conn == nil {
		return fmt.Errorf("websocket: no active connection for ChatID %q", msg.ChatID)
	}
	return w.api.Send(ctx, conn, msg.Text)
}

// Broadcast writes msg.Text to every active connection. It is the
// fan-out path for agent replies that aren't tied to a specific conn.
func (w *WebSocket) Broadcast(ctx context.Context, msg OutgoingMessage) error {
	if w.api == nil {
		return fmt.Errorf("websocket: not connected")
	}
	srv, ok := w.api.(*wsServer)
	if !ok {
		return fmt.Errorf("websocket: broadcast requires the real wsServer (got %T)", w.api)
	}
	srv.mu.Lock()
	conns := make([]webSocketConn, 0, len(srv.conns))
	for c := range srv.conns {
		conns = append(conns, c)
	}
	srv.mu.Unlock()
	if len(conns) == 0 {
		return fmt.Errorf("websocket: no active connections to broadcast to")
	}
	var lastErr error
	for _, c := range conns {
		if err := w.api.Send(ctx, c, msg.Text); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// lookupConn returns the active connection with the given connID, or
// nil when the api isn't a *wsServer or no conn matches. The caller
// must not hold the mutex; lookupConn takes it transiently.
func (w *WebSocket) lookupConn(id string) webSocketConn {
	srv, ok := w.api.(*wsServer)
	if !ok {
		return nil
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for c := range srv.conns {
		if connID(c) == id {
			return c
		}
	}
	return nil
}

// SendConn writes msg.Text to the supplied conn. Used by callers that
// have access to the originating connection.
func (w *WebSocket) SendConn(ctx context.Context, conn webSocketConn, msg OutgoingMessage) error {
	if w.api == nil {
		return fmt.Errorf("websocket: not connected")
	}
	return w.api.Send(ctx, conn, msg.Text)
}

// connID returns a stable identifier for a connection. We use the
// pointer address as a cheap unique ID.
func connID(c webSocketConn) string {
	return fmt.Sprintf("%p", c)
}
