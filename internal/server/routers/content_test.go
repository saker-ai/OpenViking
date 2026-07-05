package routers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

func TestContent_PutAndReadRaw(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)

	// PUT a file.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/content/hello.txt",
		bytes.NewBufferString("hello world"))
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// GET the bytes back.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/content/hello.txt", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hello world", rec.Body.String())
}

func TestContent_ReadMissingReturnsError(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/missing.txt", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestContent_LayerAbstractMissing(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)

	// PUT a file so the path exists.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/content/hello.txt",
		bytes.NewBufferString("hello world"))
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Read the abstract sidecar (missing -> empty string, no error).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/content/hello.txt?layer=abstract", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Path    string `json:"path"`
		Layer   string `json:"layer"`
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "abstract", body.Layer)
	assert.Equal(t, "", body.Content)
}

func TestContent_LayerAbstractWriteRead(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	ctx := reqContext(t, r, "acct")

	// Write a file and an abstract sidecar directly via ragfs.
	p := "/accounts/acct/content/hello.txt"
	require.NoError(t, deps.RAGFS.Write(ctx, p, bytes.NewBufferString("hello"), 0o644))
	require.NoError(t, ragfs.WriteAbstract(ctx, deps.RAGFS, p, "an abstract"))

	// Read it back via the API.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/hello.txt?layer=abstract", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "an abstract", body.Content)
}

func TestContent_LayerChunksListAndRead(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	ctx := reqContext(t, r, "acct")

	// Write a file and two chunks.
	p := "/accounts/acct/content/hello.txt"
	require.NoError(t, deps.RAGFS.Write(ctx, p, bytes.NewBufferString("hello"), 0o644))
	require.NoError(t, ragfs.WriteChunk(ctx, deps.RAGFS, p, "chunk_000", "first"))
	require.NoError(t, ragfs.WriteChunk(ctx, deps.RAGFS, p, "chunk_001", "second"))

	// List chunks.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/hello.txt?layer=chunks", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	var list struct {
		ChunkNames []string `json:"chunk_names"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Equal(t, []string{"chunk_000", "chunk_001"}, list.ChunkNames)

	// Read chunk index 1.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/content/hello.txt?layer=chunks:1", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	var chunk struct {
		ChunkName string `json:"chunk_name"`
		Content   string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &chunk))
	assert.Equal(t, "chunk_001", chunk.ChunkName)
	assert.Equal(t, "second", chunk.Content)
}

func TestContent_LayerUnsupported(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/hello.txt?layer=bogus", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestContent_ChunkIndexOutOfRange(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouter(t, deps)
	ctx := reqContext(t, r, "acct")
	p := "/accounts/acct/content/hello.txt"
	require.NoError(t, deps.RAGFS.Write(ctx, p, bytes.NewBufferString("hello"), 0o644))
	require.NoError(t, ragfs.WriteChunk(ctx, deps.RAGFS, p, "chunk_000", "only"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/hello.txt?layer=chunks:9", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}
