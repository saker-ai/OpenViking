package routers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/server/identity"
)

func newUserSettingsTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterUserSettings(api, deps)
	return r
}

func TestUserSettings_GetEmpty(t *testing.T) {
	deps := newTestDeps(t)
	r := newUserSettingsTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user-settings", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Path     string         `json:"path"`
		Settings map[string]any `json:"settings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "/accounts/acct/settings.json", body.Path)
	assert.Empty(t, body.Settings)
}

func TestUserSettings_UpdateRoot(t *testing.T) {
	deps := newTestDeps(t)
	r := newUserSettingsTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/user-settings",
		bytes.NewBufferString(`{"settings":{"theme":"dark","lang":"en"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Read back.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user-settings", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Settings map[string]any `json:"settings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "dark", got.Settings["theme"])
	assert.Equal(t, "en", got.Settings["lang"])
}

func TestUserSettings_NamespaceRoundTrip(t *testing.T) {
	deps := newTestDeps(t)
	r := newUserSettingsTestRouter(t, deps)

	// get namespace -> empty
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user-settings/ui", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got struct {
		Namespace string         `json:"namespace"`
		Settings  map[string]any `json:"settings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "ui", got.Namespace)
	assert.Empty(t, got.Settings)

	// put namespace
	req = httptest.NewRequest(http.MethodPut, "/api/v1/user-settings/ui",
		bytes.NewBufferString(`{"settings":{"sidebar":"collapsed"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// get namespace back
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user-settings/ui", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "collapsed", got.Settings["sidebar"])
}

func TestUserSettings_NilDepsReturnsError(t *testing.T) {
	r := newUserSettingsTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user-settings", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestUserSettings_BadJSON(t *testing.T) {
	r := newUserSettingsTestRouter(t, newTestDeps(t))
	req := httptest.NewRequest(http.MethodPut, "/api/v1/user-settings",
		bytes.NewBufferString(`{not-json`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}
