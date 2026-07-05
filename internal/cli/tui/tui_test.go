package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/fs/ls", r.URL.Path)
		require.Equal(t, "/x", r.URL.Query().Get("path"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"uri":"a","type":"file","name":"a","size":10,"modified_at":"2024-01-01T00:00:00Z"}]}`))
	}))
	defer srv.Close()
	c := newHTTPClientShim(srv.URL, "test")
	items, err := fetchItems(context.Background(), c, "/x")
	require.NoError(t, err)
	require.Len(t, items, 1)
	it := items[0].(item)
	assert.Equal(t, "a", it.res.URI)
}

func TestFetchItemsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"RESOURCE_NOT_FOUND","message":"missing"}}`))
	}))
	defer srv.Close()
	c := newHTTPClientShim(srv.URL, "test")
	_, err := fetchItems(context.Background(), c, "/missing")
	require.Error(t, err)
}

func TestNewCmdRequiresResolver(t *testing.T) {
	cmd := NewCmd(nil, nil)
	err := cmd.RunE(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolver")
}

func TestNewCmdNilClient(t *testing.T) {
	resolver := func(_ *cobra.Command) (Client, error) { return nil, nil }
	cmd := NewCmd(resolver, nil)
	err := cmd.RunE(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "client is required")
}

// newHTTPClientShim builds a Client for tests without importing the cli
// package. The shim mirrors the request shape *cli.Client sends
// (GET + JSON decode) but is implemented locally.
func newHTTPClientShim(baseURL, account string) Client {
	return &shimClient{baseURL: baseURL, account: account, hc: http.DefaultClient}
}

type shimClient struct {
	baseURL string
	account string
	hc      *http.Client
}

func (s *shimClient) GetJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return err
	}
	if s.account != "" {
		req.Header.Set("X-OpenViking-Account", s.account)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *shimClient) Raw(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return s.hc.Do(req)
}

func (s *shimClient) BaseURL() string { return s.baseURL }
