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

func newStatsTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterStats(api, deps)
	RegisterResources(api, deps)
	return r
}

// seedResource writes a file under the caller's resources root via the
// resources router so stats endpoints have something to count.
func seedResource(t *testing.T, r *gin.Engine, account, path, content string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/resources/"+path, bytes.NewBufferString(content))
	req.Header.Set(identity.HeaderAccount, account)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestStats_ResourcesEmpty(t *testing.T) {
	deps := newTestDeps(t)
	r := newStatsTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats/resources", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Path  string `json:"path"`
		Count int64  `json:"count"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "/accounts/acct/resources", body.Path)
	assert.Equal(t, int64(0), body.Count)
}

func TestStats_ResourcesCounts(t *testing.T) {
	deps := newTestDeps(t)
	r := newStatsTestRouter(t, deps)
	seedResource(t, r, "acct", "a.txt", "hello")
	seedResource(t, r, "acct", "b.txt", "world!")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats/resources", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Count int64 `json:"count"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, int64(2), body.Count)
}

func TestStats_StorageMeasures(t *testing.T) {
	deps := newTestDeps(t)
	r := newStatsTestRouter(t, deps)
	seedResource(t, r, "acct", "a.txt", "hello") // 5 bytes
	seedResource(t, r, "acct", "b.txt", "world!") // 6 bytes

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats/storage", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Bytes int64 `json:"bytes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, int64(11), body.Bytes)
}

func TestStats_SessionsEmptyByDefault(t *testing.T) {
	deps := newTestDeps(t)
	r := newStatsTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats/sessions", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Sessions map[string]any `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Empty(t, body.Sessions)
}

func TestStats_SummaryAggregates(t *testing.T) {
	deps := newTestDeps(t)
	r := newStatsTestRouter(t, deps)
	seedResource(t, r, "acct", "a.txt", "hello")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats/summary", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, float64(1), body["resources"])
	assert.Equal(t, float64(5), body["storage_bytes"])
}
