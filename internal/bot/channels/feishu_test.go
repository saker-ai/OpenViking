package channels

import (
	"context"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

type stubFeishuAPI struct {
	startHandler func(ctx context.Context, ev *larkim.P2MessageReceiveV1) error
	sends        []feishuSend
	started      chan struct{}
}

type feishuSend struct {
	chatID string
	text   string
}

func (s *stubFeishuAPI) Start(ctx context.Context, handler func(ctx context.Context, ev *larkim.P2MessageReceiveV1) error) error {
	s.startHandler = handler
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	return nil
}

func (s *stubFeishuAPI) Send(ctx context.Context, chatID, text string) error {
	s.sends = append(s.sends, feishuSend{chatID: chatID, text: text})
	return nil
}

func TestFeishu_ConnectWithStub(t *testing.T) {
	f := NewFeishu("feishu", config.ChannelConfig{Provider: "feishu", AppID: "ai", AppSecret: "as"})
	stub := &stubFeishuAPI{}
	if err := f.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestFeishu_StartDispatchesMessages(t *testing.T) {
	f := NewFeishu("feishu", config.ChannelConfig{Provider: "feishu", AppID: "acct"})
	stub := &stubFeishuAPI{started: make(chan struct{})}
	if err := f.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan IncomingMessage, 1)
	go func() {
		_ = f.Start(ctx, func(ctx context.Context, m IncomingMessage) error {
			got <- m
			return nil
		})
	}()
	<-stub.started

	// Build a P2MessageReceiveV1 with chat_id and content.
	chatID := "oc_123"
	content := `{"text":"hello feishu"}`
	openID := "ou_456"
	ev := &larkim.P2MessageReceiveV1{}
	ev.Event = &larkim.P2MessageReceiveV1Data{}
	ev.Event.Message = &larkim.EventMessage{
		ChatId:  &chatID,
		Content: &content,
	}
	ev.Event.Sender = &larkim.EventSender{
		SenderId: &larkim.UserId{OpenId: &openID},
	}
	if err := stub.startHandler(ctx, ev); err != nil {
		t.Fatalf("handler: %v", err)
	}
	select {
	case m := <-got:
		if m.ChatID != "oc_123" {
			t.Errorf("ChatID = %q", m.ChatID)
		}
		if m.UserID != "ou_456" {
			t.Errorf("UserID = %q", m.UserID)
		}
		if m.Text != "hello feishu" {
			t.Errorf("Text = %q", m.Text)
		}
		if m.Identity.ActorPeer != "feishu" {
			t.Errorf("ActorPeer = %q", m.Identity.ActorPeer)
		}
	}
}

func TestFeishu_Send(t *testing.T) {
	f := NewFeishu("feishu", config.ChannelConfig{})
	stub := &stubFeishuAPI{}
	_ = f.Connect(context.Background(), stub)
	if err := f.Send(context.Background(), OutgoingMessage{ChatID: "oc_1", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(stub.sends) != 1 || stub.sends[0].chatID != "oc_1" || stub.sends[0].text != "hi" {
		t.Errorf("sends = %+v", stub.sends)
	}
}

func TestFeishu_StartNotConnected(t *testing.T) {
	f := NewFeishu("feishu", config.ChannelConfig{})
	if err := f.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil }); err == nil {
		t.Fatalf("Start should error when not connected")
	}
}
