package channels

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// stubEmailAPI implements emailAPI for tests.
type stubEmailAPI struct {
	fetched     []emailInbound
	fetchErr    error
	rangeFetched []emailInbound
	rangeErr    error
	sends       []emailSend
	sendErr     error
}

type emailSend struct {
	from string
	to   []string
	raw  []byte
}

func (s *stubEmailAPI) FetchNew(ctx context.Context) ([]emailInbound, error) {
	if s.fetchErr != nil {
		return nil, s.fetchErr
	}
	return s.fetched, nil
}

func (s *stubEmailAPI) FetchRange(ctx context.Context, since, before time.Time) ([]emailInbound, error) {
	if s.rangeErr != nil {
		return nil, s.rangeErr
	}
	return s.rangeFetched, nil
}

func (s *stubEmailAPI) Send(ctx context.Context, from string, to []string, raw []byte) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sends = append(s.sends, emailSend{from: from, to: to, raw: raw})
	return nil
}

func emailTestConfig() config.ChannelConfig {
	return config.ChannelConfig{
		Provider:  "email",
		Endpoint:  "imap.example.com",
		BaseURL:   "smtp.example.com",
		AppID:     "user@example.com",
		AppSecret: "secret-imap",
		Token:     "secret-smtp",
		Extra: map[string]any{
			"consent_granted":      true,
			"auto_reply_enabled":   true,
			"mark_seen":            true,
			"poll_interval_seconds": 5,
			"smtp_username":        "user@example.com",
			"from_address":         "bot@example.com",
		},
	}
}

func TestEmail_ParseSettings(t *testing.T) {
	cfg := emailTestConfig()
	s, err := parseEmailSettings(cfg)
	if err != nil {
		t.Fatalf("parseEmailSettings: %v", err)
	}
	if s.IMAPHost != "imap.example.com" {
		t.Errorf("IMAPHost = %q", s.IMAPHost)
	}
	if s.IMAPPort != 993 {
		t.Errorf("IMAPPort = %d, want 993 (default for SSL)", s.IMAPPort)
	}
	if s.SMTPPort != 587 {
		t.Errorf("SMTPPort = %d, want 587 (default for STARTTLS)", s.SMTPPort)
	}
	if !s.Consent {
		t.Errorf("Consent = false")
	}
	if !s.AutoReply {
		t.Errorf("AutoReply = false")
	}
	if s.SMTPUser != "user@example.com" {
		t.Errorf("SMTPUser = %q", s.SMTPUser)
	}
	if s.SMTPPass != "secret-smtp" {
		t.Errorf("SMTPPass = %q", s.SMTPPass)
	}
}

func TestEmail_ParseSettingsRejectsMissing(t *testing.T) {
	_, err := parseEmailSettings(config.ChannelConfig{Provider: "email"})
	if err == nil {
		t.Fatalf("expected error for missing config")
	}
}

func TestEmail_ReplySubject(t *testing.T) {
	cases := []struct {
		base, prefix, want string
	}{
		{"Hello", "Re: ", "Re: Hello"},
		{"re: already", "Re: ", "re: already"},
		{"Re: Uppercase", "Re: ", "Re: Uppercase"},
		{"", "Re: ", "Re: vikingbot reply"},
		{"Hello", "", "Re: Hello"}, // default prefix when empty
	}
	for _, c := range cases {
		got := replySubject(c.base, c.prefix)
		if got != c.want {
			t.Errorf("replySubject(%q,%q) = %q, want %q", c.base, c.prefix, got, c.want)
		}
	}
}

func TestEmail_ParseEmailBody_PlainSingle(t *testing.T) {
	raw := "From: alice@example.com\r\n" +
		"Subject: Hello\r\n" +
		"Message-ID: <abc@example.com>\r\n" +
		"Date: Mon, 1 Jan 2024 00:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Hi there."
	p, err := parseEmailBody(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parseEmailBody: %v", err)
	}
	if p.Sender != "alice@example.com" {
		t.Errorf("Sender = %q", p.Sender)
	}
	if p.Subject != "Hello" {
		t.Errorf("Subject = %q", p.Subject)
	}
	if p.MessageID != "<abc@example.com>" {
		t.Errorf("MessageID = %q", p.MessageID)
	}
	if p.Body != "Hi there." {
		t.Errorf("Body = %q", p.Body)
	}
}

