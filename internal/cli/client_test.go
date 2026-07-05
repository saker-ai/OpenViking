package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// stubDoer captures the last request and returns a canned response.
type stubDoer struct {
	lastReq *http.Request
	resp    *http.Response
	err     error
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func newResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestClientHeaders(t *testing.T) {
	stub := &stubDoer{resp: newResp(http.StatusOK, `{}`)}
	c := NewClient("https://api.example.com",
		WithHTTPClient(stub),
		WithToken("tok-123"),
		WithAccount("acct"),
		WithUser("alice"),
		WithUserAgent("ov-test"),
	)
	resp, err := c.Do(context.Background(), http.MethodGet, "/api/v1/resources", nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	h := stub.lastReq.Header
	assert.Equal(t, "acct", h.Get("X-OpenViking-Account"))
	assert.Equal(t, "alice", h.Get("X-OpenViking-User"))
	assert.Equal(t, "Bearer tok-123", h.Get("Authorization"))
	assert.Equal(t, "ov-test", h.Get("User-Agent"))
	assert.NotEmpty(t, h.Get("X-Request-ID"))
}

func TestClientAppError(t *testing.T) {
	body := `{"error":{"code":"RESOURCE_NOT_FOUND","message":"missing","details":{"uri":"x"}}}`
	stub := &stubDoer{resp: newResp(http.StatusNotFound, body)}
	c := NewClient("https://api.example.com", WithHTTPClient(stub))
	_, err := c.Do(context.Background(), http.MethodGet, "/api/v1/resources/foo", nil)
	require.Error(t, err)
	var ae *domain.AppError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, domain.CodeResourceNotFound, ae.Code)
	assert.Equal(t, http.StatusNotFound, ae.Status)
	assert.Equal(t, "missing", ae.Err.Error())
	assert.Equal(t, "x", ae.Details["uri"])
}

func TestClientNonJSONError(t *testing.T) {
	stub := &stubDoer{resp: newResp(http.StatusBadGateway, `bad gateway`)}
	c := NewClient("https://api.example.com", WithHTTPClient(stub))
	_, err := c.Do(context.Background(), http.MethodGet, "/x", nil)
	require.Error(t, err)
	var ae *domain.AppError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, http.StatusBadGateway, ae.Status)
}

func TestClientGetJSON(t *testing.T) {
	stub := &stubDoer{resp: newResp(http.StatusOK, `{"uri":"a","type":"file"}`)}
	c := NewClient("https://api.example.com", WithHTTPClient(stub))
	var out map[string]any
	require.NoError(t, c.GetJSON(context.Background(), "/api/v1/resources/a", &out))
	assert.Equal(t, "a", out["uri"])
}

func TestClientPostJSON(t *testing.T) {
	stub := &stubDoer{resp: newResp(http.StatusCreated, `{"id":"1"}`)}
	c := NewClient("https://api.example.com", WithHTTPClient(stub))
	var out map[string]any
	require.NoError(t, c.PostJSON(context.Background(), "/api/v1/resources", map[string]any{"path": "/x"}, &out))
	assert.Equal(t, "1", out["id"])
	assert.Equal(t, "application/json", stub.lastReq.Header.Get("Content-Type"))
}

func TestClientBadPath(t *testing.T) {
	c := NewClient("https://api.example.com")
	_, err := c.Do(context.Background(), http.MethodGet, "no-slash", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must start with /")
}

// --- Auth / TokenSource tests ---

func TestTokenCacheIsExpired(t *testing.T) {
	var tc *TokenCache
	assert.True(t, tc.IsExpired())
	tc = &TokenCache{AccessToken: "x"}
	assert.False(t, tc.IsExpired()) // no expiry = never expires
	tc.Expiry = time.Now().Add(-time.Hour)
	assert.True(t, tc.IsExpired())
	tc.Expiry = time.Now().Add(2 * time.Minute)
	assert.False(t, tc.IsExpired())
}

func TestTokenSourceMissingCreds(t *testing.T) {
	ts := NewTokenSource("https://api.example.com", CLIConfigAuth{}, WithTokenCachePath(t.TempDir()+"/c.json"))
	_, err := ts.Token(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "client_id")
}

func TestTokenSourceUsesCache(t *testing.T) {
	dir := t.TempDir()
	cachePath := dir + "/c.json"
	tc := &TokenCache{AccessToken: "cached-tok", Expiry: time.Now().Add(time.Hour)}
	buf, _ := json.Marshal(tc)
	require.NoError(t, os.WriteFile(cachePath, buf, 0o600))

	ts := NewTokenSource("https://api.example.com",
		CLIConfigAuth{ClientID: "id", ClientSecret: "sec"},
		WithTokenCachePath(cachePath))
	tok, err := ts.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "cached-tok", tok)
}

func TestTokenSourceFetchesWhenCacheExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/oauth/token", r.URL.Path)
		require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		// fosite expects HTTP Basic auth for confidential clients.
		user, pass, ok := r.BasicAuth()
		require.True(t, ok, "Basic auth header expected")
		assert.Equal(t, "id", user)
		assert.Equal(t, "sec", pass)
		_ = r.ParseForm()
		assert.Equal(t, "client_credentials", r.PostForm.Get("grant_type"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-tok","token_type":"bearer","expires_in":3600,"scope":"fosite"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cachePath := dir + "/c.json"
	ts := NewTokenSource(srv.URL,
		CLIConfigAuth{ClientID: "id", ClientSecret: "sec"},
		WithTokenCachePath(cachePath))
	tok, err := ts.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "fresh-tok", tok)

	// Token should be cached on disk.
	buf, err := os.ReadFile(cachePath)
	require.NoError(t, err)
	var got TokenCache
	require.NoError(t, json.Unmarshal(buf, &got))
	assert.Equal(t, "fresh-tok", got.AccessToken)
}

func TestFetchTokenRawError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()
	_, err := FetchTokenRaw(t.Context(), srv.Client(), srv.URL+"/token", "id", "bad", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 401")
}
