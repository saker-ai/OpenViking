package channels

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// stubWhatsAppAPI implements whatsAppAPI for tests.
type stubWhatsAppAPI struct {
	startHandler func(ctx context.Context, m *whatsAppInbound) error
	started      chan struct{}
	sends        []whatsAppSend
	startErr     error
	sendErr      error
	mu           sync.Mutex
}

type whatsAppSend struct {
	to   string
	text string
}

func (s *stubWhatsAppAPI) Start(ctx context.Context, handler func(ctx context.Context, m *whatsAppInbound) error) error {
	if s.startErr != nil {
		return s.startErr
	}
	s.startHandler = handler
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	return nil
}

func (s *stubWhatsAppAPI) Send(ctx context.Context, to, text string) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.mu.Lock()
	s.sends = append(s.sends, whatsAppSend{to: to, text: text})
	s.mu.Unlock()
	return nil
}

func TestWhatsApp_ConnectWithStub(t *testing.T) {
	w := NewWhatsApp("whatsapp", config.ChannelConfig{Provider: "whatsapp"})
	stub := &stubWhatsAppAPI{}
	if err := w.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if w.api != stub {
		t.Fatalf("api not set")
	}
}

func TestWhatsApp_StartDispatchesMessages(t *testing.T) {
	w := NewWhatsApp("whatsapp", config.ChannelConfig{Provider: "whatsapp", AppID: "acct"})
	stub := &stubWhatsAppAPI{started: make(chan struct{})}
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
	if stub.startHandler == nil {
		t.Fatalf("Start did not register a handler")
	}
	inbound := &whatsAppInbound{
		ChatID:    "lid-xyz@s.whatsapp.net",
		SenderID:  "15551234567",
		Content:   "hello whatsapp",
		MessageID: "msg-1",
		IsGroup:   false,
	}
	if err := stub.startHandler(ctx, inbound); err != nil {
		t.Fatalf("handler: %v", err)
	}
	select {
	case m := <-got:
		if m.ChatID != "lid-xyz@s.whatsapp.net" {
			t.Errorf("ChatID = %q", m.ChatID)
		}
		if m.UserID != "15551234567" {
			t.Errorf("UserID = %q", m.UserID)
		}
		if m.Text != "hello whatsapp" {
			t.Errorf("Text = %q", m.Text)
		}
		if m.Identity.ActorPeer != "whatsapp" {
			t.Errorf("ActorPeer = %q", m.Identity.ActorPeer)
		}
		if m.Identity.Account != "acct" {
			t.Errorf("Account = %q", m.Identity.Account)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no message received")
	}
}

func TestWhatsApp_VoiceMessageRewritten(t *testing.T) {
	w := NewWhatsApp("whatsapp", config.ChannelConfig{Provider: "whatsapp"})
	stub := &stubWhatsAppAPI{started: make(chan struct{})}
	_ = w.Connect(context.Background(), stub)
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
	inbound := &whatsAppInbound{
		ChatID:   "lid@s.whatsapp.net",
		SenderID: "1555",
		Content:  "[Voice Message]",
	}
	_ = stub.startHandler(ctx, inbound)
	select {
	case m := <-got:
		if !strings.Contains(m.Text, "Transcription not available") {
			t.Errorf("Text = %q, want transcription-not-available marker", m.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no message received")
	}
}

func TestWhatsApp_IgnoresEmptyMessage(t *testing.T) {
	w := NewWhatsApp("whatsapp", config.ChannelConfig{Provider: "whatsapp"})
	stub := &stubWhatsAppAPI{started: make(chan struct{})}
	_ = w.Connect(context.Background(), stub)
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
	// Empty content -> dropped.
	_ = stub.startHandler(ctx, &whatsAppInbound{ChatID: "x", SenderID: "y", Content: ""})
	// Empty chat_id -> dropped.
	_ = stub.startHandler(ctx, &whatsAppInbound{ChatID: "", SenderID: "y", Content: "hi"})
	select {
	case m := <-got:
		t.Fatalf("expected drop, got %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWhatsApp_Send(t *testing.T) {
	w := NewWhatsApp("whatsapp", config.ChannelConfig{Provider: "whatsapp"})
	stub := &stubWhatsAppAPI{}
	_ = w.Connect(context.Background(), stub)
	if err := w.Send(context.Background(), OutgoingMessage{ChatID: "to-1", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.sends) != 1 || stub.sends[0].to != "to-1" || stub.sends[0].text != "hi" {
		t.Errorf("sends = %+v", stub.sends)
	}
}

func TestWhatsApp_SendPropagatesError(t *testing.T) {
	w := NewWhatsApp("whatsapp", config.ChannelConfig{Provider: "whatsapp"})
	stub := &stubWhatsAppAPI{sendErr: errors.New("boom")}
	_ = w.Connect(context.Background(), stub)
	err := w.Send(context.Background(), OutgoingMessage{ChatID: "x", Text: "y"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Send should propagate error, got %v", err)
	}
}

func TestWhatsApp_StartNotConnected(t *testing.T) {
	w := NewWhatsApp("whatsapp", config.ChannelConfig{Provider: "whatsapp"})
	if err := w.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil }); err == nil {
		t.Fatalf("Start should error when not connected")
	}
}

func TestWhatsApp_SendNotConnected(t *testing.T) {
	w := NewWhatsApp("whatsapp", config.ChannelConfig{Provider: "whatsapp"})
	if err := w.Send(context.Background(), OutgoingMessage{ChatID: "x", Text: "y"}); err == nil {
		t.Fatalf("Send should error when not connected")
	}
}

func TestWhatsApp_BridgeClientRequiresEndpoint(t *testing.T) {
	c := newWhatsAppBridgeClient(config.ChannelConfig{Provider: "whatsapp"})
	err := c.Start(context.Background(), func(context.Context, *whatsAppInbound) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("expected endpoint error, got %v", err)
	}
}

func TestWhatsApp_BridgeSendNotConnected(t *testing.T) {
	c := newWhatsAppBridgeClient(config.ChannelConfig{Provider: "whatsapp", Endpoint: "ws://127.0.0.1:0"})
	if err := c.Send(context.Background(), "to", "text"); err == nil {
		t.Fatalf("Send should error when not connected")
	}
}

func TestWhatsApp_BridgeClientAuthAndMessage(t *testing.T) {
	// Spin up a local WebSocket server that:
	//  1. accepts an auth frame
	//  2. sends one message frame
	//  3. closes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "done")
		// Read auth frame.
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var auth map[string]string
		_ = json.Unmarshal(data, &auth)
		if auth["type"] != "auth" || auth["token"] != "tok" {
			t.Errorf("unexpected auth frame: %s", data)
		}
		// Send a message frame.
		msg, _ := json.Marshal(map[string]any{
			"type":    "message",
			"sender":  "lid-1@s.whatsapp.net",
			"pn":      "1555",
			"content": "hi from bridge",
			"id":      "m-1",
			"isGroup": false,
		})
		_ = c.Write(r.Context(), websocket.MessageText, msg)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	cfg := config.ChannelConfig{
		Provider: "whatsapp",
		Endpoint: wsURL,
		Token:    "tok",
	}
	c := newWhatsAppBridgeClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got := make(chan *whatsAppInbound, 1)
	go func() {
		_ = c.Start(ctx, func(ctx context.Context, m *whatsAppInbound) error {
			got <- m
			return nil
		})
	}()
	select {
	case m := <-got:
		if m.ChatID != "lid-1@s.whatsapp.net" {
			t.Errorf("ChatID = %q", m.ChatID)
		}
		if m.SenderID != "1555" {
			t.Errorf("SenderID = %q", m.SenderID)
		}
		if m.Content != "hi from bridge" {
			t.Errorf("Content = %q", m.Content)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no message received from bridge")
	}
}

func TestWhatsApp_BridgeClientSendRoundTrip(t *testing.T) {
	// Spin up a local WebSocket server that records the send frame.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "done")
		// Read frames until we see a send frame.
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var env map[string]any
			_ = json.Unmarshal(data, &env)
			if env["type"] == "send" {
				break
			}
		}
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	cfg := config.ChannelConfig{
		Provider: "whatsapp",
		Endpoint: wsURL,
	}
	c := newWhatsAppBridgeClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	handlerErr := make(chan error, 1)
	go func() {
		handlerErr <- c.Start(ctx, func(context.Context, *whatsAppInbound) error { return nil })
	}()
	// Wait for connection to be established by polling Send.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.Send(ctx, "to-1", "hello"); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Send never succeeded; bridge did not connect")
}
