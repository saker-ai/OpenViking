package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// QQ is the QQ Bot channel adapter.
//
// The QQ Bot open platform (bot.q.qq.com) exposes two inbound transports:
//   - webhook: QQ calls our /qq/callback HTTP endpoint on each event.
//   - websocket: we dial wss://api.sgroup.qq.com/websockets, accept
//     IDENTIFY/RESUMED/READY op frames, send HEARTBEAT ping every ~30s,
//     and dispatch MESSAGE_CREATE events.
//
// Outbound messages are HTTPS POSTs to https://api.sgroup.qq.com/v2/...
// regardless of inbound transport. Authentication uses app_access_token
// (issued by https://bots.qq.com/app/getAppAccessToken) which expires
// every 2 hours; this adapter refreshes it 5 minutes before expiry via
// a background goroutine.
//
// Group vs C2C message routing uses the GroupOpenID / UserOpenID
// carried in each inbound event.
type QQ struct {
	name    string
	cfg     config.ChannelConfig
	api     qqAPI
	tokenFn func() string
}

// qqAPI is the subset of the QQ Bot inbound + outbound API the adapter
// uses. Real implementations: qqHTTPAPI (webhook) and qqGatewayAPI
// (websocket). Tests inject a stub via httptest.NewServer.
type qqAPI interface {
	Start(ctx context.Context, handler func(ctx context.Context, ev *qqInboundEvent) error) error
	Send(ctx context.Context, msg qqOutbound) error
}

// qqInboundEvent is the QQ Bot webhook / gateway payload. Fields are
// the union of the group / C2C message events; empty when not
// applicable.
//
// See https://bot.q.qq.com/wiki/develop/api-v2/server-emit/message-events.html
type qqInboundEvent struct {
	// EventType is "GROUP_AT_MESSAGE_CREATE" or "C2C_MESSAGE_CREATE".
	EventType string `json:"event_type,omitempty" `
	// MsgID is the message ID, used to reply via the QQ API.
	MsgID string `json:"msg_id,omitempty"`
	// GroupOpenID is set for group messages.
	GroupOpenID string `json:"group_openid,omitempty"`
	// UserOpenID is set for C2C messages.
	UserOpenID string `json:"user_openid,omitempty"`
	// AuthorID is the sender's openid.
	AuthorID string `json:"author_id,omitempty"`
	// Content is the message text (may include @bot mention).
	Content string `json:"content,omitempty"`
	// Timestamp is the unix seconds string.
	Timestamp string `json:"timestamp,omitempty"`
}

// qqOutbound is the QQ Bot outbound message body.
type qqOutbound struct {
	// Path is the API path, e.g. /v2/groups/{openid}/messages.
	Path string `json:"path"`
	// MsgType is "text" (0) or "markdown" (3).
	MsgType string `json:"msg_type"`
	// Content is {"content": "..."} for text messages.
	Content map[string]string `json:"content"`
	// MsgID is the inbound message ID (for passive reply).
	MsgID string `json:"msg_id,omitempty"`
}

// qqHTTPAPI is the webhook-inbound + HTTP-outbound qqAPI implementation.
type qqHTTPAPI struct {
	cfg        config.ChannelConfig
	tokenFn    func() string
	srv        *http.Server
	listenAddr string
	handlerFn  func(ctx context.Context, ev *qqInboundEvent) error
}

func newQQHTTPAPI(cfg config.ChannelConfig, tokenFn func() string) *qqHTTPAPI {
	return &qqHTTPAPI{cfg: cfg, tokenFn: tokenFn, listenAddr: ":0"}
}