func TestEmail_ParseEmailBody_MultipartPrefersPlain(t *testing.T) {
	raw := "From: bob@example.com\r\n" +
		"Subject: Multi\r\n" +
		"Content-Type: multipart/alternative; boundary=\"bnd\"\r\n" +
		"\r\n" +
		"--bnd\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"plain body\r\n" +
		"--bnd\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>html body</p>\r\n" +
		"--bnd--\r\n"
	p, err := parseEmailBody(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parseEmailBody: %v", err)
	}
	if p.Body != "plain body" {
		t.Errorf("Body = %q, want 'plain body'", p.Body)
	}
}

func TestEmail_ParseEmailBody_HTMLFallback(t *testing.T) {
	raw := "From: carol@example.com\r\n" +
		"Subject: HTML Only\r\n" +
		"Content-Type: multipart/alternative; boundary=\"bnd\"\r\n" +
		"\r\n" +
		"--bnd\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>Hello <b>world</b></p><br/><p>line2</p>\r\n" +
		"--bnd--\r\n"
	p, err := parseEmailBody(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parseEmailBody: %v", err)
	}
	if !strings.Contains(p.Body, "Hello world") {
		t.Errorf("Body = %q, want stripped text containing 'Hello world'", p.Body)
	}
	if !strings.Contains(p.Body, "line2") {
		t.Errorf("Body = %q, want 'line2' on its own line", p.Body)
	}
}

func TestEmail_HTMLToText(t *testing.T) {
	out := htmlToText("<p>Hello <b>world</b></p><br/><p>line2</p>")
	if !strings.Contains(out, "Hello world") {
		t.Errorf("htmlToText: %q missing 'Hello world'", out)
	}
	if !strings.Contains(out, "line2") {
		t.Errorf("htmlToText: %q missing 'line2'", out)
	}
}

func TestEmail_StartDispatchesMessages(t *testing.T) {
	cfg := emailTestConfig()
	e := NewEmail("email", cfg)
	stub := &stubEmailAPI{
		fetched: []emailInbound{
			{Sender: "alice@example.com", Subject: "Hi", Body: "Email received.\nFrom: alice@example.com\nSubject: Hi\n\nhello", MessageID: "<m1>"},
		},
	}
	if err := e.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan IncomingMessage, 1)
	// Run one poll cycle manually since Start runs a ticker loop.
	go func() {
		_ = e.pollOnce(ctx, func(ctx context.Context, m IncomingMessage) error {
			got <- m
			return nil
		})
	}()
	select {
	case m := <-got:
		if m.ChatID != "alice@example.com" {
			t.Errorf("ChatID = %q", m.ChatID)
		}
		if m.UserID != "alice@example.com" {
			t.Errorf("UserID = %q", m.UserID)
		}
		if !strings.Contains(m.Text, "hello") {
			t.Errorf("Text = %q", m.Text)
		}
		if m.Identity.ActorPeer != "email" {
			t.Errorf("ActorPeer = %q", m.Identity.ActorPeer)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no message received")
	}
}

func TestEmail_StartNotConnected(t *testing.T) {
	e := NewEmail("email", emailTestConfig())
	if err := e.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil }); err == nil {
		t.Fatalf("Start should error when not connected")
	}
}

func TestEmail_StartRejectsNoConsent(t *testing.T) {
	cfg := emailTestConfig()
	cfg.Extra["consent_granted"] = false
	e := NewEmail("email", cfg)
	stub := &stubEmailAPI{}
	_ = e.Connect(context.Background(), stub)
	if err := e.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil }); err == nil {
		t.Fatalf("Start should error when consent_granted is false")
	}
}

