package integrations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

func TestNew_DisabledByDefault(t *testing.T) {
	l := New(config.LangfuseConfig{})
	if l.cfg.Enabled {
		t.Errorf("Enabled should be false")
	}
}

func TestLangfuse_DisabledNoop(t *testing.T) {
	l := New(config.LangfuseConfig{})
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	l.Trace("id", "name", nil)
	if l.Pending() != 0 {
		t.Errorf("Pending = %d, want 0 (disabled)", l.Pending())
	}
}

func TestLangfuse_FlushOnStop(t *testing.T) {
	var posts atomic.Int32
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	l := New(config.LangfuseConfig{
		Enabled:       true,
		BaseURL:       srv.URL,
		PublicKey:     "pk",
		SecretKey:     "sk",
		FlushInterval: 60, // long; we'll trigger via Stop
	})
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	l.Trace("trace-1", "test", map[string]any{"k": "v"})
	l.Generation("trace-1", "gen-1", "hello", "hi back", nil)
	l.Stop()
	if posts.Load() != 1 {
		t.Fatalf("posts = %d, want 1", posts.Load())
	}
	batch, _ := lastBody["batch"].([]any)
	if len(batch) != 2 {
		t.Errorf("batch len = %d, want 2", len(batch))
	}
}

func TestLangfuse_AuthHeaderSent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	l := New(config.LangfuseConfig{
		Enabled:   true,
		BaseURL:   srv.URL,
		PublicKey: "pk-abc",
		SecretKey: "sk-xyz",
	})
	l.Trace("t1", "name", nil)
	l.flush(context.Background())
	if gotAuth == "" || !startsWith(gotAuth, "Basic ") {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestLangfuse_ReenqueueOn500(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	l := New(config.LangfuseConfig{
		Enabled:   true,
		BaseURL:   srv.URL,
		PublicKey: "pk",
		SecretKey: "sk",
	})
	l.Trace("t1", "name", nil)
	l.flush(context.Background())
	if l.Pending() != 1 {
		t.Errorf("Pending = %d, want 1 (reenqueued)", l.Pending())
	}
}

func TestLangfuse_StartStopIdempotent(t *testing.T) {
	l := New(config.LangfuseConfig{Enabled: true, BaseURL: "https://x", PublicKey: "p", SecretKey: "s"})
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	l.Stop()
	l.Stop() // should not panic
}

// startsWith is a tiny helper to avoid pulling in strings for one check.
func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
