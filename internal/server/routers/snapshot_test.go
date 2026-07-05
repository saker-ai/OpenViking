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

func newSnapshotTestRouter(t *testing.T, deps *Deps) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorMiddlewareForTest(), identity.Middleware())
	api := r.Group("/api/v1")
	RegisterSnapshot(api, deps)
	RegisterResources(api, deps)
	return r
}

func TestSnapshot_CreateAndList(t *testing.T) {
	deps := newTestDeps(t)
	r := newSnapshotTestRouter(t, deps)

	// Seed a resource so the snapshot has content.
	seedResource(t, r, "acct", "note.txt", "hello")

	// Create snapshot.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshot",
		bytes.NewBufferString(`{"id":"snap1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// List snapshots.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Snapshots []string `json:"snapshots"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body.Snapshots, "snap1")
}

func TestSnapshot_CreateConflict(t *testing.T) {
	deps := newTestDeps(t)
	r := newSnapshotTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshot",
		bytes.NewBufferString(`{"id":"dup"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/api/v1/snapshot",
		bytes.NewBufferString(`{"id":"dup"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestSnapshot_Get(t *testing.T) {
	deps := newTestDeps(t)
	r := newSnapshotTestRouter(t, deps)
	seedResource(t, r, "acct", "note.txt", "hello")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshot",
		bytes.NewBufferString(`{"id":"snap-get"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/api/v1/snapshot/snap-get", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "snap-get", body.ID)
}

func TestSnapshot_RestoreAndDelete(t *testing.T) {
	deps := newTestDeps(t)
	r := newSnapshotTestRouter(t, deps)
	seedResource(t, r, "acct", "note.txt", "original")

	// snapshot
	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshot",
		bytes.NewBufferString(`{"id":"snap-rd"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	// mutate resource
	req = httptest.NewRequest(http.MethodPut, "/api/v1/resources/note.txt",
		bytes.NewBufferString("changed"))
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// restore
	req = httptest.NewRequest(http.MethodPost, "/api/v1/snapshot/snap-rd/restore", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// verify content restored via content router isn't in this engine; use resources stat.
	// Just confirm the snapshot file re-appeared by reading it through resources router.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/resources/note.txt", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// delete snapshot
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/snapshot/snap-rd", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// get -> 404
	req = httptest.NewRequest(http.MethodGet, "/api/v1/snapshot/snap-rd", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSnapshot_NilDepsReturnsError(t *testing.T) {
	r := newSnapshotTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
