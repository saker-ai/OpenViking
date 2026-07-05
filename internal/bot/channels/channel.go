// Package channels defines the multi-channel adapter surface for
// vikingbot. Each chat platform (Telegram, Feishu, DingTalk, Slack, QQ,
// WebSocket) is wrapped by an implementation of the Channel interface.
//
// All external SDK calls go through this interface. Tests inject stub
// implementations, so no test makes network calls.
package channels

import (
	"context"
	"errors"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// IncomingMessage is the canonical representation of an inbound chat
// message produced by a Channel adapter. The agent loop consumes it.
type IncomingMessage struct {
	// ChannelName is the config key of the channel that produced the
	// message (e.g. "telegram", "feishu"). It is set by the Dispatcher.
	ChannelName string `json:"channel_name"`
	// ChatID is the conversation identifier on the originating platform
	// (Telegram chat ID, Feishu chat ID, etc.). Replies use the same ID.
	ChatID string `json:"chat_id"`
	// UserID is the platform-specific user identifier.
	UserID string `json:"user_id"`
	// UserName is the human-readable display name (may be empty).
	UserName string `json:"user_name,omitempty"`
	// Text is the message body. Adapters strip bot mentions.
	Text string `json:"text"`
	// Attachments carries optional media references (file URLs, image IDs).
	Attachments []Attachment `json:"attachments,omitempty"`
	// Raw is the original SDK event, kept for adapter-specific context
	// in case the agent needs to call back. Adapters must not assume
	// any concrete type.
	Raw any `json:"-"`
	// Identity is the OpenViking identity the bot should act as when
	// processing this message. Adapters fill it from channel config.
	Identity domain.Identifier `json:"identity"`
}

// Attachment is a media reference attached to an IncomingMessage.
type Attachment struct {
	Type     string `json:"type"` // "image", "file", "audio"
	URL      string `json:"url,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Name     string `json:"name,omitempty"`
}

// OutgoingMessage is the canonical reply produced by the agent loop.
type OutgoingMessage struct {
	ChatID       string `json:"chat_id"`
	Text         string `json:"text"`
	ReplyToMsgID string `json:"reply_to_msg_id,omitempty"`
}

// Channel is the adapter interface. Each platform implements it. The
// Start method blocks until ctx is canceled or the underlying stream
// exits; callers run it in its own goroutine.
type Channel interface {
	// Name returns the config key (e.g. "telegram").
	Name() string
	// Start subscribes to the platform's inbound stream and dispatches
	// each event to handler. It returns when ctx is canceled or the
	// stream exits with an error.
	Start(ctx context.Context, handler Handler) error
	// Send delivers a reply to the platform.
	Send(ctx context.Context, msg OutgoingMessage) error
}

// Handler processes an IncomingMessage. The agent loop implements it.
// Returning a non-nil error logs the failure but does not stop the
// channel; the channel continues receiving subsequent messages.
type Handler func(ctx context.Context, msg IncomingMessage) error

// ErrUnsupported is returned by adapters whose SDK is not yet wired.
var ErrUnsupported = errors.Join(domain.ErrUnsupported, errors.New("channel: SDK not wired"))

// New constructs a Channel from config. provider is the channel's
// config key (telegram/feishu/dingtalk/slack/qq/websocket/discord/
// email/openapi/whatsapp). It returns ErrUnsupported when the SDK has
// not been wired yet.
func New(name string, cfg config.ChannelConfig) (Channel, error) {
	switch cfg.Provider {
	case "telegram":
		return NewTelegram(name, cfg), nil
	case "feishu":
		return NewFeishu(name, cfg), nil
	case "dingtalk":
		return NewDingTalk(name, cfg), nil
	case "slack":
		return NewSlack(name, cfg), nil
	case "qq":
		return NewQQ(name, cfg), nil
	case "websocket":
		return NewWebSocket(name, cfg), nil
	case "discord":
		return NewDiscord(name, cfg), nil
	case "email":
		return NewEmail(name, cfg), nil
	case "openapi":
		return NewOpenAPI(name, cfg), nil
	case "whatsapp":
		return NewWhatsApp(name, cfg), nil
	default:
		return nil, ErrUnsupported
	}
}
