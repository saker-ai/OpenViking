package routers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/version"
)

func newSystemTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest())
	api := r.Group("/api/v1")
	RegisterSystem(api, deps)
	return r
}

func TestSystem_Version(t *testing.T) {
	r := newSystemTestRouter(t, newTestDeps(t))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/version", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var info version.Info
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
	// version.Version defaults to "dev" when not set via ldflags.
	assert.NotEmpty(t, info.Version)
}

func TestSystem_Info(t *testing.T) {
	r := newSystemTestRouter(t, newTestDeps(t))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/info", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotEmpty(t, body["version"])
	assert.NotEmpty(t, body["go_version"])
	assert.NotEmpty(t, body["platform"])
	assert.NotZero(t, body["num_cpu"])
}

func TestSystem_Health(t *testing.T) {
	r := newSystemTestRouter(t, newTestDeps(t))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ok", body["status"])
}

func TestSystem_Ready(t *testing.T) {
	r := newSystemTestRouter(t, newTestDeps(t))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/ready", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ready", body["status"])
}
