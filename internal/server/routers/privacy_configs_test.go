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

func newPrivacyTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterPrivacyConfigs(api, deps)
	return r
}

func TestPrivacy_ListEmpty(t *testing.T) {
	deps := newTestDeps(t)
	r := newPrivacyTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/privacy-configs", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Configs []string `json:"configs"`
		Path    string   `json:"path"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "/accounts/acct/privacy_configs", body.Path)
	assert.Empty(t, body.Configs)
}

func TestPrivacy_CreateAndGet(t *testing.T) {
	deps := newTestDeps(t)
	r := newPrivacyTestRouter(t, deps)

	body := `{"id":"default","config":{"redact":true,"fields":["ssn"]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/privacy-configs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	req = httptest.NewRequest(http.MethodGet, "/api/v1/privacy-configs/default", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got struct {
		ID     string                 `json:"id"`
		Config map[string]interface{} `json:"config"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "default", got.ID)
	assert.Equal(t, true, got.Config["redact"])
}

func TestPrivacy_CreateConflict(t *testing.T) {
	deps := newTestDeps(t)
	r := newPrivacyTestRouter(t, deps)

	body := `{"id":"dup","config":{"k":1}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/privacy-configs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/privacy-configs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestPrivacy_UpdateAndDelete(t *testing.T) {
	deps := newTestDeps(t)
	r := newPrivacyTestRouter(t, deps)

	// create
	req := httptest.NewRequest(http.MethodPost, "/api/v1/privacy-configs",
		bytes.NewBufferString(`{"id":"cfg","config":{"v":1}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// update
	req = httptest.NewRequest(http.MethodPut, "/api/v1/privacy-configs/cfg",
		bytes.NewBufferString(`{"id":"cfg","config":{"v":2}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// verify update
	req = httptest.NewRequest(http.MethodGet, "/api/v1/privacy-configs/cfg", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var got struct {
		Config map[string]interface{} `json:"config"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, float64(2), got.Config["v"])

	// delete
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/privacy-configs/cfg", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// get -> 404
	req = httptest.NewRequest(http.MethodGet, "/api/v1/privacy-configs/cfg", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPrivacy_NilDepsReturnsError(t *testing.T) {
	r := newPrivacyTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/privacy-configs", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
