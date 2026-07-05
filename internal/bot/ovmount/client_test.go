package ovmount

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

func TestHTTPClient_Health(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/system" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(config.OvmountConfig{}, srv.URL)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestHTTPClient_HealthHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(config.OvmountConfig{}, srv.URL)
	if err := c.Health(context.Background()); err == nil {
		t.Fatalf("Health should error")
	}
}

func TestHTTPClient_SetsAuthAndIdentity(t *testing.T) {
	var gotAuth, gotAcct, gotUser, gotPeer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAcct = r.Header.Get("X-OpenViking-Account")
		gotUser = r.Header.Get("X-OpenViking-User")
		gotPeer = r.Header.Get("X-OpenViking-Actor-Peer")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	c := New(config.OvmountConfig{APIKey: "tok"}, srv.URL)
	_, _ = c.List(context.Background(), domain.Identifier{Account: "acct", User: "u", ActorPeer: "p"}, "viking://acct")
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotAcct != "acct" || gotUser != "u" || gotPeer != "p" {
		t.Errorf("identity headers = %q/%q/%q", gotAcct, gotUser, gotPeer)
	}
}

func TestHTTPClient_List(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/resources") {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]domain.Resource{
			{URI: "viking://acct/foo", Name: "foo", Type: domain.ResourceTypeFile},
			{URI: "viking://acct/bar", Name: "bar", Type: domain.ResourceTypeDir},
		})
	}))
	defer srv.Close()
	c := New(config.OvmountConfig{}, srv.URL)
	out, err := c.List(context.Background(), domain.Identifier{Account: "acct"}, "viking://acct")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("len = %d, want 2", len(out))
	}
	if out[0].Name != "foo" {
		t.Errorf("name = %q", out[0].Name)
	}
}

func TestHTTPClient_Remember(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := New(config.OvmountConfig{}, srv.URL)
	if err := c.Remember(context.Background(), domain.Identifier{Account: "a"}, "remembered fact", nil); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if !strings.Contains(gotBody, "remembered fact") {
		t.Errorf("body = %q", gotBody)
	}
}
