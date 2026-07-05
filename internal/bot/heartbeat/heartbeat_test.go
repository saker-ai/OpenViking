package heartbeat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

type stubPinger struct {
	calls atomic.Int32
	last  Payload
}

func (s *stubPinger) Ping(ctx context.Context, p Payload) error {
	s.calls.Add(1)
	s.last = p
	return nil
}

func TestHeartbeat_DisabledByZeroInterval(t *testing.T) {
	h := New(config.HeartbeatConfig{Interval: 0}, "https://x", Payload{Bot: "v"}, &stubPinger{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestHeartbeat_LoopPingsUntilStop(t *testing.T) {
	stub := &stubPinger{}
	h := New(config.HeartbeatConfig{Interval: 1}, "https://x", Payload{Bot: "v", Account: "a"}, stub)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = h.Start(ctx)
		close(done)
	}()
	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done
	if stub.calls.Load() == 0 {
		t.Errorf("expected at least one ping, got %d", stub.calls.Load())
	}
	if stub.last.Bot != "v" {
		t.Errorf("last payload Bot = %q", stub.last.Bot)
	}
}

func TestHeartbeat_StopIsIdempotent(t *testing.T) {
	h := New(config.HeartbeatConfig{Interval: 1}, "https://x", Payload{}, &stubPinger{})
	h.Stop()
	h.Stop() // should not panic
}

func TestHTTPPinger_PostsJSON(t *testing.T) {
	var gotPath string
	var gotBody Payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := newHTTPPinger(srv.URL, "/api/v1/observer")
	if err := p.Ping(context.Background(), Payload{Bot: "vikingbot", Account: "acct", Channels: []string{"telegram"}}); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if gotPath != "/api/v1/observer" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody.Bot != "vikingbot" {
		t.Errorf("body Bot = %q", gotBody.Bot)
	}
	if len(gotBody.Channels) != 1 || gotBody.Channels[0] != "telegram" {
		t.Errorf("body Channels = %+v", gotBody.Channels)
	}
}

func TestHTTPPinger_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := newHTTPPinger(srv.URL, "/api/v1/observer")
	err := p.Ping(context.Background(), Payload{})
	if err == nil {
		t.Fatalf("Ping should error on 500")
	}
}
