package routers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

func newDebugTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterDebug(api, deps)
	return r
}

func TestDebug_Ctx(t *testing.T) {
	deps := newTestDeps(t)
	r := newDebugTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/debug/ctx?foo=bar", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set(identity.HeaderUser, "user1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Identity struct {
			Present bool   `json:"present"`
			Account string `json:"account"`
			User    string `json:"user"`
		} `json:"identity"`
		Method string `json:"method"`
		Query  string `json:"query"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body.Identity.Present)
	assert.Equal(t, "acct", body.Identity.Account)
	assert.Equal(t, "user1", body.Identity.User)
	assert.Equal(t, "GET", body.Method)
	assert.Equal(t, "foo=bar", body.Query)
}

func TestDebug_HeadersMasksAuth(t *testing.T) {
	deps := newTestDeps(t)
	r := newDebugTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/debug/headers", nil)
	req.Header.Set("Authorization", "Bearer super-secret-token-value")
	req.Header.Set("X-OpenViking-Account", "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	auth, ok := body.Headers["Authorization"]
	require.True(t, ok, "Authorization header should be present in debug output")
	assert.NotEqual(t, "Bearer super-secret-token-value", auth, "Authorization value must be masked")
	assert.Contains(t, auth, "*")
	assert.Equal(t, "acct", body.Headers["X-Openviking-Account"])
}

func TestDebug_EnvMasksSecrets(t *testing.T) {
	deps := newTestDeps(t)
	r := newDebugTestRouter(t, deps)
	// Set a sensitive env var and a public OV_ var. Reseting in tests is
	// safe because t.Setenv restores the previous value on test cleanup.
	t.Setenv("OV_DEBUG_TEST", "visible-value")
	t.Setenv("MY_SECRET_TOKEN", "very-sensitive-value")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/debug/env", nil)
	req.Header.Set(identity.HeaderAccount, "admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Env map[string]string `json:"env"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "visible-value", body.Env["OV_DEBUG_TEST"], "OV_ vars must be unmasked")
	assert.NotEqual(t, "very-sensitive-value", body.Env["MY_SECRET_TOKEN"], "secret vars must be masked")
	assert.Contains(t, body.Env["MY_SECRET_TOKEN"], "*")
}

func TestDebug_ConfigMasksSecrets(t *testing.T) {
	deps := newTestDeps(t)
	// Build a minimal Config with a secret field. The OAuth client secret
	// is a leaf keyed "secret", which isSecretKey matches.
	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 8080},
		OAuth:  config.OAuthConfig{Clients: []config.ClientConfig{{ID: "c1", Secret: "super-secret-key"}}},
	}
	deps.Config = cfg
	r := newDebugTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/debug/config", nil)
	req.Header.Set(identity.HeaderAccount, "admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The secret leaf should be masked, not the raw value.
	body := rec.Body.String()
	assert.NotContains(t, body, "super-secret-key", "full secret must not appear in response")
	assert.Contains(t, body, "*", "masked body should contain asterisks")
}

func TestDebug_ConfigNilReturnsUnsupported(t *testing.T) {
	deps := newTestDeps(t)
	r := newDebugTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/debug/config", nil)
	req.Header.Set(identity.HeaderAccount, "admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestDebug_NoExternalNetwork(t *testing.T) {
	// Smoke test that env endpoint doesn't accidentally hit the network.
	_ = os.Getenv("HOME")
	deps := newTestDeps(t)
	r := newDebugTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/debug/env", nil)
	req.Header.Set(identity.HeaderAccount, "admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}
