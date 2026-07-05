package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/crypto"
	"github.com/saker-ai/ctxhub/internal/observability"
)

// newTestServerWithStore returns a Server wired with a real token
// store, a mock feishu provider, and an isolated Prometheus registry
// for metrics assertions.
func newTestServerWithStore(t *testing.T) (*Server, *prometheus.Registry, *Store, *observability.Metrics) {
	t.Helper()
	cfg := &config.OAuthConfig{
		Issuer:  testExternalURL,
		Clients: nil,
		Providers: []config.OAuthProviderConfig{
			{
				Name:         ProviderFeishu,
				ClientID:     "feishu-cid",
				ClientSecret: "feishu-cs",
				RedirectURIs: []string{"https://app.test/cb"},
			},
			{
				Name:         ProviderGoogle,
				ClientID:     "google-cid",
				ClientSecret: "google-cs",
				RedirectURIs: []string{"https://app.test/cb"},
			},
		},
	}
	srv, err := New(cfg, testBasePath, testExternalURL)
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetricsFor(reg)
	srv.SetMetrics(metrics)

	enc, err := crypto.New(crypto.Config{
		Provider: "local",
		Local: crypto.LocalConfig{
			Passphrase: "test-passphrase-do-not-use-in-prod",
		},
	})
	require.NoError(t, err)
	store, err := NewStore(StoreConfig{
		DSN:       ":memory:",
		Encryptor: enc,
		Metrics:   metrics,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	srv.SetTokenStore(store)

	// Replace the production feishu provider with a mock that points
	// at an httptest server.
	mockSrv := newMockFeishuServer(t)
	srv.providers[ProviderFeishu] = newFeishuProvider(ProviderConfig{
		Name:         ProviderFeishu,
		ClientID:     "feishu-cid",
		ClientSecret: "feishu-cs",
		AuthURL:      mockSrv.URL + "/auth",
		TokenURL:     mockSrv.URL + "/token",
		UserInfoURL:  mockSrv.URL + "/userinfo",
	}, mockSrv.Client())

	return srv, reg, store, metrics
}

// newMockFeishuServer returns an httptest.Server that handles feishu's
// /token (exchange + refresh) + /userinfo endpoints.
func newMockFeishuServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gt := r.FormValue("grant_type")
		switch gt {
		case "authorization_code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"access_token":  "at-exchanged",
					"refresh_token": "rt-exchanged",
					"token_type":    "Bearer",
					"expires_in":    7200,
				},
			})
		case "refresh_token":
			rt := r.FormValue("refresh_token")
			if strings.Contains(rt, "invalid") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": 40013,
					"msg":  "invalid refresh_token",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"access_token":  "at-refreshed",
					"refresh_token": "rt-refreshed",
					"token_type":    "Bearer",
					"expires_in":    7200,
				},
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"msg":  "ok",
			"data": map[string]any{
				"open_id": "ou_test_1",
				"name":    "Test User",
				"email":   "test@example.com",
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// counterValue extracts the current value of a labeled counter from
// the registry. Returns 0.0 when the metric or label set is missing.
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := true
			for k, v := range labels {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0.0
}

func TestAuthorize_RedirectsToProvider(t *testing.T) {
	srv, _, _, _ := newTestServerWithStore(t)
	r := newTestRouter(srv)

	rec := doRequest(t, r, http.MethodGet, "/api/v1/oauth/authorize?provider=feishu&redirect_uri=https://app.test/cb&state=st1", "", nil)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	loc := rec.Header().Get("Location")
	require.NotEmpty(t, loc)
	assert.Contains(t, loc, "state=")
	// Feishu uses app_id; google/slack/dingtalk use client_id. Either
	// is acceptable — assert the value is present under one of them.
	assert.True(t, containsParam(loc, "app_id", "feishu-cid") || containsParam(loc, "client_id", "feishu-cid"),
		"auth URL must include app_id or client_id=feishu-cid: %s", loc)
}

func TestCallback_ExchangeAndPersist(t *testing.T) {
	srv, _, store, _ := newTestServerWithStore(t)
	r := newTestRouter(srv)

	// 1. Authorize to get a state.
	rec := doRequest(t, r, http.MethodGet, "/api/v1/oauth/authorize?provider=feishu&redirect_uri=https://app.test/cb&state=st1", "", nil)
	require.Equal(t, http.StatusFound, rec.Code)
	loc := rec.Header().Get("Location")
	parsed, err := url.Parse(loc)
	require.NoError(t, err)
	state := parsed.Query().Get("state")
	require.NotEmpty(t, state)

	// 2. Callback with code + state. Should exchange + persist.
	cb := "/api/v1/oauth/callback?provider=feishu&code=code-1&state=" + state
	rec = doRequest(t, r, http.MethodGet, cb, "", nil)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())

	// 3. Verify the token was persisted.
	stored, err := store.LoadTokenByRefresh(context.Background(), ProviderFeishu, "rt-exchanged")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "at-exchanged", stored.AccessToken)
	assert.Equal(t, "rt-exchanged", stored.RefreshToken)
	assert.Equal(t, "ou_test_1", stored.UserID)
}

func TestCallback_InvalidState(t *testing.T) {
	srv, _, _, _ := newTestServerWithStore(t)
	r := newTestRouter(srv)

	rec := doRequest(t, r, http.MethodGet, "/api/v1/oauth/callback?provider=feishu&code=c&state=unknown", "", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "state")
}

