package channels

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// OpenAPI is the OpenViking HTTP chat API channel. It exposes a small
// REST surface (/chat, /chat/stream, /health, /sessions, /feedback)
// that external callers hit to drive the agent loop. Inbound HTTP
// requests are converted to IncomingMessages and dispatched to the
// handler; the agent's reply is delivered back via Send, which looks
// up the pending request by session ID and unblocks the HTTP handler.
//
// This mirrors the Python openapi.py request/response pattern: the
// channel does not subscribe to a platform stream; instead each
// inbound HTTP request creates a PendingResponse, publishes an
// InboundMessage, and blocks until Send delivers the reply.
type OpenAPI struct {
	name string
	cfg  config.ChannelConfig
	api  openAPIAPI
}

// openAPIAPI is the subset of the HTTP server the adapter uses. The
// real implementation serves real routes; tests inject a stub.
type openAPIAPI interface {
	Start(ctx context.Context, handler Handler) error
	Send(ctx context.Context, msg OutgoingMessage) error
}

// openAPIPending tracks one in-flight chat request.
type openAPIPending struct {
	events      []openAPIEvent
	final       string
	responseID  string
	done        chan struct{}
	streamCh    chan *openAPIEvent
	mu          sync.Mutex
	relevant    string
	tokenUsage  map[string]int
}

type openAPIEvent struct {
	Type      string                 `json:"event"`
	Data      any                    `json:"data"`
	Timestamp string                 `json:"timestamp"`
}

func newPending() *openAPIPending {
	return &openAPIPending{
		done:     make(chan struct{}),
		streamCh: make(chan *openAPIEvent, 64),
	}
}

