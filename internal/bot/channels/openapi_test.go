package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// stubOpenAPIAPI implements openAPIAPI for tests that don't want to
// spin up a real net/http listener.
type stubOpenAPIAPI struct {
	startHandler Handler
	started      chan struct{}
	sends        []OutgoingMessage
	sendErr      error
}

func (s *stubOpenAPIAPI) Start(ctx context.Context, handler Handler) error {
	s.startHandler = handler
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	return nil
}

func (s *stubOpenAPIAPI) Send(ctx context.Context, msg OutgoingMessage) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sends = append(s.sends, msg)
	return nil
}

func openAPITestConfig(addr, token string) config.ChannelConfig {
	return config.ChannelConfig{
		Provider:  "openapi",
		Endpoint:  addr,
		Token:     token,
		AppID:     "acct",
		Extra:     map[string]any{"timeout_seconds": 5},
	}
}

func TestOpenAPI_ParseSettings(t *testing.T) {
	cfg := openAPITestConfig(":8080", "secret")
	s := parseOpenAPISettings(cfg)
	if s.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q", s.ListenAddr)
	}
	if s.GatewayToken != "secret" {
		t.Errorf("GatewayToken = %q", s.GatewayToken)
	}
	if s.TimeoutSec != 5 {
		t.Errorf("TimeoutSec = %d", s.TimeoutSec)
	}
}

func TestOpenAPI_ParseSettingsDefaults(t *testing.T) {
	cfg := config.ChannelConfig{Provider: "openapi"}
	s := parseOpenAPISettings(cfg)
	if s.ListenAddr != ":0" {
		t.Errorf("ListenAddr = %q, want ':0' default", s.ListenAddr)
	}
	if s.TimeoutSec != 300 {
		t.Errorf("TimeoutSec = %d, want 300 default", s.TimeoutSec)
	}
}

func TestOpenAPI_ConnectWithStub(t *testing.T) {
	o := NewOpenAPI("openapi", openAPITestConfig(":0", ""))
	stub := &stubOpenAPIAPI{}
	if err := o.Connect(context.Background(), stub); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if o.api != stub {
		t.Fatalf("api not set")
	}
}

func TestOpenAPI_StartNotConnected(t *testing.T) {
	o := NewOpenAPI("openapi", openAPITestConfig(":0", ""))
	if err := o.Start(context.Background(), func(context.Context, IncomingMessage) error { return nil }); err == nil {
		t.Fatalf("Start should error when not connected")
	}
}

