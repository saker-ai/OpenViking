package channels

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// stubDiscordAPI implements discordAPI for tests.
type stubDiscordAPI struct {
	startHandler func(ctx context.Context, ev *discordgo.MessageCreate) error
	sends        []discordSend
	started      chan struct{}
}

type discordSend struct {
	channelID string
	text      string
	replyTo   string
}

func (s *stubDiscordAPI) Start(ctx context.Context, handler func(ctx context.Context, ev *discordgo.MessageCreate) error) error {
	s.startHandler = handler
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	return nil
}

func (s *stubDiscordAPI) Send(ctx context.Context, channelID, text, replyTo string) error {
	s.sends = append(s.sends, discordSend{channelID: channelID, text: text, replyTo: replyTo})
	return nil
}

func TestDiscord_ConnectWithStub(t *testing.T) {
	d := NewDiscord("discord", config.ChannelConfig{Provider: "discord", Token: "x"})
	stub := &stubDiscordAPI{}
	if err := d.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if d.api != stub {
		t.Fatalf("api not set")
	}
}

func TestDiscord_StartDispatchesMessages(t *testing.T) {
	d := NewDiscord("discord", config.ChannelConfig{Provider: "discord", AppID: "acct"})
	stub := &stubDiscordAPI{started: make(chan struct{})}
	if err := d.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan IncomingMessage, 1)
	go func() {
		_ = d.Start(ctx, func(ctx context.Context, m IncomingMessage) error {
			got <- m
			return nil
		})
	}()
	<-stub.started
	if stub.startHandler == nil {
		t.Fatalf("Start did not register a handler")
	}
	ev := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-1",
			ChannelID: "chan-456",
			Content:   "<@999> hello discord",
			Author: &discordgo.User{
				ID:       "123",
				Username: "alice",
				Bot:      false,
			},
			Attachments: []*discordgo.MessageAttachment{
				{ID: "a1", URL: "https://example.com/x.png", Filename: "x.png", ContentType: "image/png"},
			},
		},
	}
	if err := stub.startHandler(ctx, ev); err != nil {
		t.Fatalf("handler: %v", err)
	}
	select {
	case m := <-got:
		if m.ChatID != "chan-456" {
			t.Errorf("ChatID = %q", m.ChatID)
		}
		if m.UserID != "123" {
			t.Errorf("UserID = %q", m.UserID)
		}
		if m.UserName != "alice" {
			t.Errorf("UserName = %q", m.UserName)
		}
		if m.Text != "hello discord" {
			t.Errorf("Text = %q, want 'hello discord' (mention stripped)", m.Text)
		}
		if m.Identity.ActorPeer != "discord" {
			t.Errorf("ActorPeer = %q", m.Identity.ActorPeer)
		}
		if m.Identity.Account != "acct" {
			t.Errorf("Account = %q", m.Identity.Account)
		}
		if len(m.Attachments) != 1 || m.Attachments[0].URL != "https://example.com/x.png" {
			t.Errorf("Attachments = %+v", m.Attachments)
		}
	}
}

func TestDiscord_IgnoresBotAuthor(t *testing.T) {
	d := NewDiscord("discord", config.ChannelConfig{Provider: "discord"})
	stub := &stubDiscordAPI{started: make(chan struct{})}
	_ = d.Connect(context.Background(), stub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan IncomingMessage, 1)
	go func() {
		_ = d.Start(ctx, func(ctx context.Context, m IncomingMessage) error {
			got <- m
			return nil
		})
	}()
	<-stub.started
	ev := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ChannelID: "c",
			Content:   "beep",
			Author:    &discordgo.User{ID: "1", Bot: true},
		},
	}
	_ = stub.startHandler(ctx, ev)
	select {
	case m := <-got:
		t.Fatalf("bot-authored message should be dropped: %+v", m)
	default:
	}
}

func TestDiscord_Send(t *testing.T) {
	d := NewDiscord("discord", config.ChannelConfig{})
	stub := &stubDiscordAPI{}
	_ = d.Connect(context.Background(), stub)
	if err := d.Send(context.Background(), OutgoingMessage{ChatID: "c1", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(stub.sends) != 1 || stub.sends[0].channelID != "c1" || stub.sends[0].text != "hi" {
		t.Errorf("sends = %+v", stub.sends)
	}
}

func TestDiscord_SendWithReply(t *testing.T) {
	d := NewDiscord("discord", config.ChannelConfig{})
	stub := &stubDiscordAPI{}
	_ = d.Connect(context.Background(), stub)
	if err := d.Send(context.Background(), OutgoingMessage{ChatID: "c1", Text: "hi", ReplyToMsgID: "msg-9"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(stub.sends) != 1 || stub.sends[0].replyTo != "msg-9" {
		t.Errorf("sends = %+v", stub.sends)
	}
}

func TestDiscord_StartNotConnected(t *testing.T) {
	d := NewDiscord("discord", config.ChannelConfig{})
	if err := d.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil }); err == nil {
		t.Fatalf("Start should error when not connected")
	}
}

func TestNewDiscordSDKClient_RejectsEmptyToken(t *testing.T) {
	_, err := newDiscordSDKClient(config.ChannelConfig{Token: ""})
	if err == nil {
		t.Fatalf("expected error for empty token")
	}
}
