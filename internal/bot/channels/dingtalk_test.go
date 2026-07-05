package channels

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dingtalk "github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

type stubDingTalkAPI struct {
	startHandler func(ctx context.Context, data *dingtalk.BotCallbackDataModel) error
	sends        []dingTalkSend
	started      chan struct{}
}

type dingTalkSend struct {
	webhook string
	text    string
}

func (s *stubDingTalkAPI) Start(ctx context.Context, handler func(ctx context.Context, data *dingtalk.BotCallbackDataModel) error) error {
	s.startHandler = handler
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	return nil
}

func (s *stubDingTalkAPI) Send(ctx context.Context, webhook, text string) error {
	s.sends = append(s.sends, dingTalkSend{webhook: webhook, text: text})
	return nil
}

func TestDingTalk_ConnectWithStub(t *testing.T) {
	d := NewDingTalk("dingtalk", config.ChannelConfig{Provider: "dingtalk", AppID: "ai", AppSecret: "as"})
	stub := &stubDingTalkAPI{}
	if err := d.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestDingTalk_StartDispatchesMessages(t *testing.T) {
	d := NewDingTalk("dingtalk", config.ChannelConfig{Provider: "dingtalk", AppID: "acct"})
	stub := &stubDingTalkAPI{started: make(chan struct{})}
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

	data := &dingtalk.BotCallbackDataModel{
		SessionWebhook: "https://oapi.dingtalk.com/robot/sendByToken?token=xyz",
		SenderStaffId:  "staff123",
		SenderNick:     "alice",
		Text:           dingtalk.BotCallbackDataTextModel{Content: " @bot hello dingtalk"},
	}
	if err := stub.startHandler(ctx, data); err != nil {
		t.Fatalf("handler: %v", err)
	}
	select {
	case m := <-got:
		if m.ChatID != data.SessionWebhook {
			t.Errorf("ChatID = %q", m.ChatID)
		}
		if m.UserID != "staff123" {
			t.Errorf("UserID = %q", m.UserID)
		}
		if m.UserName != "alice" {
			t.Errorf("UserName = %q", m.UserName)
		}
		if m.Text != "hello dingtalk" {
			t.Errorf("Text = %q, want 'hello dingtalk'", m.Text)
		}
		if m.Identity.ActorPeer != "dingtalk" {
			t.Errorf("ActorPeer = %q", m.Identity.ActorPeer)
		}
	}
}

func TestDingTalk_Send(t *testing.T) {
	d := NewDingTalk("dingtalk", config.ChannelConfig{})
	stub := &stubDingTalkAPI{}
	_ = d.Connect(context.Background(), stub)
	if err := d.Send(context.Background(), OutgoingMessage{ChatID: "https://hook", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(stub.sends) != 1 || stub.sends[0].webhook != "https://hook" {
		t.Errorf("sends = %+v", stub.sends)
	}
}

func TestDingTalk_StartNotConnected(t *testing.T) {
	d := NewDingTalk("dingtalk", config.ChannelConfig{})
	if err := d.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil }); err == nil {
		t.Fatalf("Start should error")
	}
}

// TestDingTalkSDKClient_SendHTTP uses httptest to verify the real
// SDK-backed Send POSTs JSON to the SessionWebhook URL.
func TestDingTalkSDKClient_SendHTTP(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := &dingtalkSDKClient{}
	if err := c.Send(context.Background(), srv.URL, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(gotBody, `"content":"hello"`) {
		t.Errorf("body = %q", gotBody)
	}
	if !strings.Contains(gotBody, `"msgtype":"text"`) {
		t.Errorf("body = %q", gotBody)
	}
}
