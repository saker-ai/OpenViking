package channels

import (
	"context"
	"fmt"
	"strings"

	tgbot "github.com/go-telegram/bot"
	tgmodels "github.com/go-telegram/bot/models"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// Telegram is the Telegram channel adapter. It wraps the official
// github.com/go-telegram/bot SDK behind a small interface so tests can
// inject a stub.
type Telegram struct {
	name string
	cfg  config.ChannelConfig
	bot  telegramAPI
}

// telegramAPI is the subset of *tgbot.Bot the adapter uses. The real
// SDK satisfies it via the telegramSDKBot wrapper; tests inject a stub.
type telegramAPI interface {
	Start(ctx context.Context, handler func(ctx context.Context, update *tgmodels.Update)) error
	SendMessage(ctx context.Context, params *tgbot.SendMessageParams) (*tgmodels.Message, error)
}

// telegramSDKBot wraps *tgbot.Bot to satisfy telegramAPI. It exists
// because the SDK's WithDefaultHandler must be set at construction
// time, so we need a closure that forwards to a stored handler field.
type telegramSDKBot struct {
	b       *tgbot.Bot
	handler func(ctx context.Context, update *tgmodels.Update)
}

func newTelegramSDKBot(cfg config.ChannelConfig) (*telegramSDKBot, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("telegram: token is empty")
	}
	s := &telegramSDKBot{}
	b, err := tgbot.New(
		cfg.Token,
		tgbot.WithSkipGetMe(),
		tgbot.WithDefaultHandler(func(ctx context.Context, _ *tgbot.Bot, u *tgmodels.Update) {
			if s.handler != nil {
				s.handler(ctx, u)
			}
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("telegram: init sdk: %w", err)
	}
	s.b = b
	return s, nil
}

// Start sets the handler and runs the SDK's long-poll loop. It blocks
// until ctx is canceled or the SDK returns.
func (s *telegramSDKBot) Start(ctx context.Context, handler func(ctx context.Context, u *tgmodels.Update)) error {
	s.handler = handler
	s.b.Start(ctx)
	return nil
}

// SendMessage forwards to the SDK.
func (s *telegramSDKBot) SendMessage(ctx context.Context, params *tgbot.SendMessageParams) (*tgmodels.Message, error) {
	return s.b.SendMessage(ctx, params)
}

// NewTelegram constructs a Telegram adapter. Use Connect to bootstrap
// the SDK client against the configured token.
func NewTelegram(name string, cfg config.ChannelConfig) *Telegram {
	return &Telegram{name: name, cfg: cfg}
}

// Connect initializes the underlying SDK client. The bot argument (when
// non-nil) replaces the SDK client — used by tests to inject a stub.
func (t *Telegram) Connect(ctx context.Context, client telegramAPI) error {
	if client != nil {
		t.bot = client
		return nil
	}
	sdk, err := newTelegramSDKBot(t.cfg)
	if err != nil {
		return err
	}
	t.bot = sdk
	return nil
}

// Name implements Channel.
func (t *Telegram) Name() string { return t.name }

// Start implements Channel. It blocks until ctx is canceled.
func (t *Telegram) Start(ctx context.Context, handler Handler) error {
	if t.bot == nil {
		return fmt.Errorf("telegram: not connected (call Connect first)")
	}
	return t.bot.Start(ctx, func(ctx context.Context, u *tgmodels.Update) {
		if u == nil || u.Message == nil {
			return
		}
		msg := toIncomingTelegram(t.name, t.cfg, u)
		if msg.ChatID == "" || msg.Text == "" {
			return
		}
		_ = handler(ctx, msg)
	})
}

// Send implements Channel.
func (t *Telegram) Send(ctx context.Context, msg OutgoingMessage) error {
	if t.bot == nil {
		return fmt.Errorf("telegram: not connected")
	}
	_, err := t.bot.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID: msg.ChatID,
		Text:   msg.Text,
	})
	return err
}

// toIncomingTelegram converts an SDK update to our IncomingMessage.
func toIncomingTelegram(name string, cfg config.ChannelConfig, u *tgmodels.Update) IncomingMessage {
	m := u.Message
	text := m.Text
	// Strip a leading "/<botcommand>" — the agent doesn't need it.
	if strings.HasPrefix(text, "/") {
		if sp := strings.IndexByte(text, ' '); sp >= 0 {
			text = strings.TrimSpace(text[sp+1:])
		} else {
			text = ""
		}
	}
	userID, userName := int64(0), ""
	if m.From != nil {
		userID = m.From.ID
		userName = m.From.Username
	}
	return IncomingMessage{
		ChannelName: name,
		ChatID:      fmt.Sprintf("%d", m.Chat.ID),
		UserID:      fmt.Sprintf("%d", userID),
		UserName:    userName,
		Text:        text,
		Identity: domain.Identifier{
			Account:   cfg.AppID,
			ActorPeer: "telegram",
		},
		Raw: u,
	}
}
