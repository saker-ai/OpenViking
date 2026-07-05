package hooks

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

func TestNew_Defaults(t *testing.T) {
	d := New(config.HooksConfig{URLs: []string{"http://x"}, Timeout: 0})
	if d.URLs()[0] != "http://x" {
		t.Errorf("URLs = %+v", d.URLs())
	}
	if d.timeout != 10*time.Second {
		t.Errorf("timeout = %s, want 10s", d.timeout)
	}
}

func TestDispatch_NoURLsIsNoop(t *testing.T) {
	d := New(config.HooksConfig{})
	d.Dispatch(context.Background(), Event{Type: "incoming"})
}

func TestDispatch_PostsToAllURLs(t *testing.T) {
	var got1, got2 atomic.Int32
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got1.Add(1)
		var ev Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		if ev.Type != "incoming" || ev.Channel != "telegram" {
			t.Errorf("ev = %+v", ev)
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got2.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv1.Close()
	defer srv2.Close()
	d := New(config.HooksConfig{URLs: []string{srv1.URL, srv2.URL}, Timeout: 5})
	d.Dispatch(context.Background(), Event{
		Type:    "incoming",
		Channel: "telegram",
		ChatID:  "1",
		Text:    "hi",
	})
	if got1.Load() != 1 || got2.Load() != 1 {
		t.Errorf("calls = %d/%d, want 1/1", got1.Load(), got2.Load())
	}
}

func TestDispatch_HTTPErrorDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	d := New(config.HooksConfig{URLs: []string{srv.URL}, Timeout: 5})
	d.Dispatch(context.Background(), Event{Type: "incoming"})
	// Should not panic, no error returned.
}

func TestDispatch_ContextCanceledStopsPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := New(config.HooksConfig{URLs: []string{srv.URL}, Timeout: 5})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Dispatch(ctx, Event{Type: "incoming"})
}
