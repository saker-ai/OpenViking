package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

type stubQQAPI struct {
	startHandler func(ctx context.Context, ev *qqInboundEvent) error
	sends        []qqOutbound
	started      chan struct{}
}

func (s *stubQQAPI) Start(ctx context.Context, handler func(ctx context.Context, ev *qqInboundEvent) error) error {
	s.startHandler = handler
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	return nil
}

func (s *stubQQAPI) Send(ctx context.Context, msg qqOutbound) error {
	s.sends = append(s.sends, msg)
	return nil
}

func TestQQ_ConnectWithStub(t *testing.T) {
	q := NewQQ("qq", config.ChannelConfig{Provider: "qq", AppID: "ai"})
	stub := &stubQQAPI{}
	if err := q.Connect(context.Background(), stub, nil); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestQQ_StartDispatchesMessages(t *testing.T) {
	q := NewQQ("qq", config.ChannelConfig{Provider: "qq", AppID: "acct"})
	stub := &stubQQAPI{started: make(chan struct{})}
	if err := q.Connect(context.Background(), stub, nil); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan IncomingMessage, 1)
	go func() {
		_ = q.Start(ctx, func(ctx context.Context, m IncomingMessage) error {
			got <- m
			return nil
		})
	}()
	<-stub.started

	ev := &qqInboundEvent{
		EventType:   "GROUP_AT_MESSAGE_CREATE",
		MsgID:       "msg-1",
		GroupOpenID: "group-xyz",
		UserOpenID:  "",
		AuthorID:    "user-abc",
		Content:     "@bot hello qq",
	}
	if err := stub.startHandler(ctx, ev); err != nil {
		t.Fatalf("handler: %v", err)
	}
	select {
	case m := <-got:
		if m.ChatID != "group:group-xyz" {
			t.Errorf("ChatID = %q", m.ChatID)
		}
		if m.UserID != "user-abc" {
			t.Errorf("UserID = %q", m.UserID)
		}
		if m.Text != "hello qq" {
			t.Errorf("Text = %q, want 'hello qq'", m.Text)
		}
		if m.Identity.ActorPeer != "qq" {
			t.Errorf("ActorPeer = %q", m.Identity.ActorPeer)
		}
	}
}

func TestQQ_Send(t *testing.T) {
	q := NewQQ("qq", config.ChannelConfig{})
	stub := &stubQQAPI{}
	_ = q.Connect(context.Background(), stub, nil)
	if err := q.Send(context.Background(), OutgoingMessage{ChatID: "group:g1", Text: "hi", ReplyToMsgID: "m1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(stub.sends) != 1 {
		t.Fatalf("sends = %+v", stub.sends)
	}
	out := stub.sends[0]
	if out.Path != "/v2/groups/g1/messages" {
		t.Errorf("Path = %q", out.Path)
	}
	if out.Content["content"] != "hi" {
		t.Errorf("Content = %+v", out.Content)
	}
	if out.MsgID != "m1" {
		t.Errorf("MsgID = %q", out.MsgID)
	}
}

func TestQQ_SendUser(t *testing.T) {
	q := NewQQ("qq", config.ChannelConfig{})
	stub := &stubQQAPI{}
	_ = q.Connect(context.Background(), stub, nil)
	if err := q.Send(context.Background(), OutgoingMessage{ChatID: "user:u1", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if stub.sends[0].Path != "/v2/users/u1/messages" {
		t.Errorf("Path = %q", stub.sends[0].Path)
	}
}

func TestQQ_SendInvalidChatID(t *testing.T) {
	q := NewQQ("qq", config.ChannelConfig{})
	stub := &stubQQAPI{}
	_ = q.Connect(context.Background(), stub, nil)
	if err := q.Send(context.Background(), OutgoingMessage{ChatID: "bogus", Text: "hi"}); err == nil {
		t.Fatalf("Send should error")
	}
}

func TestQQ_StartNotConnected(t *testing.T) {
	q := NewQQ("qq", config.ChannelConfig{})
	if err := q.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil }); err == nil {
		t.Fatalf("Start should error")
	}
}

// TestQQHTTPAPI_WebhookHandler exercises the real webhook handler via
// httptest.NewServer. No network egress.
func TestQQHTTPAPI_WebhookHandler(t *testing.T) {
	api := newQQHTTPAPI(config.ChannelConfig{Provider: "qq", AppID: "acct"}, nil)
	got := make(chan *qqInboundEvent, 1)
	api.handlerFn = func(ctx context.Context, ev *qqInboundEvent) error {
		got <- ev
		return nil
	}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"event_name": "GROUP_AT_MESSAGE_CREATE",
		"d": map[string]any{
			"msg_id":       "m1",
			"group_openid": "group-xyz",
			"author_id":    "user-abc",
			"content":      "hello qq",
		},
	})
	resp, err := http.Post(srv.URL+"/qq/callback", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	select {
	case ev := <-got:
		if ev.GroupOpenID != "group-xyz" {
			t.Errorf("GroupOpenID = %q", ev.GroupOpenID)
		}
		if ev.Content != "hello qq" {
			t.Errorf("Content = %q", ev.Content)
		}
	default:
		t.Fatalf("handler not called")
	}
}

// TestQQHTTPAPI_SendHTTP exercises the real outbound HTTP client via
// httptest.NewServer.
func TestQQHTTPAPI_SendHTTP(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	api := newQQHTTPAPI(config.ChannelConfig{Provider: "qq", BaseURL: srv.URL}, func() string { return "tok" })
	if err := api.Send(context.Background(), qqOutbound{
		Path:    "/v2/groups/g1/messages",
		MsgType: "text",
		Content: map[string]string{"content": "hi"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/v2/groups/g1/messages" {
		t.Errorf("Path = %q", gotPath)
	}
	if gotAuth != "QQBot tok" {
		t.Errorf("Auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"content":"hi"`) {
		t.Errorf("body = %q", gotBody)
	}
}
