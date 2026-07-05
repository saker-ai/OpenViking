package channels

import (
	"context"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

type stubWSAPI struct {
	startHandler func(ctx context.Context, conn webSocketConn, text string) error
	sends        []wsSend
	started      chan struct{}
}

type wsSend struct {
	text string
}

func (s *stubWSAPI) Start(ctx context.Context, handler func(ctx context.Context, conn webSocketConn, text string) error) error {
	s.startHandler = handler
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	return nil
}

func (s *stubWSAPI) Send(ctx context.Context, conn webSocketConn, text string) error {
	s.sends = append(s.sends, wsSend{text: text})
	return nil
}

// stubWSConn is a minimal webSocketConn for tests.
type stubWSConn struct {
	mu  sync.Mutex
	buf []byte
}

func (c *stubWSConn) Write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, data...)
	return nil
}

func (c *stubWSConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	return websocket.MessageText, nil, nil
}

func (c *stubWSConn) Close() error { return nil }

func TestWebSocket_ConnectWithStub(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{Provider: "websocket"})
	stub := &stubWSAPI{}
	if err := w.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestWebSocket_StartDispatchesMessages(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{Provider: "websocket", AppID: "acct"})
	stub := &stubWSAPI{started: make(chan struct{})}
	if err := w.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan IncomingMessage, 1)
	go func() {
		_ = w.Start(ctx, func(ctx context.Context, m IncomingMessage) error {
			got <- m
			return nil
		})
	}()
	<-stub.started

	conn := &stubWSConn{}
	if err := stub.startHandler(ctx, conn, "hello ws"); err != nil {
		t.Fatalf("handler: %v", err)
	}
	select {
	case m := <-got:
		if m.Text != "hello ws" {
			t.Errorf("Text = %q", m.Text)
		}
		if m.Identity.ActorPeer != "websocket" {
			t.Errorf("ActorPeer = %q", m.Identity.ActorPeer)
		}
		if m.Identity.Account != "acct" {
			t.Errorf("Account = %q", m.Identity.Account)
		}
	}
}

func TestWebSocket_SendConn(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{})
	stub := &stubWSAPI{}
	_ = w.Connect(context.Background(), stub)
	conn := &stubWSConn{}
	if err := w.SendConn(context.Background(), conn, OutgoingMessage{Text: "hi"}); err != nil {
		t.Fatalf("SendConn: %v", err)
	}
	if len(stub.sends) != 1 || stub.sends[0].text != "hi" {
		t.Errorf("sends = %+v", stub.sends)
	}
}

func TestWebSocket_SendNotConnected(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{})
	if err := w.Send(context.Background(), OutgoingMessage{}); err == nil {
		t.Fatalf("Send should error")
	}
}

func TestWebSocket_StartNotConnected(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{})
	if err := w.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil }); err == nil {
		t.Fatalf("Start should error")
	}
}

// TestWebSocket_SendByChatID verifies that Send routes to the right
// connection when ChatID is connID(conn). The connection must be
// tracked by the wsServer for the lookup to succeed.
func TestWebSocket_SendByChatID(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{})
	stub := &stubWSAPI{}
	_ = w.Connect(context.Background(), stub)
	conn := &stubWSConn{}
	// The stub api doesn't track conns, so Send should return a
	// descriptive "no active connection" error rather than
	// ErrUnsupported.
	err := w.Send(context.Background(), OutgoingMessage{ChatID: connID(conn), Text: "hi"})
	if err == nil {
		t.Fatalf("Send should error when no conn is tracked")
	}
}

// TestWebSocket_SendNoChatID verifies that Send returns a descriptive
// error when ChatID is empty (no conn reference threaded).
func TestWebSocket_SendNoChatID(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{})
	stub := &stubWSAPI{}
	_ = w.Connect(context.Background(), stub)
	err := w.Send(context.Background(), OutgoingMessage{Text: "hi"})
	if err == nil {
		t.Fatalf("Send should error on empty ChatID")
	}
}

// TestWebSocket_BroadcastNotConnected verifies that Broadcast errors
// when the api is not connected.
func TestWebSocket_BroadcastNotConnected(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{})
	if err := w.Broadcast(context.Background(), OutgoingMessage{Text: "hi"}); err == nil {
		t.Fatalf("Broadcast should error when not connected")
	}
}

// TestWebSocket_BroadcastStubAPI verifies that Broadcast surfaces a
// descriptive error when the api isn't the real wsServer (e.g. a stub).
func TestWebSocket_BroadcastStubAPI(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{})
	stub := &stubWSAPI{}
	_ = w.Connect(context.Background(), stub)
	err := w.Broadcast(context.Background(), OutgoingMessage{Text: "hi"})
	if err == nil {
		t.Fatalf("Broadcast should error with stub api")
	}
}

// TestWebSocket_SendByChatID_RealServer verifies that Send routes to a
// conn tracked by the real wsServer. We register a fake conn directly
// on the server's conn map (same package) and assert Send writes to it.
func TestWebSocket_SendByChatID_RealServer(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{})
	srv := newWSServer(config.ChannelConfig{})
	if err := w.Connect(context.Background(), srv); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	conn := &stubWSConn{}
	srv.track(conn)
	if err := w.Send(context.Background(), OutgoingMessage{ChatID: connID(conn), Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(conn.buf) == 0 {
		t.Fatalf("no bytes written to conn")
	}
}

// TestWebSocket_Broadcast_RealServer verifies that Broadcast writes to
// every tracked conn on the real wsServer.
func TestWebSocket_Broadcast_RealServer(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{})
	srv := newWSServer(config.ChannelConfig{})
	if err := w.Connect(context.Background(), srv); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	c1 := &stubWSConn{}
	c2 := &stubWSConn{}
	srv.track(c1)
	srv.track(c2)
	if err := w.Broadcast(context.Background(), OutgoingMessage{Text: "ping"}); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if len(c1.buf) == 0 || len(c2.buf) == 0 {
		t.Errorf("expected both conns to receive bytes; c1=%d c2=%d", len(c1.buf), len(c2.buf))
	}
}

// TestWebSocket_Broadcast_NoConns verifies that Broadcast returns a
// descriptive error when no conns are tracked.
func TestWebSocket_Broadcast_NoConns(t *testing.T) {
	w := NewWebSocket("websocket", config.ChannelConfig{})
	srv := newWSServer(config.ChannelConfig{})
	if err := w.Connect(context.Background(), srv); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := w.Broadcast(context.Background(), OutgoingMessage{Text: "hi"}); err == nil {
		t.Fatalf("Broadcast should error with no conns")
	}
}