// startTestServer boots a real openAPIServer on a random port and
// returns the server, base URL, and a cancel func. The handler may be
// nil. Tests that need to call Send on the server use the returned
// *openAPIServer.
func startTestServer(t *testing.T, token string, handler Handler) (*openAPIServer, string, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := openAPITestConfig(addr, token)
	srv, err := newOpenAPIServer(cfg)
	if err != nil {
		t.Fatalf("newOpenAPIServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx, handler) }()
	// Wait until the listener is up.
	for i := 0; i < 100; i++ {
		c, derr := net.Dial("tcp", addr)
		if derr == nil {
			_ = c.Close()
			return srv, "http://" + addr, cancel
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			break
		}
	}
	cancel()
	t.Fatalf("server did not start in time")
	return nil, "", cancel
}

func TestOpenAPI_HealthEndpoint(t *testing.T) {
	_, url, cancel := startTestServer(t, "", func(context.Context, IncomingMessage) error { return nil })
	defer cancel()
	resp, err := http.Get(url + "/health")
	if err != nil {
		t.Fatalf("Get /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "healthy" {
		t.Errorf("status = %q", body["status"])
	}
}

func TestOpenAPI_AuthRejectsMissingToken(t *testing.T) {
	_, url, cancel := startTestServer(t, "secret", func(context.Context, IncomingMessage) error { return nil })
	defer cancel()
	resp, err := http.Post(url+"/chat", "application/json", strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestOpenAPI_AuthRejectsBadToken(t *testing.T) {
	_, url, cancel := startTestServer(t, "secret", func(context.Context, IncomingMessage) error { return nil })
	defer cancel()
	req, _ := http.NewRequest("POST", url+"/chat", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("X-Gateway-Token", "wrong")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestOpenAPI_ChatHappyPath(t *testing.T) {
	_, url, cancel := startTestServer(t, "secret", func(ctx context.Context, m IncomingMessage) error {
		if m.Text != "hello" {
			t.Errorf("handler got Text = %q", m.Text)
		}
		if m.Identity.ActorPeer != "openapi" {
			t.Errorf("ActorPeer = %q", m.Identity.ActorPeer)
		}
		return nil
	})
	defer cancel()
	req, _ := http.NewRequest("POST", url+"/chat", strings.NewReader(`{"message":"hello","session_id":"s1"}`))
	req.Header.Set("X-Gateway-Token", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	// No reply was delivered via Send, so expect 504 timeout.
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (no reply delivered)", resp.StatusCode)
	}
}

func TestOpenAPI_ChatReceivesReply(t *testing.T) {
	var srv *openAPIServer
	srv, url, cancel := startTestServer(t, "secret", func(ctx context.Context, m IncomingMessage) error {
		// Reply via the server's own Send method (the same path the
		// Dispatcher takes). Run in a goroutine so the handler returns
		// immediately, letting the HTTP handler block on pending.done.
		go func() {
			if srv != nil {
				_ = srv.Send(ctx, OutgoingMessage{ChatID: m.ChatID, Text: "hi back", ReplyToMsgID: "r-1"})
			}
		}()
		return nil
	})
	defer cancel()
	body := `{"message":"hello","session_id":"s2"}`
	req, _ := http.NewRequest("POST", url+"/chat", strings.NewReader(body))
	req.Header.Set("X-Gateway-Token", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cr.SessionID != "s2" {
		t.Errorf("SessionID = %q", cr.SessionID)
	}
	if cr.Message != "hi back" {
		t.Errorf("Message = %q", cr.Message)
	}
	if cr.ResponseID != "r-1" {
		t.Errorf("ResponseID = %q", cr.ResponseID)
	}
}

func TestOpenAPI_SessionsCRUD(t *testing.T) {
	_, url, cancel := startTestServer(t, "secret", func(context.Context, IncomingMessage) error { return nil })
	defer cancel()
	// Create.
	req, _ := http.NewRequest("POST", url+"/sessions", strings.NewReader(`{"user_id":"alice"}`))
	req.Header.Set("X-Gateway-Token", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	sid, _ := created["session_id"].(string)
	if sid == "" {
		t.Fatalf("no session_id in response")
	}
	// Get.
	req, _ = http.NewRequest("GET", url+"/sessions/"+sid, nil)
	req.Header.Set("X-Gateway-Token", "secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	// List.
	req, _ = http.NewRequest("GET", url+"/sessions", nil)
	req.Header.Set("X-Gateway-Token", "secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list sessionListResponse
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if list.Total < 1 {
		t.Errorf("list total = %d", list.Total)
	}
	// Delete.
	req, _ = http.NewRequest("DELETE", url+"/sessions/"+sid, nil)
	req.Header.Set("X-Gateway-Token", "secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	// Get after delete = 404.
	req, _ = http.NewRequest("GET", url+"/sessions/"+sid, nil)
	req.Header.Set("X-Gateway-Token", "secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get-after-delete: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get-after-delete status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestOpenAPI_Feedback(t *testing.T) {
	_, url, cancel := startTestServer(t, "secret", func(context.Context, IncomingMessage) error { return nil })
	defer cancel()
	body := `{"session_id":"s1","response_id":"r1","feedback_type":"thumbs_up","feedback_score":1.0,"feedback_text":"great"}`
	req, _ := http.NewRequest("POST", url+"/feedback", strings.NewReader(body))
	req.Header.Set("X-Gateway-Token", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("feedback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var fb map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&fb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fb["accepted"] != true {
		t.Errorf("accepted = %v", fb["accepted"])
	}
	if fb["feedback_type"] != "thumbs_up" {
		t.Errorf("feedback_type = %v", fb["feedback_type"])
	}
}

func TestOpenAPI_FeedbackRejectsMissing(t *testing.T) {
	_, url, cancel := startTestServer(t, "secret", func(context.Context, IncomingMessage) error { return nil })
	defer cancel()
	body := `{"session_id":"s1"}`
	req, _ := http.NewRequest("POST", url+"/feedback", strings.NewReader(body))
	req.Header.Set("X-Gateway-Token", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("feedback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOpenAPI_ChatStream(t *testing.T) {
	var srv *openAPIServer
	srv, url, cancel := startTestServer(t, "secret", func(ctx context.Context, m IncomingMessage) error {
		go func() {
			if srv == nil {
				return
			}
			defer srv.dropPending(m.ChatID)
			p, ok := srv.lookupPending(m.ChatID)
			if !ok {
				return
			}
			p.addEvent("content_delta", "Hello ")
			p.addEvent("content_delta", "world")
			// Mirror what openAPIServer.Send does: emit a final
			// "response" event before unblocking the consumer.
			p.addEvent("response", map[string]any{
				"content":     "Hello world",
				"response_id": "r-1",
			})
			p.setFinal("Hello world", "r-1")
			p.closeStream()
		}()
		return nil
	})
	defer cancel()
	req, _ := http.NewRequest("POST", url+"/chat/stream", strings.NewReader(`{"message":"hi","session_id":"sx","stream":true}`))
	req.Header.Set("X-Gateway-Token", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := string(raw)
	if !strings.Contains(out, "Hello ") || !strings.Contains(out, "world") {
		t.Errorf("stream body missing deltas: %q", out)
	}
	if !strings.Contains(out, `"event":"response"`) {
		t.Errorf("stream body missing final response event: %q", out)
	}
}

func TestOpenAPI_MethodNotAllowed(t *testing.T) {
	_, url, cancel := startTestServer(t, "secret", func(context.Context, IncomingMessage) error { return nil })
	defer cancel()
	req, _ := http.NewRequest("GET", url+"/chat", nil)
	req.Header.Set("X-Gateway-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestOpenAPI_SendNoPending(t *testing.T) {
	o := NewOpenAPI("openapi", openAPITestConfig(":0", ""))
	stub := &stubOpenAPIAPI{}
	_ = o.Connect(context.Background(), stub)
	// The stub always succeeds even when no pending; the real server
	// path is exercised via TestOpenAPI_ChatReceivesReply.
	if err := o.Send(context.Background(), OutgoingMessage{ChatID: "nope", Text: "x"}); err != nil {
		t.Fatalf("Send via stub: %v", err)
	}
}

func TestOpenAPI_SendNotConnected(t *testing.T) {
	o := NewOpenAPI("openapi", openAPITestConfig(":0", ""))
	if err := o.Send(context.Background(), OutgoingMessage{ChatID: "x", Text: "y"}); err == nil {
		t.Fatalf("Send should error when not connected")
	}
}

func TestOpenAPI_DecodeJSONBodyRejectsEmpty(t *testing.T) {
	req, _ := http.NewRequest("POST", "/", bytes.NewReader(nil))
	if err := decodeJSONBody(req, &chatRequest{}); err == nil {
		t.Fatalf("expected error for empty body")
	}
}

func TestOpenAPI_BuildReplyEmail(t *testing.T) {
	raw := buildReplyEmail("bot@example.com", "alice@example.com", "Re: Hi", "hello there", "<msg-1>")
	s := string(raw)
	if !strings.Contains(s, "From: bot@example.com") {
		t.Errorf("missing From: %s", s)
	}
	if !strings.Contains(s, "To: alice@example.com") {
		t.Errorf("missing To: %s", s)
	}
	if !strings.Contains(s, "Subject: Re: Hi") {
		t.Errorf("missing Subject: %s", s)
	}
	if !strings.Contains(s, "In-Reply-To: <msg-1>") {
		t.Errorf("missing In-Reply-To: %s", s)
	}
	if !strings.Contains(s, "hello there") {
		t.Errorf("missing body: %s", s)
	}
}

// lookupPending exposes the pending map for tests in the same package.
func (s *openAPIServer) lookupPending(sessionID string) (*openAPIPending, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pending[sessionID]
	return p, ok
}
