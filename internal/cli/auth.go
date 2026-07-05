package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// TokenCache is the on-disk JSON shape persisted at ~/.ov/credentials.json.
// It mirrors the OAuth 2.0 token response so callers can reuse an
// access_token until it expires.
type TokenCache struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Scope        string    `json:"scope,omitempty"`
}

// IsExpired reports whether the cached token is missing or past its
// expiry (with a 10s safety margin to avoid 401s on the wire).
func (t *TokenCache) IsExpired() bool {
	if t == nil || t.AccessToken == "" {
		return true
	}
	if t.Expiry.IsZero() {
		return false
	}
	return time.Until(t.Expiry) <= 10*time.Second
}

// TokenSource acquires and caches an OAuth bearer token via the
// client_credentials grant. It is used by `ov` to authenticate against
// OpenViking's /api/v1/oauth/token endpoint.
type TokenSource struct {
	cfg        CLIConfigAuth
	baseURL    string
	cachePath  string
	clock      func() time.Time
	httpClient *http.Client // optional override; defaults to http.DefaultClient
}

// TokenSourceOption configures a TokenSource at construction time.
type TokenSourceOption func(*TokenSource)

// WithTokenCachePath overrides the credentials cache location.
func WithTokenCachePath(p string) TokenSourceOption {
	return func(ts *TokenSource) { ts.cachePath = p }
}

// WithHTTPClientOverride replaces the underlying http.Client used by
// the OAuth flow. Tests use this to inject an httptest.Server transport.
func WithHTTPClientOverride(hc *http.Client) TokenSourceOption {
	return func(ts *TokenSource) { ts.httpClient = hc }
}

// WithTokenClock replaces the clock used for expiry checks. Tests use
// this to deterministically drive token refresh.
func WithTokenClock(f func() time.Time) TokenSourceOption {
	return func(ts *TokenSource) { ts.clock = f }
}

// NewTokenSource builds a TokenSource for the given auth config.
// baseURL is the OpenViking server URL used to compute the token
// endpoint when Auth.TokenURL is empty.
func NewTokenSource(baseURL string, cfg CLIConfigAuth, opts ...TokenSourceOption) *TokenSource {
	ts := &TokenSource{
		cfg:        cfg,
		baseURL:    strings.TrimRight(baseURL, "/"),
		clock:      time.Now,
		httpClient: http.DefaultClient,
	}
	if p, err := CredentialsPath(); err == nil {
		ts.cachePath = p
	}
	for _, opt := range opts {
		opt(ts)
	}
	return ts
}

// TokenURL returns the absolute token endpoint URL.
func (ts *TokenSource) TokenURL() string {
	if ts.cfg.TokenURL != "" {
		return ts.cfg.TokenURL
	}
	return ts.baseURL + "/api/v1/oauth/token"
}

// Token returns a non-expired access token. It checks the cache first
// and only contacts the server when the cache is stale. The result is
// cached on success.
func (ts *TokenSource) Token(ctx context.Context) (string, error) {
	if ts.cfg.ClientID == "" || ts.cfg.ClientSecret == "" {
		return "", errors.New("cli: auth.client_id / auth.client_secret not set; run `ov init`")
	}
	cached, _ := ts.loadCache()
	if cached != nil && !cached.IsExpired() {
		return cached.AccessToken, nil
	}
	tok, err := ts.fetchToken(ctx)
	if err != nil {
		return "", err
	}
	_ = ts.saveCache(tok)
	return tok.AccessToken, nil
}

// fetchToken uses golang.org/x/oauth2/clientcredentials to perform the
// grant. Using the canonical SDK keeps us aligned with RFC 6749 §4.4
// and gives us automatic Basic auth + Content-Type handling.
func (ts *TokenSource) fetchToken(ctx context.Context) (*TokenCache, error) {
	cfg := &clientcredentials.Config{
		ClientID:     ts.cfg.ClientID,
		ClientSecret: ts.cfg.ClientSecret,
		TokenURL:     ts.TokenURL(),
		Scopes:       ts.cfg.Scopes,
		AuthStyle:    oauth2.AuthStyleInHeader,
	}
	httpCtx := context.Background()
	if ts.httpClient != nil && ts.httpClient != http.DefaultClient {
		httpCtx = context.WithValue(httpCtx, oauth2.HTTPClient, ts.httpClient)
	}
	tok, err := cfg.Token(httpCtx)
	if err != nil {
		// Surface a friendlier error for the common 401/invalid_client case.
		var reqErr *oauth2.RetrieveError
		if errors.As(err, &reqErr) {
			return nil, fmt.Errorf("cli: oauth token request failed (status %d): %s",
				reqErr.Response.StatusCode, strings.TrimSpace(string(reqErr.Body)))
		}
		return nil, fmt.Errorf("cli: oauth token: %w", err)
	}
	tc := &TokenCache{
		AccessToken:  tok.AccessToken,
		TokenType:    tok.TokenType,
		Expiry:       tok.Expiry,
		RefreshToken: tok.RefreshToken,
	}
	if v := tok.Extra("scope"); v != nil {
		switch s := v.(type) {
		case string:
			tc.Scope = s
		default:
			tc.Scope = fmt.Sprintf("%v", v)
		}
	}
	if ts.clock != nil && !tc.Expiry.IsZero() {
		// Recompute expiry from the configured clock for deterministic tests.
		tc.Expiry = ts.clock().Add(time.Until(tok.Expiry))
	}
	return tc, nil
}

// InvalidateCache removes the on-disk credentials cache, forcing the
// next Token call to refetch. Used by `ov auth logout` (if added).
func (ts *TokenSource) InvalidateCache() error {
	if ts.cachePath == "" {
		return nil
	}
	if err := os.Remove(ts.cachePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (ts *TokenSource) loadCache() (*TokenCache, error) {
	if ts.cachePath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(ts.cachePath)
	if err != nil {
		return nil, err
	}
	var tc TokenCache
	if err := json.Unmarshal(data, &tc); err != nil {
		return nil, err
	}
	return &tc, nil
}

func (ts *TokenSource) saveCache(tc *TokenCache) error {
	if ts.cachePath == "" {
		return nil
	}
	buf, err := json.MarshalIndent(tc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ts.cachePath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(ts.cachePath, buf, 0o600); err != nil {
		return err
	}
	return nil
}

// RawTokenResponse is the JSON shape returned by POST /api/v1/oauth/token.
// Exposed so tests and callers can decode the response without oauth2.
type RawTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// FetchTokenRaw performs the client_credentials grant using plain HTTP,
// without the oauth2 SDK. It exists to support deployments that use
// non-standard auth (e.g. mTLS) and as a fallback when the SDK can't
// reach the server. Tests use it to assert request shape.
func FetchTokenRaw(ctx context.Context, client *http.Client, tokenURL, clientID, clientSecret string, scopes []string) (*TokenCache, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	hc := client
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cli: oauth token request failed (status %d): %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var raw RawTokenResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("cli: decode token response: %w", err)
	}
	expiry := time.Time{}
	if raw.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)
	}
	return &TokenCache{
		AccessToken:  raw.AccessToken,
		TokenType:    raw.TokenType,
		Expiry:       expiry,
		RefreshToken: raw.RefreshToken,
		Scope:        raw.Scope,
	}, nil
}