func (p *openAPIPending) addEvent(t string, data any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ev := openAPIEvent{
		Type:      t,
		Data:      data,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	p.events = append(p.events, ev)
	select {
	case p.streamCh <- &ev:
	default:
		// Drop intermediate events when the consumer is not keeping up;
		// the final response always unblocks via done.
	}
}

func (p *openAPIPending) setFinal(content, responseID string) {
	p.mu.Lock()
	p.final = content
	p.responseID = responseID
	p.mu.Unlock()
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

func (p *openAPIPending) closeStream() {
	// Sentinel nil to signal end-of-stream. Don't close the channel —
	// the streaming reader may still be selecting on it.
	select {
	case <-p.done:
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// signal EOF by sending nil; reader treats nil as end.
	select {
	case p.streamCh <- nil:
	default:
	}
}

// openAPIServer is the production openAPIAPI: a real net/http server.
type openAPIServer struct {
	cfg       config.ChannelConfig
	settings  openAPISettings
	srv       *http.Server
	listener  net.Listener
	tenants   *TenantRegistry

	mu       sync.RWMutex
	pending  map[string]*openAPIPending
	sessions map[string]*openAPISession
}

// openAPISession is the per-session state the channel tracks.
type openAPISession struct {
	UserID       string    `json:"user_id"`
	CreatedAt    time.Time `json:"created_at"`
	LastActive   time.Time `json:"last_active"`
	MessageCount int       `json:"message_count"`
}

// openAPISettings is the typed view of cfg.Extra for the OpenAPI channel.
type openAPISettings struct {
	ListenAddr  string
	GatewayToken string
	TimeoutSec  int
}

func parseOpenAPISettings(cfg config.ChannelConfig) openAPISettings {
	addr := cfg.Endpoint
	if addr == "" {
		addr = ":0"
	}
	token := cfg.Token
	if token == "" {
		token = cfg.WebhookSecret
	}
	timeout := extraInt(cfg.Extra, "timeout_seconds", 300)
	return openAPISettings{
		ListenAddr:  addr,
		GatewayToken: token,
		TimeoutSec:  timeout,
	}
}

func newOpenAPIServer(cfg config.ChannelConfig) (*openAPIServer, error) {
	s := &openAPIServer{
		cfg:       cfg,
		settings:  parseOpenAPISettings(cfg),
		pending:   make(map[string]*openAPIPending),
		sessions:  make(map[string]*openAPISession),
		tenants:   NewTenantRegistry(),
	}
	s.initTenantsFromConfig()
	return s, nil
}

// Start implements openAPIAPI. It binds a listener, registers the
// routes, and runs http.Server.Serve until ctx is canceled.
func (s *openAPIServer) Start(ctx context.Context, handler Handler) error {
	ln, err := net.Listen("tcp", s.settings.ListenAddr)
	if err != nil {
		return fmt.Errorf("openapi: listen %s: %w", s.settings.ListenAddr, err)
	}
	s.listener = ln
	s.srv = &http.Server{
		Handler:      s.routes(handler),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 600 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// Send implements openAPIAPI. It delivers the agent's reply to the
// pending request identified by msg.ChatID (session_id).
func (s *openAPIServer) Send(ctx context.Context, msg OutgoingMessage) error {
	s.mu.RLock()
	p, ok := s.pending[msg.ChatID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("openapi: no pending request for session %q", msg.ChatID)
	}
	p.addEvent("response", map[string]any{
		"content":     msg.Text,
		"response_id": msg.ReplyToMsgID,
	})
	p.relevant = ""
	p.setFinal(msg.Text, msg.ReplyToMsgID)
	p.closeStream()
	return nil
}

// routes builds the http.Handler with all OpenAPI endpoints.
func (s *openAPIServer) routes(handler Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/chat", func(w http.ResponseWriter, r *http.Request) {
		s.handleChat(w, r, handler, "")
	})
	mux.HandleFunc("/chat/stream", func(w http.ResponseWriter, r *http.Request) {
		s.handleChatStream(w, r, handler, "")
	})
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		s.handleSessions(w, r, "")
	})
	mux.HandleFunc("/sessions/", func(w http.ResponseWriter, r *http.Request) {
		s.handleSessionByPath(w, r, "")
	})
	mux.HandleFunc("/feedback", func(w http.ResponseWriter, r *http.Request) {
		s.handleFeedback(w, r, handler, "")
	})
	mux.HandleFunc("/tenants/", s.handleTenantRoute(handler))
	return s.withAuth(mux)
}

// withAuth wraps the mux with X-Gateway-Token verification. When no
// gateway token is configured, requests are allowed (the channel is
// assumed to be loopback-only). When a token is set, callers must
// present a matching X-Gateway-Token header.
func (s *openAPIServer) withAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.settings.GatewayToken != "" {
			got := r.Header.Get("X-Gateway-Token")
			if got == "" {
				writeJSONError(w, http.StatusUnauthorized, "X-Gateway-Token header required")
				return
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.settings.GatewayToken)) != 1 {
				writeJSONError(w, http.StatusForbidden, "Invalid API key")
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func (s *openAPIServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
}

// chatRequest is the JSON body for /chat and /chat/stream.
type chatRequest struct {
	Message   string            `json:"message"`
	SessionID string            `json:"session_id"`
	UserID    string            `json:"user_id"`
	Stream    bool              `json:"stream"`
	Metadata  map[string]any    `json:"metadata,omitempty"`
}

// chatResponse is the JSON body for /chat responses.
type chatResponse struct {
	SessionID  string         `json:"session_id"`
	ResponseID string         `json:"response_id,omitempty"`
	Message    string         `json:"message"`
	Events     []openAPIEvent `json:"events,omitempty"`
}

func (s *openAPIServer) handleChat(w http.ResponseWriter, r *http.Request, handler Handler, tenantID string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req chatRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	sessionID = tenantScope(tenantID, sessionID)
	userID := req.UserID
	if userID == "" {
		userID = "anonymous"
	}
	s.touchSession(sessionID, userID)
	p := newPending()
	s.storePending(sessionID, p)
	defer s.dropPending(sessionID)
	im := IncomingMessage{
		ChannelName: s.cfg.Provider,
		ChatID:      sessionID,
		UserID:      userID,
		Text:        req.Message,
		Identity: domain.Identifier{
			Account:   s.cfg.AppID,
			ActorPeer: "openapi",
		},
		Raw: req,
	}
	if err := handler(r.Context(), im); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	timeout := time.Duration(s.settings.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 300 * time.Second
	}
	select {
	case <-p.done:
	case <-time.After(timeout):
		writeJSONError(w, http.StatusGatewayTimeout, "request timeout")
		return
	case <-r.Context().Done():
		return
	}
	p.mu.Lock()
	final := p.final
	rid := p.responseID
	events := append([]openAPIEvent(nil), p.events...)
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, chatResponse{
		SessionID:  sessionID,
		ResponseID: rid,
		Message:    final,
		Events:     events,
	})
}

func (s *openAPIServer) handleChatStream(w http.ResponseWriter, r *http.Request, handler Handler, tenantID string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req chatRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	sessionID = tenantScope(tenantID, sessionID)
	userID := req.UserID
	if userID == "" {
		userID = "anonymous"
	}
	s.touchSession(sessionID, userID)
	p := newPending()
	s.storePending(sessionID, p)
	defer s.dropPending(sessionID)
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	im := IncomingMessage{
		ChannelName: s.cfg.Provider,
		ChatID:      sessionID,
		UserID:      userID,
		Text:        req.Message,
		Identity: domain.Identifier{
			Account:   s.cfg.AppID,
			ActorPeer: "openapi",
		},
		Raw: req,
	}
	if err := handler(r.Context(), im); err != nil {
		writeSSEError(w, flusher, err.Error())
		return
	}
	timeout := time.Duration(s.settings.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 300 * time.Second
	}
	for {
		select {
		case ev := <-p.streamCh:
			if ev == nil {
				return
			}
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-time.After(timeout):
			errEv := openAPIEvent{Type: "response", Data: map[string]any{"error": "timeout"}}
			data, _ := json.Marshal(errEv)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			return
		case <-r.Context().Done():
			return
		}
	}
}

// sessionListResponse is the JSON body for GET /sessions.
type sessionListResponse struct {
	Sessions []openAPISessionInfo `json:"sessions"`
	Total    int                  `json:"total"`
}

type openAPISessionInfo struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	CreatedAt    time.Time `json:"created_at"`
	LastActive   time.Time `json:"last_active"`
	MessageCount int       `json:"message_count"`
}