func TestCallback_StateProviderMismatch(t *testing.T) {
	srv, _, store, _ := newTestServerWithStore(t)
	r := newTestRouter(srv)

	// Save a state for feishu.
	require.NoError(t, store.SaveState(context.Background(), "st-mismatch", ProviderFeishu, "", "", "https://app.test/cb", "", 10*time.Minute))

	rec := doRequest(t, r, http.MethodGet, "/api/v1/oauth/callback?provider=google&code=c&state=st-mismatch", "", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "mismatch")
}

func TestTokenHandler_RefreshGrant_OK(t *testing.T) {
	srv, reg, store, _ := newTestServerWithStore(t)
	r := newTestRouter(srv)

	// Seed a token directly into the store.
	tok := &Token{
		AccessToken:  "at-old",
		RefreshToken: "rt-old",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		UserID:       "ou_test_1",
		Subject:      "Test User",
	}
	require.NoError(t, store.SaveToken(context.Background(), ProviderFeishu, "acct-1", tok))

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"provider":      {ProviderFeishu},
		"refresh_token": {"rt-old"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp TokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "at-refreshed", resp.AccessToken)
	assert.Equal(t, "rt-refreshed", resp.RefreshToken)
	assert.Equal(t, ProviderFeishu, resp.Provider)

	// The old refresh token must be invalid after rotation.
	stored, err := store.LoadTokenByRefresh(context.Background(), ProviderFeishu, "rt-old")
	require.NoError(t, err)
	assert.Nil(t, stored, "old refresh token must not load after rotation")

	// The new refresh token should be the current one.
	storedNew, err := store.LoadTokenByRefresh(context.Background(), ProviderFeishu, "rt-refreshed")
	require.NoError(t, err)
	require.NotNil(t, storedNew)
	assert.Equal(t, "at-refreshed", storedNew.AccessToken)

	// Metric: oauthTokenRefreshTotal{provider=feishu,status=ok} == 1.
	got := counterValue(t, reg, "openviking_oauth_token_refresh_total", map[string]string{
		"provider": ProviderFeishu,
		"status":   "ok",
	})
	assert.Equal(t, 1.0, got)
}

func TestTokenHandler_RefreshGrant_PermanentError(t *testing.T) {
	srv, reg, store, _ := newTestServerWithStore(t)
	r := newTestRouter(srv)

	// Seed a token whose refresh value triggers the mock's
	// "invalid refresh_token" branch.
	tok := &Token{
		AccessToken:  "at-old",
		RefreshToken: "rt-invalid",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		UserID:       "ou_test_1",
		Subject:      "Test User",
	}
	require.NoError(t, store.SaveToken(context.Background(), ProviderFeishu, "acct-1", tok))

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"provider":      {ProviderFeishu},
		"refresh_token": {"rt-invalid"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Metric: oauthTokenRefreshTotal{provider=feishu,status=permanent_error} == 1.
	got := counterValue(t, reg, "openviking_oauth_token_refresh_total", map[string]string{
		"provider": ProviderFeishu,
		"status":   "permanent_error",
	})
	assert.Equal(t, 1.0, got)

	// Permanent error deletes the row so the user must re-authorize.
	stored, err := store.LoadTokenByRefresh(context.Background(), ProviderFeishu, "rt-invalid")
	require.NoError(t, err)
	assert.Nil(t, stored)
}

func TestTokenHandler_RefreshGrant_ReplayDetected(t *testing.T) {
	srv, reg, store, _ := newTestServerWithStore(t)
	r := newTestRouter(srv)

	// Seed + rotate.
	tok1 := &Token{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		UserID:       "ou_test_1",
		Subject:      "Test User",
	}
	require.NoError(t, store.SaveToken(context.Background(), ProviderFeishu, "acct-1", tok1))
	tok2 := &Token{
		AccessToken:  "at-2",
		RefreshToken: "rt-2",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		UserID:       "ou_test_1",
		Subject:      "Test User",
	}
	require.NoError(t, store.SaveToken(context.Background(), ProviderFeishu, "acct-1", tok2))

	// Now attempt to refresh with the OLD rt-1. Should be flagged as
	// replay and rejected with permanent_error.
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"provider":      {ProviderFeishu},
		"refresh_token": {"rt-1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "replay")

	got := counterValue(t, reg, "openviking_oauth_token_refresh_total", map[string]string{
		"provider": ProviderFeishu,
		"status":   "permanent_error",
	})
	assert.Equal(t, 1.0, got)
}

func TestTokenHandler_RefreshGrant_MissingFields(t *testing.T) {
	srv, reg, _, _ := newTestServerWithStore(t)
	r := newTestRouter(srv)

	form := url.Values{"grant_type": {"refresh_token"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	got := counterValue(t, reg, "openviking_oauth_token_refresh_total", map[string]string{
		"provider": "",
		"status":   "error",
	})
	assert.Equal(t, 1.0, got)
}

func TestTokenHandler_ClientCredentialsStillWorks(t *testing.T) {
	// Ensure the new refresh_token dispatch doesn't break the existing
	// client_credentials grant.
	srv := newTestServer(t, config.ClientConfig{
		ID:         "cc-client",
		Secret:     "foobar",
		GrantTypes: []string{"client_credentials"},
		Scopes:     []string{"fosite"},
	})
	r := newTestRouter(srv)

	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {"fosite"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cc-client", "foobar")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// stub unused-import guard: ensure gin is referenced.
var _ = gin.TestMode