// handler returns the http.Handler that serves the webhook endpoint.
// Exposed so tests can drive it via httptest.NewServer without binding
// a real listener.
func (q *qqHTTPAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/qq/callback", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var envelope struct {
			EventName string          `json:"event_name"`
			Data      json.RawMessage `json:"d"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		ev := &qqInboundEvent{EventType: envelope.EventName}
		_ = json.Unmarshal(envelope.Data, ev)
		if ev.EventType == "" {
			ev.EventType = envelope.EventName
		}
		_ = q.handlerFn(r.Context(), ev)
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// Start launches the webhook server. It blocks until ctx is canceled.
func (q *qqHTTPAPI) Start(ctx context.Context, handlerFn func(ctx context.Context, ev *qqInboundEvent) error) error {
	q.handlerFn = handlerFn
	addr := q.cfg.Endpoint
	if addr == "" {
		addr = q.listenAddr
	}
	q.srv = &http.Server{
		Addr:    addr,
		Handler: q.handler(),
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = q.srv.Shutdown(shutdownCtx)
	}()
	if err := q.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("qq: webhook server: %w", err)
	}
	return nil
}

// Send implements qqAPI by POSTing to the QQ Bot v2 API.
func (q *qqHTTPAPI) Send(ctx context.Context, msg qqOutbound) error {
	body, _ := json.Marshal(msg.Content)
	url := strings.TrimRight(q.cfg.BaseURL, "/") + msg.Path
	if url == msg.Path {
		// BaseURL was empty — fall back to the official endpoint.
		url = "https://api.sgroup.qq.com" + msg.Path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("qq: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if q.tokenFn != nil {
		if tok := q.tokenFn(); tok != "" {
			req.Header.Set("Authorization", "QQBot "+tok)
		}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("qq: send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qq: send: http %d", resp.StatusCode)
	}
	return nil
}

// --- Access token refresh --------------------------------------------------

// qqTokenRefresher fetches and caches the QQ Bot app_access_token.
//
// QQ Bot access tokens expire 2 hours after issue. We refresh 5
// minutes before expiry via a background goroutine; callers consume the
// cached value via Token(). The refresh endpoint is
// https://bots.qq.com/app/getAppAccessToken and takes a JSON body
// {appId, clientSecret}. See:
//   https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-fram/api-token.html
//
// The refresher is safe for concurrent use. If a refresh fails, the
// last error is stored and surfaced on the next Token() call; the
// previous valid token (if any) keeps being served until it expires.
type qqTokenRefresher struct {
	appID     string
	appSecret string
	baseURL   string // override for tests; defaults to https://bots.qq.com
	httpc     *http.Client

	mu         sync.Mutex
	accessToken string
	expiresAt   time.Time
	lastErr     error
	stopCh    chan struct{}
	stopped   bool
}

// newQQTokenRefresher constructs a refresher. httpc may be nil; the
// default client has a 10s timeout.
func newQQTokenRefresher(appID, appSecret, baseURL string, httpc *http.Client) *qqTokenRefresher {
	if httpc == nil {
		httpc = &http.Client{Timeout: 10 * time.Second}
	}
	if baseURL == "" {
		baseURL = "https://bots.qq.com"
	}
	return &qqTokenRefresher{
		appID:     appID,
		appSecret: appSecret,
		baseURL:   strings.TrimRight(baseURL, "/"),
		httpc:     httpc,
		stopCh:    make(chan struct{}),
	}
}

// qqAccessTokenResponse is the wire shape of /app/getAppAccessToken.
type qqAccessTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"` // seconds
	// Error fields the API returns on failure.
	Code int    `json:"code,omitempty"`
	Msg  string `json:"message,omitempty"`
}

// Start begins the background refresh loop. It blocks until ctx is
// canceled or Stop() is called. The first fetch is synchronous so
// callers have a valid token (or a clear error) when Start returns.
//
// On a successful fetch, subsequent refreshes are scheduled 5 minutes
// before the token's expiry. On failure, Start returns the error and
// does not start the loop.
func (r *qqTokenRefresher) Start(ctx context.Context) error {
	if err := r.refresh(ctx); err != nil {
		return err
	}
	go r.loop(ctx)
	return nil
}

// Stop cancels the refresh loop. It is safe to call after the context
// passed to Start is already canceled.
func (r *qqTokenRefresher) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.stopped = true
	close(r.stopCh)
}

// Token returns the current cached access token. Returns the empty
// string and the last refresh error if no token is cached.
func (r *qqTokenRefresher) Token() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accessToken == "" && r.lastErr != nil {
		return "", r.lastErr
	}
	return r.accessToken, nil
}

