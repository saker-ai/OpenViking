package channels

import (
	"context"
	"encoding/json"
	"fmt"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// Feishu is the 飞书 channel adapter. It wraps the official
// larksuite/oapi-sdk-go/v3 behind a small interface so tests can inject
// a stub.
//
// The real implementation uses the SDK's WebSocket long-connect
// (larkws.Client) for inbound events and the IM HTTP API
// (lark.Client.Im.Message.Create) for outbound replies.
type Feishu struct {
	name string
	cfg  config.ChannelConfig
	api  feishuAPI
}

// feishuAPI is the subset of the Feishu SDK the adapter uses. The real
// implementation wraps larkws.Client + lark.Client; tests inject a stub.
//
// Start blocks until ctx is canceled or the long-connect exits. handler
// is invoked for each inbound im.message.receive_v1 event.
type feishuAPI interface {
	Start(ctx context.Context, handler func(ctx context.Context, event *larkim.P2MessageReceiveV1) error) error
	Send(ctx context.Context, chatID, text string) error
}

// feishuSDKClient wraps the real SDK clients to satisfy feishuAPI.
type feishuSDKClient struct {
	wsClient *larkws.Client
	larkCli  *lark.Client
	handler  func(ctx context.Context, event *larkim.P2MessageReceiveV1) error
}

func newFeishuSDKClient(cfg config.ChannelConfig) (*feishuSDKClient, error) {
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, fmt.Errorf("feishu: app_id/app_secret required")
	}
	c := &feishuSDKClient{}
	// Event dispatcher. VerificationToken and EncryptKey are optional
	// for the WebSocket long-connect (the SDK handles decryption inline
	// when an encrypt key is configured on the app). We pass the
	// WebhookSecret as the encrypt key when set.
	disp := dispatcher.NewEventDispatcher("", cfg.WebhookSecret)
	disp.OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		if c.handler != nil {
			return c.handler(ctx, event)
		}
		return nil
	})
	ws := larkws.NewClient(cfg.AppID, cfg.AppSecret, larkws.WithEventHandler(disp))
	larkCli := lark.NewClient(cfg.AppID, cfg.AppSecret)
	c.wsClient = ws
	c.larkCli = larkCli
	return c, nil
}

// Start implements feishuAPI.
func (c *feishuSDKClient) Start(ctx context.Context, handler func(ctx context.Context, event *larkim.P2MessageReceiveV1) error) error {
	c.handler = handler
	return c.wsClient.Start(ctx)
}

// Send implements feishuAPI by creating a text message via the IM API.
func (c *feishuSDKClient) Send(ctx context.Context, chatID, text string) error {
	// Content is a JSON string with the message body, e.g.
	// {"text":"hello"} for text messages. chat_id is set via ReceiveId.
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType("text").
			ReceiveId(chatID).
			Content(string(content)).
			Build()).
		Build()
	resp, err := c.larkCli.Im.Message.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu: send: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: send: %d %s", resp.Code, resp.Msg)
	}
	return nil
}

// NewFeishu constructs a Feishu adapter. Use Connect to bootstrap.
func NewFeishu(name string, cfg config.ChannelConfig) *Feishu {
	return &Feishu{name: name, cfg: cfg}
}

// Connect initializes the SDK client. api (when non-nil) replaces the
// real SDK client — used by tests to inject a stub.
func (f *Feishu) Connect(ctx context.Context, api feishuAPI) error {
	if api != nil {
		f.api = api
		return nil
	}
	sdk, err := newFeishuSDKClient(f.cfg)
	if err != nil {
		return err
	}
	f.api = sdk
	return nil
}

// Name implements Channel.
func (f *Feishu) Name() string { return f.name }

// Start implements Channel.
func (f *Feishu) Start(ctx context.Context, handler Handler) error {
	if f.api == nil {
		return fmt.Errorf("feishu: not connected")
	}
	return f.api.Start(ctx, func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		msg := toIncomingFeishu(f.name, f.cfg, event)
		if msg.ChatID == "" || msg.Text == "" {
			return nil
		}
		return handler(ctx, msg)
	})
}

// Send implements Channel.
func (f *Feishu) Send(ctx context.Context, msg OutgoingMessage) error {
	if f.api == nil {
		return fmt.Errorf("feishu: not connected")
	}
	return f.api.Send(ctx, msg.ChatID, msg.Text)
}

// toIncomingFeishu converts a P2MessageReceiveV1 event to IncomingMessage.
//
// The Feishu event payload uses a nested structure: event.Event.Message.
// ChatID and UserID are extracted from the message; the text body lives
// in event.Event.Message.Content as JSON {"text":"..."}.
func toIncomingFeishu(name string, cfg config.ChannelConfig, ev *larkim.P2MessageReceiveV1) IncomingMessage {
	if ev == nil {
		return IncomingMessage{}
	}
	chatID, userID, text := "", "", ""
	if ev.Event != nil {
		if msg := ev.Event.Message; msg != nil {
			chatID = strPtr(msg.ChatId)
			if c := strPtr(msg.Content); c != "" {
				var body map[string]any
				if err := json.Unmarshal([]byte(c), &body); err == nil {
					if t, ok := body["text"].(string); ok {
						text = t
					}
				}
			}
		}
		if sender := ev.Event.Sender; sender != nil && sender.SenderId != nil {
			userID = strPtr(sender.SenderId.OpenId)
		}
	}
	return IncomingMessage{
		ChannelName: name,
		ChatID:      chatID,
		UserID:      userID,
		Text:        text,
		Identity: domain.Identifier{
			Account:   cfg.AppID,
			ActorPeer: "feishu",
		},
		Raw: ev,
	}
}

// strPtr safely dereferences a *string; empty when nil.
func strPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
