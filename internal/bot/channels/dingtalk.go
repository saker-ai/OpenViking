package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	dingtalk "github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	dingclient "github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// DingTalk is the DingTalk channel adapter. It uses the official
// open-dingtalk/dingtalk-stream-sdk-go for inbound stream events and
// the SessionWebhook URL provided in each callback for outbound
// replies (the official DingTalk bot reply pattern).
type DingTalk struct {
	name string
	cfg  config.ChannelConfig
	api  dingtalkAPI
}

// dingtalkAPI is the subset of the DingTalk SDK the adapter uses. Real
// implementation wraps StreamClient; tests inject a stub.
type dingtalkAPI interface {
	Start(ctx context.Context, handler func(ctx context.Context, data *dingtalk.BotCallbackDataModel) error) error
	Send(ctx context.Context, sessionWebhook, text string) error
}

// dingtalkSDKClient wraps the real SDK client.
type dingtalkSDKClient struct {
	streamCli *dingclient.StreamClient
	handler   func(ctx context.Context, data *dingtalk.BotCallbackDataModel) error
}

func newDingtalkSDKClient(cfg config.ChannelConfig) (*dingtalkSDKClient, error) {
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, fmt.Errorf("dingtalk: app_id/app_secret required")
	}
	c := &dingtalkSDKClient{}
	cli := dingclient.NewStreamClient(
		dingclient.WithAppCredential(dingclient.NewAppCredentialConfig(cfg.AppID, cfg.AppSecret)),
	)
	cli.RegisterChatBotCallbackRouter(func(ctx context.Context, data *dingtalk.BotCallbackDataModel) ([]byte, error) {
		if c.handler != nil {
			if err := c.handler(ctx, data); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	c.streamCli = cli
	return c, nil
}

// Start implements dingtalkAPI.
func (c *dingtalkSDKClient) Start(ctx context.Context, handler func(ctx context.Context, data *dingtalk.BotCallbackDataModel) error) error {
	c.handler = handler
	return c.streamCli.Start(ctx)
}

// Send implements dingtalkAPI by POSTing to the SessionWebhook URL.
// DingTalk's bot reply protocol: each inbound callback carries a
// temporary webhook URL (SessionWebhook) valid for ~1 hour; replies
// are sent there as JSON {"msgtype":"text","text":{"content":"..."}}.
func (c *dingtalkSDKClient) Send(ctx context.Context, sessionWebhook, text string) error {
	body, _ := json.Marshal(map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sessionWebhook, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("dingtalk: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("dingtalk: send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("dingtalk: send: http %d", resp.StatusCode)
	}
	return nil
}

// NewDingTalk constructs a DingTalk adapter.
func NewDingTalk(name string, cfg config.ChannelConfig) *DingTalk {
	return &DingTalk{name: name, cfg: cfg}
}

// Connect initializes the SDK client.
func (d *DingTalk) Connect(ctx context.Context, api dingtalkAPI) error {
	if api != nil {
		d.api = api
		return nil
	}
	sdk, err := newDingtalkSDKClient(d.cfg)
	if err != nil {
		return err
	}
	d.api = sdk
	return nil
}

// Name implements Channel.
func (d *DingTalk) Name() string { return d.name }

// Start implements Channel.
func (d *DingTalk) Start(ctx context.Context, handler Handler) error {
	if d.api == nil {
		return fmt.Errorf("dingtalk: not connected")
	}
	return d.api.Start(ctx, func(ctx context.Context, data *dingtalk.BotCallbackDataModel) error {
		im := toIncomingDingTalk(d.name, d.cfg, data)
		if im.ChatID == "" || im.Text == "" {
			return nil
		}
		return handler(ctx, im)
	})
}

// Send implements Channel. msg.ChatID is the SessionWebhook URL for
// DingTalk (set from the inbound callback).
func (d *DingTalk) Send(ctx context.Context, msg OutgoingMessage) error {
	if d.api == nil {
		return fmt.Errorf("dingtalk: not connected")
	}
	return d.api.Send(ctx, msg.ChatID, msg.Text)
}

// toIncomingDingTalk converts a BotCallbackDataModel to IncomingMessage.
//
// ChatID is set to the SessionWebhook URL because DingTalk replies are
// addressed per-message, not per-conversation.
func toIncomingDingTalk(name string, cfg config.ChannelConfig, data *dingtalk.BotCallbackDataModel) IncomingMessage {
	if data == nil {
		return IncomingMessage{}
	}
	text := strings.TrimSpace(data.Text.Content)
	// Strip a leading "@botname " — the agent doesn't need it.
	if strings.HasPrefix(text, "@") {
		if sp := strings.IndexByte(text, ' '); sp >= 0 {
			text = strings.TrimSpace(text[sp+1:])
		}
	}
	return IncomingMessage{
		ChannelName: name,
		ChatID:      data.SessionWebhook,
		UserID:      data.SenderStaffId,
		UserName:    data.SenderNick,
		Text:        text,
		Identity: domain.Identifier{
			Account:   cfg.AppID,
			ActorPeer: "dingtalk",
		},
		Raw: data,
	}
}