// loop runs the periodic refresh. It exits when the context is canceled
// or Stop() is called.
func (r *qqTokenRefresher) loop(ctx context.Context) {
	for {
		r.mu.Lock()
		expiresAt := r.expiresAt
		r.mu.Unlock()
		// Refresh 5 minutes before expiry; clamp to >=1s so we don't
		// busy-loop when the server returns a tiny expires_in.
		delay := time.Until(expiresAt) - 5*time.Minute
		if delay < time.Second {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-time.After(delay):
		}
		if err := r.refresh(ctx); err != nil {
			// Surface the error on the next Token() call; keep the
			// previous token until it expires. If the context is
			// canceled, exit quietly.
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// refresh fetches a new app_access_token and caches it.
func (r *qqTokenRefresher) refresh(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{
		"appId":        r.appID,
		"clientSecret": r.appSecret,
	})
	url := r.baseURL + "/app/getAppAccessToken"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		r.storeErr(err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.httpc.Do(req)
	if err != nil {
		err = fmt.Errorf("qq: refresh token: %w", err)
		r.storeErr(err)
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		err = fmt.Errorf("qq: refresh token: http %d: %s", resp.StatusCode, string(data))
		r.storeErr(err)
		return err
	}
	var out qqAccessTokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		err = fmt.Errorf("qq: refresh token: unmarshal: %w", err)
		r.storeErr(err)
		return err
	}
	if out.AccessToken == "" {
		err = fmt.Errorf("qq: refresh token: empty token (code=%d msg=%q)", out.Code, out.Msg)
		r.storeErr(err)
		return err
	}
	r.mu.Lock()
	r.accessToken = out.AccessToken
	r.expiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	r.lastErr = nil
	r.mu.Unlock()
	return nil
}

func (r *qqTokenRefresher) storeErr(err error) {
	r.mu.Lock()
	r.lastErr = err
	r.mu.Unlock()
}

// --- WebSocket gateway -----------------------------------------------------

// qqGatewayAPI is the qqAPI implementation that consumes inbound events
// from the QQ WebSocket gateway. Outbound Send is identical to the
// webhook API (HTTPS POST to /v2/...).
//
// Gateway protocol (per https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-fram/websocket.html):
//   1. Connect to wss://api.sgroup.qq.com/websockets.
//   2. Receive op=10 HELLO with heartbeat_interval.
//   3. Send op=2 IDENTIFY with the app_access_token.
//   4. Receive op=0 READY; from here on, MESSAGE_CREATE events arrive
//      as op=0 with t="GROUP_AT_MESSAGE_CREATE" or "C2C_MESSAGE_CREATE".
//   5. Send op=1 HEARTBEAT every heartbeat_interval ms.
//   6. On disconnect, reconnect with exponential backoff and op=6 RESUME.
//
// We use coder/websocket for the underlying transport. Reconnect uses
// a backoff of 1s, 2s, 4s, ..., capped at 30s.
type qqGatewayAPI struct {
	cfg     config.ChannelConfig
	tokenFn func() string

	// dialer lets tests inject a fake connection. When nil, the real
	// coder/websocket dialer is used.
	dialer qqDialer
}

// qqDialer abstracts the websocket connection step so tests can feed
// canned frames without hitting the network.
type qqDialer interface {
	Dial(ctx context.Context, url string, headers map[string]string) (qqConn, error)
}

// qqConn is the minimal connection surface the gateway uses. Both
// *websocket.Conn and the test stub satisfy it.
type qqConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, data []byte) error
	Close() error
}

// qqRealDialer dials the real QQ gateway via coder/websocket.
type qqRealDialer struct{}

func (qqRealDialer) Dial(ctx context.Context, url string, headers map[string]string) (qqConn, error) {
	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{},
	}
	for k, v := range headers {
		opts.HTTPHeader.Set(k, v)
	}
	c, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		return nil, err
	}
	return &wsConn{c: c}, nil
}

func newQQGatewayAPI(cfg config.ChannelConfig, tokenFn func() string, dialer qqDialer) *qqGatewayAPI {
	if dialer == nil {
		dialer = qqRealDialer{}
	}
	return &qqGatewayAPI{cfg: cfg, tokenFn: tokenFn, dialer: dialer}
}

// qqGatewayFrame is the wire shape of a gateway frame. op=0 events
// carry t + s + d; op=10 HELLO carries d.heartbeat.
type qqGatewayFrame struct {
	Op   int             `json:"op"`
	Seq  int             `json:"s,omitempty"`
	Type string          `json:"t,omitempty"`
	Data json.RawMessage `json:"d,omitempty"`
}

// qqGatewayHello is the payload of op=10 HELLO.
type qqGatewayHello struct {
	HeartbeatInterval int `json:"heartbeat_interval"` // ms
}

// qqGatewayIdentify is the payload of op=2 IDENTIFY.
type qqGatewayIdentify struct {
	Token      string `json:"token"`
	Intents    int    `json:"intents"`
	Shard      [2]int `json:"shard,omitempty"`
	Properties struct {
		OS      string `json:"$os"`
		Browser string `json:"$browser"`
		Device  string `json:"$device"`
	} `json:"properties"`
}

// qqGatewayResume is the payload of op=6 RESUME.
type qqGatewayResume struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id"`
	Seq       int    `json:"seq"`
}

