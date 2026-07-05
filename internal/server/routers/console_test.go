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

	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

func TestConsole_ListCollectionsEmpty(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/collections", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Mounts      []string `json:"mounts"`
		Collections []string `json:"collections"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body.Mounts, "/")
	assert.Empty(t, body.Collections)
}

func TestConsole_CreateAndDropCollection(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	// Create a collection.
	body := `{"kind":"collection","name":"ov_acct__file","schema":{"name":"ov_acct__file","dim":8,"distance":"cosine"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections",
		jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// List collections should now include it.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/console/collections", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var list struct {
		Collections []string `json:"collections"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Contains(t, list.Collections, "ov_acct__file")

	// Drop it.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/console/collections/ov_acct__file", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// List should now be empty.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/console/collections", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.NotContains(t, list.Collections, "ov_acct__file")
}

func TestConsole_MountAndUnmount(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	// Mount requires admin token (per the task spec).
	body := `{"kind":"mount","name":"extra","backend":"memory"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections",
		jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set("X-OpenViking-Admin-Token", "test-admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// List mounts should include "extra".
	req = httptest.NewRequest(http.MethodGet, "/api/v1/console/collections", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var list struct {
		Mounts []string `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Contains(t, list.Mounts, "/extra")

	// Unmount by path.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/console/collections/extra", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestConsole_MountForbiddenWithoutAdminToken(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	body := `{"kind":"mount","name":"extra","backend":"memory"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections",
		jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestConsole_MountLocalBackend mounts a local filesystem backend via the
// console API and verifies it shows up in the mounts list. The local path is
// a t.TempDir() so the test does not touch the real filesystem.
func TestConsole_MountLocalBackend(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	dir := t.TempDir()
	body := `{"kind":"mount","name":"localmount","backend":"local","path":"` + dir + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections",
		jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set("X-OpenViking-Admin-Token", "test-admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp struct {
		Mount   string `json:"mount"`
		Backend string `json:"backend"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "/localmount", resp.Mount)
	assert.Equal(t, "local", resp.Backend)

	// List mounts should include "/localmount".
	req = httptest.NewRequest(http.MethodGet, "/api/v1/console/collections", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var list struct {
		Mounts []string `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Contains(t, list.Mounts, "/localmount")
}

// TestConsole_MountLocalMissingPath verifies a local mount without a path
// is rejected with 422.
func TestConsole_MountLocalMissingPath(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	body := `{"kind":"mount","name":"localmount","backend":"local"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections",
		jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set("X-OpenViking-Admin-Token", "test-admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// TestConsole_MountS3Backend mounts an S3 backend via the console API. The
// s3fs stub reports healthy without a real S3 endpoint, so this only verifies
// the mount table registers the backend (not real network I/O).
func TestConsole_MountS3Backend(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	body := `{"kind":"mount","name":"s3mount","backend":"s3","endpoint":"https://s3.example.com","bucket":"test-bucket","region":"us-east-1","access_key":"ak","secret_key":"sk"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections",
		jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set("X-OpenViking-Admin-Token", "test-admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp struct {
		Mount   string `json:"mount"`
		Backend string `json:"backend"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "/s3mount", resp.Mount)
	assert.Equal(t, "s3", resp.Backend)

	// List mounts should include "/s3mount".
	req = httptest.NewRequest(http.MethodGet, "/api/v1/console/collections", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var list struct {
		Mounts []string `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Contains(t, list.Mounts, "/s3mount")
}

// TestConsole_MountS3MissingEndpoint verifies an s3 mount without endpoint
// or bucket is rejected with 422.
func TestConsole_MountS3MissingEndpoint(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	body := `{"kind":"mount","name":"s3mount","backend":"s3"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections",
		jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set("X-OpenViking-Admin-Token", "test-admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

// TestConsole_MountUnknownBackend verifies an unknown backend name is
// rejected with 422.
func TestConsole_MountUnknownBackend(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	body := `{"kind":"mount","name":"unknown","backend":"ftp"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections",
		jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	req.Header.Set("X-OpenViking-Admin-Token", "test-admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestConsole_Health(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/health", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ok", body.Status)
}

func TestConsole_CreateValidation(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)

	// kind=collection with empty name should fail.
	body := `{"kind":"collection","name":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections",
		jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestConsole_NilDepsReturnsEmpty(t *testing.T) {
	// With nil services the console list endpoint degrades to an empty
	// object rather than 501; this is intentional so console UIs can boot
	// before backends are wired.
	r := newTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/collections", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// ensureCollection creates a vectordb collection via the console API. Helper
// for upsert/search/delete tests which all need a collection to exist.
func ensureCollection(t *testing.T, r *gin.Engine, name string) {
	t.Helper()
	body := `{"kind":"collection","name":"` + name + `","schema":{"name":"` + name + `","dim":4,"distance":"cosine"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections", jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestConsole_UpsertAndSearch(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)
	collection := "ov_acct__file"
	ensureCollection(t, r, collection)

	// Upsert two vectors.
	upsertBody := `{"vectors":[{"id":"v1","embedding":[1.0,0.0,0.0,0.0],"metadata":{"kind":"file"}},{"id":"v2","embedding":[0.0,1.0,0.0,0.0],"metadata":{"kind":"file"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections/"+collection+"/upsert", jsonBody(upsertBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var upsertResp struct {
		Upserted int `json:"upserted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &upsertResp))
	assert.Equal(t, 2, upsertResp.Upserted)

	// Search for the nearest neighbor of [1.0,0.0,0.0,0.0] — should be v1.
	searchBody := `{"query":[1.0,0.0,0.0,0.0],"top_k":1}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/console/collections/"+collection+"/search", jsonBody(searchBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var searchResp struct {
		Hits []vectordb.Vector `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &searchResp))
	require.Len(t, searchResp.Hits, 1)
	assert.Equal(t, "v1", searchResp.Hits[0].ID)
}

func TestConsole_SearchWithFilter(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)
	collection := "ov_acct__filter"
	ensureCollection(t, r, collection)

	// Upsert two vectors with different "kind" metadata.
	upsertBody := `{"vectors":[{"id":"a","embedding":[1.0,0.0,0.0,0.0],"metadata":{"kind":"file"}},{"id":"b","embedding":[1.0,0.0,0.0,0.0],"metadata":{"kind":"image"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections/"+collection+"/upsert", jsonBody(upsertBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Search with filter kind=image — only "b" should match.
	searchBody := `{"query":[1.0,0.0,0.0,0.0],"top_k":5,"filter":{"kind":"image"}}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/console/collections/"+collection+"/search", jsonBody(searchBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var searchResp struct {
		Hits []vectordb.Vector `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &searchResp))
	require.Len(t, searchResp.Hits, 1)
	assert.Equal(t, "b", searchResp.Hits[0].ID)
}

func TestConsole_Delete(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)
	collection := "ov_acct__del"
	ensureCollection(t, r, collection)

	// Upsert two vectors.
	upsertBody := `{"vectors":[{"id":"d1","embedding":[1.0,0.0,0.0,0.0]},{"id":"d2","embedding":[0.0,1.0,0.0,0.0]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections/"+collection+"/upsert", jsonBody(upsertBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Delete d1.
	deleteBody := `{"ids":["d1"]}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/console/collections/"+collection+"/delete", jsonBody(deleteBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var deleteResp struct {
		Deleted int `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &deleteResp))
	assert.Equal(t, 1, deleteResp.Deleted)

	// Search should now only return d2.
	searchBody := `{"query":[1.0,0.0,0.0,0.0],"top_k":5}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/console/collections/"+collection+"/search", jsonBody(searchBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var searchResp struct {
		Hits []vectordb.Vector `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &searchResp))
	require.Len(t, searchResp.Hits, 1)
	assert.Equal(t, "d2", searchResp.Hits[0].ID)
}

func TestConsole_UpsertValidation(t *testing.T) {
	deps := newTestDeps(t)
	deps.VectorDB = vectordb.NewMemoryAdapter()
	r := newTestRouter(t, deps)
	collection := "ov_acct__val"
	ensureCollection(t, r, collection)

	// Empty vectors array should fail validation.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections/"+collection+"/upsert", jsonBody(`{"vectors":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestConsole_SearchNilDepsReturns501(t *testing.T) {
	r := newTestRouter(t, &Deps{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/console/collections/any/search", jsonBody(`{"query":[1.0],"top_k":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// jsonBody is a small helper to avoid repeating bytes.NewReaderString.
func jsonBody(s string) *bytes.Reader {
	return bytes.NewReader([]byte(s))
}

// bytes is imported via the helper above.
var _ = ragfs.Normalize
