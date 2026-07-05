package channels

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

func TestNew_UnknownProvider(t *testing.T) {
	_, err := New("x", config.ChannelConfig{Provider: "bogus"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestNew_KnownProviders(t *testing.T) {
	for _, p := range []string{"telegram", "feishu", "dingtalk", "slack", "qq", "websocket", "discord", "email", "openapi", "whatsapp"} {
		t.Run(p, func(t *testing.T) {
			c, err := New(p, config.ChannelConfig{Provider: p, Token: "x", AppID: "ai", AppSecret: "as"})
			if err != nil {
				t.Fatalf("New(%q): %v", p, err)
			}
			if c == nil {
				t.Fatalf("New(%q) returned nil", p)
			}
			if c.Name() != p {
				t.Errorf("Name = %q, want %q", c.Name(), p)
			}
		})
	}
}

// TestDispatcher_ConcurrentSend verifies the Dispatcher routes replies
// to the originating channel. It uses stub channels so no network is
// involved.
func TestDispatcher_ConcurrentSend(t *testing.T) {
	d := NewDispatcher()
	tg := NewTelegram("telegram", config.ChannelConfig{})
	stubTg := &stubTelegramBot{}
	_ = tg.Connect(context.Background(), stubTg)
	d.Register(tg)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = d.Send(context.Background(), OutgoingMessage{ChatID: "1", Text: "hi"}, "telegram")
	}()
	wg.Wait()

	// The Send should have landed on the telegram stub.
	if len(stubTg.sends) != 1 {
		t.Errorf("sends = %+v", stubTg.sends)
	}
}

func TestDispatcher_SendUnknownChannel(t *testing.T) {
	d := NewDispatcher()
	if err := d.Send(context.Background(), OutgoingMessage{}, "nope"); err == nil {
		t.Fatalf("Send should error for unknown channel")
	}
}
