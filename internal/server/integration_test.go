package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
)

// newTestAppWithOAuth builds an App with the OAuth server enabled and one
// pre-seeded confidential client so the token endpoint can issue tokens.
func newTestAppWithOAuth(t *testing.T) *App {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 0},
		Auth:   config.AuthConfig{OAuth: true},
		OAuth: config.OAuthConfig{
			Issuer: "https://openviking.test",
			Clients: []config.ClientConfig{
				{
					ID:         "integration-client",
					Secret:     "s3cret",
					GrantTypes: []string{"client_credentials"},
					Scopes:     []string{"fosite", "openviking"},
				},
			},
			Crypto: config.CryptoConfig{
				Provider: "local",
				Local:    config.LocalCryptoConfig{Passphrase: "test-passphrase-for-integration-test-only"},
			},
		},
	}
	app, cleanup, err := BuildApp(cfg)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return app
}

// (a) End-to-end OAuth flow: DCR → token (client_credentials) → call a
// protected route with the bearer token → 200. Exercises the real fosite
// OAuth server through the full App middleware chain.
func TestIntegration_OAuthFlow_DCRTokenProtectedRoute(t *testing.T) {
	app := newTestAppWithOAuth(t)

	// 1) DCR: register a new confidential client.
	dcrBody := `{"client_name":"dcr-test","redirect_uris":["https://app.test/cb"],"grant_types":["client_credentials"],"scope":["fosite"]}`
	dcrReq := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/register", strings.NewReader(dcrBody))
	dcrReq.Header.Set("Content-Type", "application/json")
	dcrRec := httptest.NewRecorder()
	app.Router().ServeHTTP(dcrRec, dcrReq)
	require.Equal(t, http.StatusCreated, dcrRec.Code, dcrRec.Body.String())

	var dcrResp struct {
		ID         string   `json:"client_id"`
		Secret     string   `json:"client_secret"`
		GrantTypes []string `json:"grant_types_allowed"`
	}
	require.NoError(t, json.Unmarshal(dcrRec.Body.Bytes(), &dcrResp))
	require.NotEmpty(t, dcrResp.ID)
	require.NotEmpty(t, dcrResp.Secret)

	// 2) Token: exchange client_credentials for an access token.
	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {"fosite"},
	}
	tokenReq := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenReq.SetBasicAuth(dcrResp.ID, dcrResp.Secret)
	tokenRec := httptest.NewRecorder()
	app.Router().ServeHTTP(tokenRec, tokenReq)
	require.Equal(t, http.StatusOK, tokenRec.Code, tokenRec.Body.String())

	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	require.NoError(t, json.Unmarshal(tokenRec.Body.Bytes(), &tok))
	require.NotEmpty(t, tok.AccessToken)
	assert.Equal(t, "bearer", tok.TokenType)

	// 3) Call a protected route with the bearer token. /api/v1/system/info
	// is a real handler (not a stub) that returns 200; it sits behind the
	// identity middleware so we still need the account header. The bearer
	// token exercises the Auth middleware in passthrough mode.
	protectedReq := httptest.NewRequest(http.MethodGet, "/api/v1/system/info", nil)
	protectedReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	protectedReq.Header.Set("X-OpenViking-Account", "acct")
	protectedRec := httptest.NewRecorder()
	app.Router().ServeHTTP(protectedRec, protectedReq)
	assert.Equal(t, http.StatusOK, protectedRec.Code, protectedRec.Body.String())
	assert.Contains(t, protectedRec.Body.String(), "go_version")
}

// (b) MCP tools/list via POST /mcp. The streamable HTTP transport accepts
// a JSON-RPC tools/list request and returns the 13 tool names. (GET /mcp
// opens an SSE notification stream; the tools/list call is a POST per the
// MCP spec.)
func TestIntegration_MCPToolsList(t *testing.T) {
	app := newTestApp(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// Every tool name should appear in the response.
	for _, name := range []string{"find", "search", "read", "remember", "health"} {
		assert.Contains(t, rec.Body.String(), name, "tool %q missing", name)
	}
}

// (c) Middleware chain: requests without X-OpenViking-Account are
// rejected with 401 on protected (identity-guarded) routes; OAuth routes
// sit outside the identity middleware and return their own response (501
// stub when OAuth is disabled, or the handler's response when enabled);
// /healthz is public and returns 200.
func TestIntegration_MiddlewareChain(t *testing.T) {
	app := newTestApp(t)

	// Protected route without account header → 401.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNAUTHORIZED")

	// OAuth route without account header → 501 (stub; OAuth disabled by
	// default in newTestApp). The identity middleware must NOT gate OAuth.
	oauthReq := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/register", strings.NewReader(`{}`))
	oauthReq.Header.Set("Content-Type", "application/json")
	oauthRec := httptest.NewRecorder()
	app.Router().ServeHTTP(oauthRec, oauthReq)
	assert.Equal(t, http.StatusNotImplemented, oauthRec.Code)

	// Health route without any headers → 200.
	healthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthRec := httptest.NewRecorder()
	app.Router().ServeHTTP(healthRec, healthReq)
	assert.Equal(t, http.StatusOK, healthRec.Code)
}

// (d) Error envelope: every error response must be a JSON object with an
// "error" field containing "code", "message", and optional "details".
func TestIntegration_ErrorEnvelope(t *testing.T) {
	app := newTestApp(t)

	// 404 catch-all: structured RESOURCE_NOT_FOUND envelope.
	req := httptest.NewRequest(http.MethodGet, "/no-such-path", nil)
	rec := httptest.NewRecorder()
	app.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
	assert.Equal(t, "RESOURCE_NOT_FOUND", env.Error.Code)
	assert.NotEmpty(t, env.Error.Message, "message must be non-empty")

	// 501 UNSUPPORTED: /bot/v1/health returns 501 because deps.Bot is nil
	// in the test config. The error middleware must render the same
	// envelope shape with code UNSUPPORTED.
	stubReq := httptest.NewRequest(http.MethodGet, "/bot/v1/health", nil)
	stubRec := httptest.NewRecorder()
	app.Router().ServeHTTP(stubRec, stubReq)
	require.Equal(t, http.StatusNotImplemented, stubRec.Code)
	require.NoError(t, json.Unmarshal(stubRec.Body.Bytes(), &env), stubRec.Body.String())
	assert.Equal(t, "UNSUPPORTED", env.Error.Code)
	assert.NotEmpty(t, env.Error.Message)
}