func TestEmail_Send(t *testing.T) {
	cfg := emailTestConfig()
	e := NewEmail("email", cfg)
	stub := &stubEmailAPI{}
	_ = e.Connect(context.Background(), stub)
	if err := e.Send(context.Background(), OutgoingMessage{ChatID: "alice@example.com", Text: "reply!"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(stub.sends) != 1 {
		t.Fatalf("sends = %+v", stub.sends)
	}
	s := stub.sends[0]
	if s.from != "bot@example.com" {
		t.Errorf("from = %q", s.from)
	}
	if len(s.to) != 1 || s.to[0] != "alice@example.com" {
		t.Errorf("to = %+v", s.to)
	}
	if !bytes.Contains(s.raw, []byte("Subject: Re: vikingbot reply")) && !bytes.Contains(s.raw, []byte("Subject: Re:")) {
		t.Errorf("raw missing Re: subject: %s", s.raw)
	}
	if !bytes.Contains(s.raw, []byte("reply!")) {
		t.Errorf("raw missing body: %s", s.raw)
	}
}

func TestEmail_SendRespectsAutoReply(t *testing.T) {
	cfg := emailTestConfig()
	cfg.Extra["auto_reply_enabled"] = false
	e := NewEmail("email", cfg)
	stub := &stubEmailAPI{}
	_ = e.Connect(context.Background(), stub)
	if err := e.Send(context.Background(), OutgoingMessage{ChatID: "x@y.com", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(stub.sends) != 0 {
		t.Fatalf("auto_reply_enabled=false should suppress sends; got %+v", stub.sends)
	}
}

func TestEmail_SendNotConnected(t *testing.T) {
	e := NewEmail("email", emailTestConfig())
	if err := e.Send(context.Background(), OutgoingMessage{ChatID: "x@y.com", Text: "hi"}); err == nil {
		t.Fatalf("Send should error when not connected")
	}
}

func TestEmail_SendPropagatesError(t *testing.T) {
	e := NewEmail("email", emailTestConfig())
	stub := &stubEmailAPI{sendErr: errors.New("boom")}
	_ = e.Connect(context.Background(), stub)
	err := e.Send(context.Background(), OutgoingMessage{ChatID: "x@y.com", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Send should propagate error, got %v", err)
	}
}

// TestEmail_BackfillNotConnected verifies that Backfill errors when the
// channel has not been connected.
func TestEmail_BackfillNotConnected(t *testing.T) {
	e := NewEmail("email", emailTestConfig())
	_, err := e.Backfill(context.Background(), time.Now().Add(-30*24*time.Hour), time.Now())
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("Backfill should error when not connected, got %v", err)
	}
}

// TestEmail_BackfillDelegates verifies that Backfill delegates to the
// underlying api.FetchRange and returns the stubbed messages.
func TestEmail_BackfillDelegates(t *testing.T) {
	e := NewEmail("email", emailTestConfig())
	stub := &stubEmailAPI{
		rangeFetched: []emailInbound{
			{Sender: "a@b.com", Subject: "old msg", UID: 42},
			{Sender: "c@d.com", Subject: "older msg", UID: 7},
		},
	}
	_ = e.Connect(context.Background(), stub)
	since := time.Now().Add(-30 * 24 * time.Hour)
	before := time.Now()
	got, err := e.Backfill(context.Background(), since, before)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].UID != 42 {
		t.Errorf("first msg UID=%d, want 42", got[0].UID)
	}
}

// TestEmail_BackfillPropagatesError verifies that FetchRange errors
// surface to the caller.
func TestEmail_BackfillPropagatesError(t *testing.T) {
	e := NewEmail("email", emailTestConfig())
	stub := &stubEmailAPI{rangeErr: errors.New("imap down")}
	_ = e.Connect(context.Background(), stub)
	_, err := e.Backfill(context.Background(), time.Now().Add(-7*24*time.Hour), time.Now())
	if err == nil || !strings.Contains(err.Error(), "imap down") {
		t.Fatalf("Backfill should propagate error, got %v", err)
	}
}

// TestEmail_BackfillEmptyRange verifies that an empty range result is
// not an error.
func TestEmail_BackfillEmptyRange(t *testing.T) {
	e := NewEmail("email", emailTestConfig())
	stub := &stubEmailAPI{rangeFetched: nil}
	_ = e.Connect(context.Background(), stub)
	got, err := e.Backfill(context.Background(), time.Now().Add(-24*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d messages, want 0", len(got))
	}
}
