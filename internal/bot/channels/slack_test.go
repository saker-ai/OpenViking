package channels

import (
	"context"
	"testing"

	"github.com/slack-go/slack/slackevents"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

type stubSlackAPI struct {
	startHandler func(ctx context.Context, ev *slackevents.MessageEvent) error
	sends        []slackSend
	started      chan struct{}
}

type slackSend struct {
	channelID string
	text      string
}

func (s *stubSlackAPI) Start(ctx context.Context, handler func(ctx context.Context, ev *slackevents.MessageEvent) error) error {
	s.startHandler = handler
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	return nil
}

func (s *stubSlackAPI) Send(ctx context.Context, channelID, text string) error {
	s.sends = append(s.sends, slackSend{channelID: channelID, text: text})
	return nil
}

func TestSlack_ConnectWithStub(t *testing.T) {
	s := NewSlack("slack", config.ChannelConfig{Provider: "slack", Token: "xoxb-t", AppSecret: "xapp-t"})
	stub := &stubSlackAPI{}
	if err := s.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestSlack_StartDispatchesMessages(t *testing.T) {
	s := NewSlack("slack", config.ChannelConfig{Provider: "slack", AppID: "acct"})
	stub := &stubSlackAPI{started: make(chan struct{})}
	if err := s.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan IncomingMessage, 1)
	go func() {
		_ = s.Start(ctx, func(ctx context.Context, m IncomingMessage) error {
			got <- m
			return nil
		})
	}()
	<-stub.started

	ev := &slackevents.MessageEvent{
		User:    "U123",
		Channel: "C456",
		Text:    "<@U0BOT> hello slack",
	}
	if err := stub.startHandler(ctx, ev); err != nil {
		t.Fatalf("handler: %v", err)
	}
	select {
	case m := <-got:
		if m.ChatID != "C456" {
			t.Errorf("ChatID = %q", m.ChatID)
		}
		if m.UserID != "U123" {
			t.Errorf("UserID = %q", m.UserID)
		}
		if m.Text != "hello slack" {
			t.Errorf("Text = %q, want 'hello slack' (mention stripped)", m.Text)
		}
		if m.Identity.ActorPeer != "slack" {
			t.Errorf("ActorPeer = %q", m.Identity.ActorPeer)
		}
	}
}

func TestSlack_Send(t *testing.T) {
	s := NewSlack("slack", config.ChannelConfig{})
	stub := &stubSlackAPI{}
	_ = s.Connect(context.Background(), stub)
	if err := s.Send(context.Background(), OutgoingMessage{ChatID: "C1", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(stub.sends) != 1 || stub.sends[0].channelID != "C1" {
		t.Errorf("sends = %+v", stub.sends)
	}
}

func TestSlack_StartNotConnected(t *testing.T) {
	s := NewSlack("slack", config.ChannelConfig{})
	if err := s.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil }); err == nil {
		t.Fatalf("Start should error")
	}
}

func TestNewSlackSDKClient_RejectsBadTokens(t *testing.T) {
	_, err := newSlackSDKClient(config.ChannelConfig{Token: "xoxb-t", AppSecret: "bad"})
	if err == nil {
		t.Fatalf("expected error for non-xapp app_secret")
	}
	_, err = newSlackSDKClient(config.ChannelConfig{Token: "bad", AppSecret: "xapp-t"})
	if err == nil {
		t.Fatalf("expected error for non-xoxb token")
	}
}
