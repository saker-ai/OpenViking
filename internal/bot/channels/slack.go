package channels

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// Slack is the Slack channel adapter. It uses the official
// github.com/slack-go/slack Socket Mode (WebSocket) for inbound events
// and the Web API (chat.PostMessage) for outbound replies.
type Slack struct {
	name string
	cfg  config.ChannelConfig
	api  slackAPI
}

// slackAPI is the subset of the Slack SDK the adapter uses. Real
// implementation wraps socketmode.Client + slack.Client; tests inject
// a stub.
type slackAPI interface {
	Start(ctx context.Context, handler func(ctx context.Context, ev *slackevents.MessageEvent) error) error
	Send(ctx context.Context, channelID, text string) error
}

// slackSDKClient wraps the real SDK clients to satisfy slackAPI.
type slackSDKClient struct {
	socketCli *socketmode.Client
	slackCli  *slack.Client
}

func newSlackSDKClient(cfg config.ChannelConfig) (*slackSDKClient, error) {
	if cfg.Token == "" || cfg.AppSecret == "" {
		return nil, fmt.Errorf("slack: token (xoxb-) and app_secret (xapp-) required")
	}
	botToken := cfg.Token
	if !strings.HasPrefix(botToken, "xoxb-") {
		return nil, fmt.Errorf("slack: token must start with xoxb-")
	}
	appToken := cfg.AppSecret
	if !strings.HasPrefix(appToken, "xapp-") {
		return nil, fmt.Errorf("slack: app_secret must start with xapp-")
	}
	slackCli := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	socketCli := socketmode.New(slackCli)
	return &slackSDKClient{socketCli: socketCli, slackCli: slackCli}, nil
}

// Start implements slackAPI. It runs the socketmode connection in a
// goroutine and ranges over the Events channel in the calling
// goroutine, blocking until ctx is canceled.
func (c *slackSDKClient) Start(ctx context.Context, handler func(ctx context.Context, ev *slackevents.MessageEvent) error) error {
	go func() {
		_ = c.socketCli.RunContext(ctx)
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-c.socketCli.Events:
			if !ok {
				return nil
			}
			// Slack Socket Mode envelopes EventsAPIEvent under
			// EventTypeEventsAPI. Extract the inner MessageEvent.
			if ev.Type != socketmode.EventTypeEventsAPI {
				continue
			}
			outer, ok := ev.Data.(*slackevents.EventsAPIEvent)
			if !ok || outer == nil {
				continue
			}
			inner, ok := outer.InnerEvent.Data.(*slackevents.MessageEvent)
			if !ok || inner == nil {
				continue
			}
			if err := handler(ctx, inner); err != nil {
				return err
			}
		}
	}
}

// Send implements slackAPI.
func (c *slackSDKClient) Send(ctx context.Context, channelID, text string) error {
	_, _, err := c.slackCli.PostMessageContext(ctx, channelID, slack.MsgOptionText(text, false))
	if err != nil {
		return fmt.Errorf("slack: send: %w", err)
	}
	return nil
}

// NewSlack constructs a Slack adapter.
func NewSlack(name string, cfg config.ChannelConfig) *Slack {
	return &Slack{name: name, cfg: cfg}
}

// Connect initializes the SDK client.
func (s *Slack) Connect(ctx context.Context, api slackAPI) error {
	if api != nil {
		s.api = api
		return nil
	}
	sdk, err := newSlackSDKClient(s.cfg)
	if err != nil {
		return err
	}
	s.api = sdk
	return nil
}

// Name implements Channel.
func (s *Slack) Name() string { return s.name }

// Start implements Channel.
func (s *Slack) Start(ctx context.Context, handler Handler) error {
	if s.api == nil {
		return fmt.Errorf("slack: not connected")
	}
	return s.api.Start(ctx, func(ctx context.Context, msg *slackevents.MessageEvent) error {
		im := toIncomingSlack(s.name, s.cfg, msg)
		if im.ChatID == "" || im.Text == "" {
			return nil
		}
		return handler(ctx, im)
	})
}

// Send implements Channel.
func (s *Slack) Send(ctx context.Context, msg OutgoingMessage) error {
	if s.api == nil {
		return fmt.Errorf("slack: not connected")
	}
	return s.api.Send(ctx, msg.ChatID, msg.Text)
}

// toIncomingSlack converts a slackevents.MessageEvent to IncomingMessage.
func toIncomingSlack(name string, cfg config.ChannelConfig, ev *slackevents.MessageEvent) IncomingMessage {
	if ev == nil {
		return IncomingMessage{}
	}
	text := ev.Text
	// Strip a leading "<@botid> " mention — the agent doesn't need it.
	if strings.HasPrefix(text, "<@") {
		if sp := strings.IndexByte(text, '>'); sp >= 0 {
			text = strings.TrimSpace(text[sp+1:])
		}
	}
	return IncomingMessage{
		ChannelName: name,
		ChatID:      ev.Channel,
		UserID:      ev.User,
		Text:        text,
		Identity: domain.Identifier{
			Account:   cfg.AppID,
			ActorPeer: "slack",
		},
		Raw: ev,
	}
}