func (s *openAPIServer) handleSessions(w http.ResponseWriter, r *http.Request, tenantID string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			UserID  string         `json:"user_id"`
			Meta    map[string]any `json:"metadata,omitempty"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		id := uuid.NewString()
		id = tenantScope(tenantID, id)
		now := time.Now().UTC()
		s.mu.Lock()
		s.sessions[id] = &openAPISession{
			UserID:     body.UserID,
			CreatedAt:  now,
			LastActive: now,
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "created_at": now})
		return
	}
	// GET: list sessions in the current scope. When tenantID is set, only
	// sessions whose ID starts with "tenantID:" are returned; otherwise
	// all sessions are listed (single-tenant mode).
	prefix := tenantID
	if prefix != "" {
		prefix += ":"
	}
	s.mu.RLock()
	out := make([]openAPISessionInfo, 0, len(s.sessions))
	for id, ss := range s.sessions {
		if prefix != "" && !strings.HasPrefix(id, prefix) {
			continue
		}
		out = append(out, openAPISessionInfo{
			ID:           id,
			UserID:       ss.UserID,
			CreatedAt:    ss.CreatedAt,
			LastActive:   ss.LastActive,
			MessageCount: ss.MessageCount,
		})
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, sessionListResponse{Sessions: out, Total: len(out)})
}

func (s *openAPIServer) handleSessionByPath(w http.ResponseWriter, r *http.Request, tenantID string) {
	id := strings.TrimPrefix(r.URL.Path, "/sessions/")
	id = strings.Trim(id, "/")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "session id required")
		return
	}
	scopedID := tenantScope(tenantID, id)
	if r.Method == http.MethodDelete {
		s.mu.Lock()
		delete(s.sessions, scopedID)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.RLock()
	ss, ok := s.sessions[scopedID]
	s.mu.RUnlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, openAPISessionInfo{
		ID:           scopedID,
		UserID:       ss.UserID,
		CreatedAt:    ss.CreatedAt,
		LastActive:   ss.LastActive,
		MessageCount: ss.MessageCount,
	})
}

// feedbackRequest is the JSON body for /feedback.
type feedbackRequest struct {
	SessionID      string  `json:"session_id"`
	ResponseID     string  `json:"response_id"`
	UserID         string  `json:"user_id,omitempty"`
	FeedbackType   string  `json:"feedback_type"`
	FeedbackScore  float64 `json:"feedback_score,omitempty"`
	FeedbackReason string  `json:"feedback_reason,omitempty"`
	FeedbackText   string  `json:"feedback_text,omitempty"`
}

func (s *openAPIServer) handleFeedback(w http.ResponseWriter, r *http.Request, handler Handler, tenantID string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req feedbackRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.SessionID == "" || req.ResponseID == "" || req.FeedbackType == "" {
		writeJSONError(w, http.StatusBadRequest, "session_id, response_id, feedback_type are required")
		return
	}
	scopedSession := tenantScope(tenantID, req.SessionID)
	// Persist feedback as an inbound event so the agent loop can record it.
	im := IncomingMessage{
		ChannelName: s.cfg.Provider,
		ChatID:      scopedSession,
		UserID:      req.UserID,
		Text:        fmt.Sprintf("[feedback] type=%s score=%g reason=%s text=%s", req.FeedbackType, req.FeedbackScore, req.FeedbackReason, req.FeedbackText),
		Identity: domain.Identifier{
			Account:   s.cfg.AppID,
			ActorPeer: "openapi",
		},
		Raw: req,
	}
	_ = handler(r.Context(), im)
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted":     true,
		"response_id":  req.ResponseID,
		"session_id":   scopedSession,
		"feedback_type": req.FeedbackType,
		"timestamp":    time.Now().UTC(),
	})
}

// touchSession creates or updates the session entry for sessionID.
func (s *openAPIServer) touchSession(sessionID, userID string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss, ok := s.sessions[sessionID]; ok {
		ss.LastActive = now
		ss.MessageCount++
		return
	}
	s.sessions[sessionID] = &openAPISession{
		UserID:       userID,
		CreatedAt:    now,
		LastActive:   now,
		MessageCount: 1,
	}
}

func (s *openAPIServer) storePending(sessionID string, p *openAPIPending) {
	s.mu.Lock()
	s.pending[sessionID] = p
	s.mu.Unlock()
}

func (s *openAPIServer) dropPending(sessionID string) {
	s.mu.Lock()
	delete(s.pending, sessionID)
	s.mu.Unlock()
}

// decodeJSONBody reads r.Body into v, capped at 1 MiB.
func decodeJSONBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("empty body")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	return nil
}

// writeJSON marshals v as JSON and writes it with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError writes a {"error": ..., "detail": ...} JSON error.
func writeJSONError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"error": http.StatusText(status), "detail": detail})
}

// writeSSEError writes an SSE-formatted error event to the response.
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, detail string) {
	ev := openAPIEvent{Type: "response", Data: map[string]any{"error": detail}}
	data, _ := json.Marshal(ev)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// NewOpenAPI constructs an OpenAPI adapter.
func NewOpenAPI(name string, cfg config.ChannelConfig) *OpenAPI {
	return &OpenAPI{name: name, cfg: cfg}
}

// Connect initializes the HTTP server. The api argument (when
// non-nil) replaces the real server — used by tests to inject a stub.
func (o *OpenAPI) Connect(ctx context.Context, api openAPIAPI) error {
	if api != nil {
		o.api = api
		return nil
	}
	srv, err := newOpenAPIServer(o.cfg)
	if err != nil {
		return err
	}
	o.api = srv
	return nil
}

// Name implements Channel.
func (o *OpenAPI) Name() string { return o.name }

// Start implements Channel.
func (o *OpenAPI) Start(ctx context.Context, handler Handler) error {
	if o.api == nil {
		return fmt.Errorf("openapi: not connected")
	}
	return o.api.Start(ctx, handler)
}

// Send implements Channel.
func (o *OpenAPI) Send(ctx context.Context, msg OutgoingMessage) error {
	if o.api == nil {
		return fmt.Errorf("openapi: not connected")
	}
	return o.api.Send(ctx, msg)
}
