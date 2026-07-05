package channels

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// Discord is the Discord channel adapter. It wraps the official
// github.com/bwmarrin/discordgo SDK behind a small interface so tests
// can inject a stub.
//
// The real implementation uses discordgo's WebSocket Gateway for
// inbound MESSAGE_CREATE events and the REST API
// (ChannelMessageSendComplex) for outbound replies. discordgo handles
// the gateway protocol (HELLO/IDENTIFY/heartbeat/resume) internally,
// which mirrors what the Python discord.py does by hand.
type Discord struct {
	name string
	cfg  config.ChannelConfig
	api  discordAPI
}

// discordAPI is the subset of the discordgo SDK the adapter uses. The
// real implementation wraps *discordgo.Session; tests inject a stub.
type discordAPI interface {
	Start(ctx context.Context, handler func(ctx context.Context, ev *discordgo.MessageCreate) error) error
	Send(ctx context.Context, channelID, text string, replyTo string) error
}

// discordSDKClient wraps *discordgo.Session to satisfy discordAPI.
type discordSDKClient struct {
	session *discordgo.Session
	handler func(ctx context.Context, ev *discordgo.MessageCreate) error
}

func newDiscordSDKClient(cfg config.ChannelConfig) (*discordSDKClient, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("discord: bot token is empty")
	}
	token := cfg.Token
	if !strings.HasPrefix(token, "Bot ") {
		token = "Bot " + token
	}
	sess, err := discordgo.New(token)
	if err != nil {
		return nil, fmt.Errorf("discord: init sdk: %w", err)
	}
	// Intents default to GuildMessages + DirectMessages (the minimum
	// needed to receive MESSAGE_CREATE). Override via extra.intents.
	sess.Identify.Intents = discordgo.IntentGuildMessages | discordgo.IntentDirectMessages
	if intentsRaw := cfg.Extra["intents"]; intentsRaw != nil {
		if n, ok := toInt(intentsRaw); ok {
			sess.Identify.Intents = discordgo.Intent(n)
		}
	}
	return &discordSDKClient{session: sess}, nil
}

// toInt attempts to coerce v to int.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		// parse via strconv in extraInt; reuse here
		return extraInt(map[string]any{"x": v}, "x", -1), true
	default:
		return 0, false
	}
}

// Start implements discordAPI. It registers a MessageCreate handler,
// then runs discordgo's blocking gateway loop. Returns when ctx is
// canceled or the session exits with an error.
func (c *discordSDKClient) Start(ctx context.Context, handler func(ctx context.Context, ev *discordgo.MessageCreate) error) error {
	c.handler = handler
	c.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if c.handler == nil {
			return
		}
		// discordgo does not forward the parent context, so derive a
		// fresh one bound to the session's rate-limiter. Cancellation
		// is driven by the wrapping goroutine's ctx via session.Close.
		_ = c.handler(ctx, m)
	})
	// discordgo.WithCancel returns a context that is canceled when the
	// session's socket disconnects. We pair it with the caller's ctx
	// so canceling either stops the loop.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = c.session.Close()
	}()
	if err := c.session.Open(); err != nil {
		return fmt.Errorf("discord: open gateway: %w", err)
	}
	// Block until ctx is canceled. session.Close() (triggered above)
	// will cause discordgo's read loop to exit.
	<-ctx.Done()
	return nil
}

// Send implements discordAPI. It sends a text message, optionally
// referencing a prior message ID for replies.
func (c *discordSDKClient) Send(ctx context.Context, channelID, text string, replyTo string) error {
	params := &discordgo.MessageSend{
		Content: text,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			RepliedUser: false,
		},
	}
	if replyTo != "" {
		params.Reference = &discordgo.MessageReference{
			MessageID: replyTo,
			ChannelID: channelID,
		}
	}
	if _, err := c.session.ChannelMessageSendComplex(channelID, params, discordgo.WithContext(ctx)); err != nil {
		return fmt.Errorf("discord: send: %w", err)
	}
	return nil
}

// NewDiscord constructs a Discord adapter.
func NewDiscord(name string, cfg config.ChannelConfig) *Discord {
	return &Discord{name: name, cfg: cfg}
}

// Connect initializes the SDK client. The api argument (when non-nil)
// replaces the real SDK client — used by tests to inject a stub.
func (d *Discord) Connect(ctx context.Context, api discordAPI) error {
	if api != nil {
		d.api = api
		return nil
	}
	sdk, err := newDiscordSDKClient(d.cfg)
	if err != nil {
		return err
	}
	d.api = sdk
	return nil
}

// Name implements Channel.
func (d *Discord) Name() string { return d.name }

// Start implements Channel.
func (d *Discord) Start(ctx context.Context, handler Handler) error {
	if d.api == nil {
		return fmt.Errorf("discord: not connected")
	}
	return d.api.Start(ctx, func(ctx context.Context, ev *discordgo.MessageCreate) error {
		im := toIncomingDiscord(d.name, d.cfg, ev)
		if im.ChatID == "" || im.Text == "" {
			return nil
		}
		return handler(ctx, im)
	})
}

// Send implements Channel.
func (d *Discord) Send(ctx context.Context, msg OutgoingMessage) error {
	if d.api == nil {
		return fmt.Errorf("discord: not connected")
	}
	return d.api.Send(ctx, msg.ChatID, msg.Text, msg.ReplyToMsgID)
}

// toIncomingDiscord converts a discordgo.MessageCreate event to an
// IncomingMessage. Bot author messages are dropped upstream; here we
// additionally strip a leading "<@botid> " mention so the agent sees
// only the prompt text.
func toIncomingDiscord(name string, cfg config.ChannelConfig, ev *discordgo.MessageCreate) IncomingMessage {
	if ev == nil || ev.Message == nil {
		return IncomingMessage{}
	}
	m := ev.Message
	// Drop messages authored by bots (including ourselves).
	if m.Author != nil && m.Author.Bot {
		return IncomingMessage{}
	}
	text := m.Content
	// Strip a leading "<@botid> " / "<@!botid> " mention.
	if strings.HasPrefix(text, "<@") {
		if end := strings.IndexByte(text, '>'); end >= 0 {
			text = strings.TrimSpace(text[end+1:])
		}
	}
	userID, userName := "", ""
	if m.Author != nil {
		userID = m.Author.ID
		userName = m.Author.Username
	}
	var attachments []Attachment
	for _, a := range m.Attachments {
		if a == nil {
			continue
		}
		attachments = append(attachments, Attachment{
			Type:     "file",
			URL:      a.URL,
			MimeType: a.ContentType,
			Name:     a.Filename,
		})
	}
	return IncomingMessage{
		ChannelName: name,
		ChatID:      m.ChannelID,
		UserID:      userID,
		UserName:    userName,
		Text:        text,
		Attachments: attachments,
		Identity: domain.Identifier{
			Account:   cfg.AppID,
			ActorPeer: "discord",
		},
		Raw: ev,
	}
}