// qqGatewayReady is the payload of op=0 READY.
type qqGatewayReady struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
	User      struct {
		ID      string `json:"id"`
		Name    string `json:"username"`
	} `json:"user"`
}

// qqGatewayEvent is the payload of an op=0 message event. It maps to
// qqInboundEvent but is declared separately so we can decode the
// author.id field that QQ nests under "author".
type qqGatewayEvent struct {
	ID         string `json:"id"`
	GroupOpenID string `json:"group_openid"`
	UserOpenID  string `json:"user_openid"`
	Author     struct {
		ID string `json:"id"`
		// MemberOpenID is set for group messages; UserOpenID for C2C.
		UnionOpenID string `json:"union_openid"`
	} `json:"author"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}

// gatewayURL returns the WebSocket gateway URL. cfg.BaseURL overrides
// the official endpoint for tests.
func (g *qqGatewayAPI) gatewayURL() string {
	if g.cfg.BaseURL != "" {
		u := g.cfg.BaseURL
		if strings.HasPrefix(u, "http://") {
			u = "ws://" + strings.TrimPrefix(u, "http://")
		} else if strings.HasPrefix(u, "https://") {
			u = "wss://" + strings.TrimPrefix(u, "https://")
		}
		return strings.TrimRight(u, "/") + "/websockets"
	}
	return "wss://api.sgroup.qq.com/websockets"
}

// Start dials the gateway and dispatches inbound events. It blocks
// until ctx is canceled. On disconnect it reconnects with exponential
// backoff (1s, 2s, 4s, ..., capped at 30s) and resumes via op=6.
func (g *qqGatewayAPI) Start(ctx context.Context, handlerFn func(ctx context.Context, ev *qqInboundEvent) error) error {
	url := g.gatewayURL()
	headers := map[string]string{}
	if g.tokenFn != nil {
		if tok := g.tokenFn(); tok != "" {
			headers["Authorization"] = "QQBot " + tok
		}
	}
	var sessionID string
	var lastSeq int
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		conn, err := g.dialer.Dial(ctx, url, headers)
		if err != nil {
			// Reconnect after backoff.
			g.sleep(ctx, backoff)
			backoff = g.nextBackoff(backoff)
			continue
		}
		ran, err := g.runSession(ctx, conn, headers, &sessionID, &lastSeq, handlerFn)
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !ran {
			// Initial handshake failed; the gateway may be unreachable.
			g.sleep(ctx, backoff)
			backoff = g.nextBackoff(backoff)
			continue
		}
		// Session ran successfully but ended; reset backoff and reconnect.
		backoff = time.Second
	}
}

// runSession runs one gateway session: HELLO -> IDENTIFY/RESUME -> event
// loop. Returns (ran, err) where ran is true if the session reached the
// READY state. lastSeq and sessionID are updated as the session
// progresses so the next session can RESUME.
func (g *qqGatewayAPI) runSession(ctx context.Context, conn qqConn, headers map[string]string, sessionID *string, lastSeq *int, handlerFn func(ctx context.Context, ev *qqInboundEvent) error) (bool, error) {
	// 1. Read HELLO (op=10).
	hello, err := g.readFrame(ctx, conn)
	if err != nil {
		return false, fmt.Errorf("qq gateway: read hello: %w", err)
	}
	if hello.Op != 10 {
		return false, fmt.Errorf("qq gateway: expected op=10 HELLO, got op=%d", hello.Op)
	}
	var hi qqGatewayHello
	_ = json.Unmarshal(hello.Data, &hi)
	heartbeatInterval := hi.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = 30000
	}

	// 2. IDENTIFY or RESUME.
	if *sessionID == "" {
		identify := qqGatewayIdentify{
			Token:   g.token(),
			Intents: 1 << 30, // PUBLIC_GUILD_MESSAGES plus defaults; QQ accepts a single signed intent bit.
		}
		identify.Properties.OS = "linux"
		identify.Properties.Browser = "vikingbot"
		identify.Properties.Device = "vikingbot"
		if err := g.sendJSON(ctx, conn, qqGatewayFrame{Op: 2, Data: mustRawJSON(identify)}); err != nil {
			return false, fmt.Errorf("qq gateway: send identify: %w", err)
		}
	} else {
		resume := qqGatewayResume{Token: g.token(), SessionID: *sessionID, Seq: *lastSeq}
		if err := g.sendJSON(ctx, conn, qqGatewayFrame{Op: 6, Data: mustRawJSON(resume)}); err != nil {
			return false, fmt.Errorf("qq gateway: send resume: %w", err)
		}
	}

	// 3. Heartbeat ticker.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go g.heartbeatLoop(hbCtx, conn, heartbeatInterval, lastSeq)

	// 4. Event loop.
	ready := false
	for {
		frame, err := g.readFrame(ctx, conn)
		if err != nil {
			return ready, fmt.Errorf("qq gateway: read: %w", err)
		}
		switch frame.Op {
		case 0:
			// Dispatch event.
			if frame.Seq > 0 {
				*lastSeq = frame.Seq
			}
			switch frame.Type {
			case "READY":
				var rdy qqGatewayReady
				_ = json.Unmarshal(frame.Data, &rdy)
				*sessionID = rdy.SessionID
				ready = true
			case "RESUMED":
				ready = true
			case "GROUP_AT_MESSAGE_CREATE", "C2C_MESSAGE_CREATE":
				ev := gatewayEventToInbound(frame.Type, frame.Data)
				if ev != nil {
					_ = handlerFn(ctx, ev)
				}
			default:
				// Other events (GUILD_MEMBER_ADD, etc.) are ignored —
				// the bot only handles message events.
			}
		case 11:
			// Heartbeat ACK — no action.
		default:
			// Unknown op; ignore.
		}
	}
}

// heartbeatLoop sends op=1 HEARTBEAT every interval until ctx is canceled.
func (g *qqGatewayAPI) heartbeatLoop(ctx context.Context, conn qqConn, intervalMs int, lastSeq *int) {
	ticker := time.NewTicker(time.Duration(intervalMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			payload := map[string]int{"op": 1, "s": *lastSeq}
			_ = g.sendJSON(ctx, conn, payload)
		}
	}
}

// readFrame reads one JSON frame from the conn.
func (g *qqGatewayAPI) readFrame(ctx context.Context, conn qqConn) (*qqGatewayFrame, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var frame qqGatewayFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return nil, fmt.Errorf("unmarshal frame: %w (data=%q)", err, string(data))
	}
	return &frame, nil
}

// sendJSON marshals v and writes it as a text frame.
func (g *qqGatewayAPI) sendJSON(ctx context.Context, conn qqConn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// token returns the current access token (empty string if none).
func (g *qqGatewayAPI) token() string {
	if g.tokenFn == nil {
		return ""
	}
	return g.tokenFn()
}

// nextBackoff doubles the backoff, capped at 30s.
func (g *qqGatewayAPI) nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// sleep blocks for d or until ctx is canceled.
func (g *qqGatewayAPI) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Send implements qqAPI by POSTing to the QQ Bot v2 API. Same shape as
// qqHTTPAPI.Send — the outbound transport is HTTP regardless of inbound.
func (g *qqGatewayAPI) Send(ctx context.Context, msg qqOutbound) error {
	body, _ := json.Marshal(msg.Content)
	url := strings.TrimRight(g.cfg.BaseURL, "/") + msg.Path
	if url == msg.Path {
		url = "https://api.sgroup.qq.com" + msg.Path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("qq: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if g.tokenFn != nil {
		if tok := g.tokenFn(); tok != "" {
			req.Header.Set("Authorization", "QQBot "+tok)
		}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("qq: send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qq: send: http %d", resp.StatusCode)
	}
	return nil
}

// gatewayEventToInbound decodes an op=0 message event payload into a
// qqInboundEvent. Returns nil for malformed payloads.
func gatewayEventToInbound(eventType string, data []byte) *qqInboundEvent {
	var ge qqGatewayEvent
	if err := json.Unmarshal(data, &ge); err != nil {
		return nil
	}
	ev := &qqInboundEvent{
		EventType:   eventType,
		MsgID:       ge.ID,
		GroupOpenID: ge.GroupOpenID,
		UserOpenID:  ge.UserOpenID,
		AuthorID:    ge.Author.ID,
		Content:     ge.Content,
		Timestamp:   ge.Timestamp,
	}
	return ev
}

// mustRawJSON marshals v to json.RawMessage; returns nil on error (the
// shape is known to be marshalable).
func mustRawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// NewQQ constructs a QQ adapter.
func NewQQ(name string, cfg config.ChannelConfig) *QQ {
	return &QQ{name: name, cfg: cfg}
}

// Connect initializes the inbound API. api (when non-nil) replaces the
// real API — used by tests. tokenFn (when non-nil) is called on each
// Send to obtain a fresh access token.
//
// When api is nil and cfg.Mode is "websocket", the adapter uses the
// QQ WebSocket gateway; otherwise it uses the webhook HTTP server.
// When cfg.AppID and cfg.AppSecret are both set, a background token
// refresher is started and its Token() method backs tokenFn.
func (q *QQ) Connect(ctx context.Context, api qqAPI, tokenFn func() string) error {
	if api != nil {
		q.api = api
		q.tokenFn = tokenFn
		return nil
	}
	// If the caller didn't supply a tokenFn, start the background
	// refresher when credentials are present.
	if tokenFn == nil && q.cfg.AppID != "" && q.cfg.AppSecret != "" {
		refresher := newQQTokenRefresher(q.cfg.AppID, q.cfg.AppSecret, "", nil)
		if err := refresher.Start(ctx); err != nil {
			return fmt.Errorf("qq: token refresh: %w", err)
		}
		tokenFn = func() string {
			tok, _ := refresher.Token()
			return tok
		}
	}
	q.tokenFn = tokenFn
	if q.cfg.Mode == "websocket" {
		q.api = newQQGatewayAPI(q.cfg, tokenFn, nil)
	} else {
		q.api = newQQHTTPAPI(q.cfg, tokenFn)
	}
	return nil
}

// Name implements Channel.
func (q *QQ) Name() string { return q.name }

// Start implements Channel.
func (q *QQ) Start(ctx context.Context, handler Handler) error {
	if q.api == nil {
		return fmt.Errorf("qq: not connected")
	}
	return q.api.Start(ctx, func(ctx context.Context, ev *qqInboundEvent) error {
		im := toIncomingQQ(q.name, q.cfg, ev)
		if im.ChatID == "" || im.Text == "" {
			return nil
		}
		return handler(ctx, im)
	})
}

// Send implements Channel. msg.ChatID is the inbound message's Group
// OpenID or User OpenID; we reconstruct the API path from it.
func (q *QQ) Send(ctx context.Context, msg OutgoingMessage) error {
	if q.api == nil {
		return fmt.Errorf("qq: not connected")
	}
	// The OutgoingMessage.ChatID is set to either "group:<openid>" or
	// "user:<openid>" by toIncomingQQ. We split it back into the API
	// path and target openid.
	path, openid := qqRouteFromChatID(msg.ChatID)
	if path == "" {
		return fmt.Errorf("qq: invalid ChatID %q", msg.ChatID)
	}
	return q.api.Send(ctx, qqOutbound{
		Path:    fmt.Sprintf(path, openid),
		MsgType: "text",
		Content: map[string]string{"content": msg.Text},
		MsgID:   msg.ReplyToMsgID,
	})
}

// toIncomingQQ converts an inbound QQ event to IncomingMessage. The
// ChatID is encoded as "group:<openid>" or "user:<openid>" so the
// outbound Send can route to the right API path.
func toIncomingQQ(name string, cfg config.ChannelConfig, ev *qqInboundEvent) IncomingMessage {
	if ev == nil {
		return IncomingMessage{}
	}
	text := strings.TrimSpace(ev.Content)
	if strings.HasPrefix(text, "@") {
		if sp := strings.IndexByte(text, ' '); sp >= 0 {
			text = strings.TrimSpace(text[sp+1:])
		}
	}
	chatID := ""
	if ev.GroupOpenID != "" {
		chatID = "group:" + ev.GroupOpenID
	} else if ev.UserOpenID != "" {
		chatID = "user:" + ev.UserOpenID
	}
	return IncomingMessage{
		ChannelName: name,
		ChatID:      chatID,
		UserID:      ev.AuthorID,
		Text:        text,
		Identity: domain.Identifier{
			Account:   cfg.AppID,
			ActorPeer: "qq",
		},
		Raw: ev,
	}
}

// qqRouteFromChatID splits "group:<openid>" or "user:<openid>" into the
// QQ Bot API path template and the openid.
func qqRouteFromChatID(chatID string) (path, openid string) {
	switch {
	case strings.HasPrefix(chatID, "group:"):
		openid = strings.TrimPrefix(chatID, "group:")
		return "/v2/groups/%s/messages", openid
	case strings.HasPrefix(chatID, "user:"):
		openid = strings.TrimPrefix(chatID, "user:")
		return "/v2/users/%s/messages", openid
	default:
		return "", ""
	}
}

// Compile-time assertions that the real APIs satisfy qqAPI.
var (
	_ qqAPI = (*qqHTTPAPI)(nil)
	_ qqAPI = (*qqGatewayAPI)(nil)
)
