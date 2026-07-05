package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// WhatsApp is the WhatsApp channel adapter. It connects to a Node.js
// bridge (running @whiskeysockets/baileys) over WebSocket. The bridge
// handles the WhatsApp Web protocol; this adapter only does JSON framing.
//
// Inbound: bridge sends {"type":"message","sender":"...","content":"..."}.
// Outbound: adapter sends {"type":"send","to":"...","text":"..."}.
//
// Auth: when cfg.Token is set, the adapter sends
// {"type":"auth","token":"..."} immediately after connecting.
type WhatsApp struct {
	name string
	cfg  config.ChannelConfig
	api  whatsAppAPI
}

// whatsAppAPI is the subset of the bridge connection the adapter uses.
// Real implementation wraps a coder/websocket dial; tests inject a stub.
type whatsAppAPI interface {
	Start(ctx context.Context, handler func(ctx context.Context, m *whatsAppInbound) error) error
	Send(ctx context.Context, to, text string) error
}

// whatsAppInbound is the parsed bridge message of type "message".
type whatsAppInbound struct {
	SenderID  string `json:"sender_id"`
	ChatID    string `json:"chat_id"` // full LID for replies
	Content   string `json:"content"`
	MessageID string `json:"message_id,omitempty"`
	IsGroup   bool   `json:"is_group,omitempty"`
}

// whatsAppBridgeClient wraps a coder/websocket connection to the bridge.
type whatsAppBridgeClient struct {
	cfg     config.ChannelConfig
	mu      sync.Mutex
	conn    *websocket.Conn
	handler func(ctx context.Context, m *whatsAppInbound) error
}

func newWhatsAppBridgeClient(cfg config.ChannelConfig) *whatsAppBridgeClient {
	return &whatsAppBridgeClient{cfg: cfg}
}

// Start dials the bridge and processes messages until ctx is canceled.
// It reconnects with a 5s backoff on transient errors.
func (c *whatsAppBridgeClient) Start(ctx context.Context, handler func(ctx context.Context, m *whatsAppInbound) error) error {
	if c.cfg.Endpoint == "" {
		return fmt.Errorf("whatsapp: bridge endpoint (endpoint) is empty")
	}
	c.handler = handler
	for {
		if err := c.connectAndServe(ctx); err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			// Reconnect after backoff
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
			}
			continue
		}
		return nil
	}
}

// connectAndServe dials the bridge, sends auth, and processes messages.
func (c *whatsAppBridgeClient) connectAndServe(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, c.cfg.Endpoint, nil)
	if err != nil {
		return fmt.Errorf("whatsapp: dial bridge: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "shutting down")
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}()

	// Send auth if token configured
	if c.cfg.Token != "" {
		auth, _ := json.Marshal(map[string]string{"type": "auth", "token": c.cfg.Token})
		if err := conn.Write(ctx, websocket.MessageText, auth); err != nil {
			return fmt.Errorf("whatsapp: auth write: %w", err)
		}
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("whatsapp: read: %w", err)
		}
		var envelope struct {
			Type      string `json:"type"`
			PN        string `json:"pn"`
			Sender    string `json:"sender"`
			Content   string `json:"content"`
			ID        string `json:"id"`
			Timestamp any    `json:"timestamp"`
			IsGroup   bool   `json:"isGroup"`
			Status    string `json:"status"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}
		if envelope.Type != "message" {
			continue
		}
		inbound := whatsAppInbound{
			ChatID:    envelope.Sender,
			Content:   envelope.Content,
			MessageID: envelope.ID,
			IsGroup:   envelope.IsGroup,
		}
		// sender_id: prefer pn, strip @domain
		senderID := envelope.PN
		if senderID == "" {
			senderID = envelope.Sender
		}
		if at := indexOfByte(senderID, '@'); at >= 0 {
			senderID = senderID[:at]
		}
		inbound.SenderID = senderID

		if c.handler != nil {
			if err := c.handler(ctx, &inbound); err != nil {
				return err
			}
		}
	}
}

// Send writes a send payload to the current bridge connection.
func (c *whatsAppBridgeClient) Send(ctx context.Context, to, text string) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("whatsapp: bridge not connected")
	}
	payload, _ := json.Marshal(map[string]string{"type": "send", "to": to, "text": text})
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return fmt.Errorf("whatsapp: send: %w", err)
	}
	return nil
}

// NewWhatsApp constructs a WhatsApp adapter.
func NewWhatsApp(name string, cfg config.ChannelConfig) *WhatsApp {
	return &WhatsApp{name: name, cfg: cfg}
}

// Connect initializes the bridge client. The api argument (when non-nil)
// replaces the real client — used by tests to inject a stub.
func (w *WhatsApp) Connect(ctx context.Context, api whatsAppAPI) error {
	if api != nil {
		w.api = api
		return nil
	}
	w.api = newWhatsAppBridgeClient(w.cfg)
	return nil
}

// Name implements Channel.
func (w *WhatsApp) Name() string { return w.name }

// Start implements Channel.
func (w *WhatsApp) Start(ctx context.Context, handler Handler) error {
	if w.api == nil {
		return fmt.Errorf("whatsapp: not connected")
	}
	return w.api.Start(ctx, func(ctx context.Context, m *whatsAppInbound) error {
		im := toIncomingWhatsApp(w.name, w.cfg, m)
		if im.ChatID == "" || im.Text == "" {
			return nil
		}
		return handler(ctx, im)
	})
}

// Send implements Channel.
func (w *WhatsApp) Send(ctx context.Context, msg OutgoingMessage) error {
	if w.api == nil {
		return fmt.Errorf("whatsapp: not connected")
	}
	return w.api.Send(ctx, msg.ChatID, msg.Text)
}

// toIncomingWhatsApp converts a bridge message to IncomingMessage.
func toIncomingWhatsApp(name string, cfg config.ChannelConfig, m *whatsAppInbound) IncomingMessage {
	if m == nil {
		return IncomingMessage{}
	}
	content := m.Content
	// Voice messages arrive from the bridge as a sentinel "[Voice Message]"
	// string. The bridge does not transcribe; surface that explicitly so the
	// agent doesn't try to play the audio back as text.
	if content == "[Voice Message]" {
		content = "[Voice Message: Transcription not available for WhatsApp yet]"
	}
	return IncomingMessage{
		ChannelName: name,
		ChatID:      m.ChatID,
		UserID:      m.SenderID,
		Text:        content,
		Identity: domain.Identifier{
			Account:   cfg.AppID,
			ActorPeer: "whatsapp",
		},
		Raw: m,
	}
}

// indexOfByte returns the index of the first occurrence of b in s, or -1.
func indexOfByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
