package channels

import (
	"context"
	"testing"

	tgbot "github.com/go-telegram/bot"
	tgmodels "github.com/go-telegram/bot/models"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// stubTelegramBot implements telegramAPI for tests.
type stubTelegramBot struct {
	startHandler func(ctx context.Context, u *tgmodels.Update)
	started      chan struct{}
	sends        []tgbot.SendMessageParams
}

func (s *stubTelegramBot) Start(ctx context.Context, handler func(ctx context.Context, u *tgmodels.Update)) error {
	s.startHandler = handler
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	return nil
}

func (s *stubTelegramBot) SendMessage(ctx context.Context, p *tgbot.SendMessageParams) (*tgmodels.Message, error) {
	s.sends = append(s.sends, *p)
	return &tgmodels.Message{}, nil
}

func TestTelegram_ConnectWithStub(t *testing.T) {
	tg := NewTelegram("telegram", config.ChannelConfig{Provider: "telegram", Token: "x"})
	stub := &stubTelegramBot{}
	if err := tg.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if tg.bot != stub {
		t.Fatalf("bot not set")
	}
}

func TestTelegram_StartDispatchesMessages(t *testing.T) {
	tg := NewTelegram("telegram", config.ChannelConfig{Provider: "telegram", AppID: "acct"})
	stub := &stubTelegramBot{started: make(chan struct{})}
	if err := tg.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan IncomingMessage, 1)
	go func() {
		_ = tg.Start(ctx, func(ctx context.Context, m IncomingMessage) error {
			got <- m
			return nil
		})
	}()

	<-stub.started

	// Stub captured the handler when Start was called; fire one update.
	if stub.startHandler == nil {
		t.Fatalf("Start did not register a handler")
	}
	stub.startHandler(ctx, &tgmodels.Update{
		Message: &tgmodels.Message{
			ID:   1,
			Chat: tgmodels.Chat{ID: 42},
			From: &tgmodels.User{ID: 7, Username: "alice"},
			Text: "/ask hello",
		},
	})

	select {
	case m := <-got:
		if m.ChatID != "42" {
			t.Errorf("ChatID = %q", m.ChatID)
		}
		if m.UserID != "7" {
			t.Errorf("UserID = %q", m.UserID)
		}
		if m.UserName != "alice" {
			t.Errorf("UserName = %q", m.UserName)
		}
		if m.Text != "hello" {
			t.Errorf("Text = %q, want hello (command stripped)", m.Text)
		}
		if m.Identity.ActorPeer != "telegram" {
			t.Errorf("ActorPeer = %q", m.Identity.ActorPeer)
		}
		if m.Identity.Account != "acct" {
			t.Errorf("Account = %q", m.Identity.Account)
		}
	}
}

func TestTelegram_StartNotConnected(t *testing.T) {
	tg := NewTelegram("telegram", config.ChannelConfig{})
	err := tg.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil })
	if err == nil {
		t.Fatalf("Start should error when not connected")
	}
}

func TestTelegram_Send(t *testing.T) {
	tg := NewTelegram("telegram", config.ChannelConfig{})
	stub := &stubTelegramBot{}
	if err := tg.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := tg.Send(context.Background(), OutgoingMessage{ChatID: "9", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(stub.sends) != 1 || stub.sends[0].ChatID != "9" || stub.sends[0].Text != "hi" {
		t.Errorf("sends = %+v", stub.sends)
	}
}

func TestTelegram_IgnoresEmptyMessages(t *testing.T) {
	tg := NewTelegram("telegram", config.ChannelConfig{})
	stub := &stubTelegramBot{started: make(chan struct{})}
	_ = tg.Connect(context.Background(), stub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan IncomingMessage, 1)
	go func() {
		_ = tg.Start(ctx, func(ctx context.Context, m IncomingMessage) error {
			got <- m
			return nil
		})
	}()
	<-stub.started
	// nil Message -> ignored
	stub.startHandler(ctx, &tgmodels.Update{Message: nil})
	// empty text -> ignored
	stub.startHandler(ctx, &tgmodels.Update{
		Message: &tgmodels.Message{Chat: tgmodels.Chat{ID: 1}, From: &tgmodels.User{}, Text: "/cmd"},
	})
	select {
	case m := <-got:
		t.Fatalf("unexpected message %+v", m)
	default:
	}
}
