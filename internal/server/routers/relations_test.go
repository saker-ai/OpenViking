package routers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/server/identity"
)

func TestRelations_CreateAndList(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	// Create two edges.
	for _, body := range []string{
		`{"source":"/a","target":"/b","kind":"link"}`,
		`{"source":"/b","target":"/c","kind":"link"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/relations", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(identity.HeaderAccount, "acct")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	// List should show both edges.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/relations", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Edges []relationEdge `json:"edges"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Edges, 2)
	assert.Equal(t, "/a", resp.Edges[0].Source)
	assert.NotEmpty(t, resp.Edges[0].ID)
}

func TestRelations_Delete(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	// Create an edge.
	body := `{"source":"/a","target":"/b"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/relations", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)
	var created struct {
		Edge relationEdge `json:"edge"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	edgeID := created.Edge.ID
	require.NotEmpty(t, edgeID)

	// Delete it.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/relations/"+edgeID, nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// List should now be empty.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/relations", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var resp struct {
		Edges []relationEdge `json:"edges"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.Edges)
}

func TestRelations_DeleteMissing(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/relations/nonexistent", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRelations_Neighbors(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	// Create edges: /a -> /b, /b -> /c, /c -> /d.
	for _, body := range []string{
		`{"source":"/a","target":"/b"}`,
		`{"source":"/b","target":"/c"}`,
		`{"source":"/c","target":"/d"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/relations", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(identity.HeaderAccount, "acct")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code)
	}

	// Neighbors of /b should be the two edges touching it.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/relations/neighbors?node=/b", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Node      string         `json:"node"`
		Neighbors []relationEdge `json:"neighbors"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "/b", resp.Node)
	require.Len(t, resp.Neighbors, 2)
}

func TestRelations_Paths(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	// Build a chain /a -> /b -> /c -> /d.
	for _, body := range []string{
		`{"source":"/a","target":"/b"}`,
		`{"source":"/b","target":"/c"}`,
		`{"source":"/c","target":"/d"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/relations", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(identity.HeaderAccount, "acct")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/relations/paths?source=/a&target=/d", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Source string     `json:"source"`
		Target string     `json:"target"`
		Paths  [][]string `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "/a", resp.Source)
	assert.Equal(t, "/d", resp.Target)
	require.Len(t, resp.Paths, 1)
	require.Len(t, resp.Paths[0], 4)
	assert.Equal(t, []string{"/a", "/b", "/c", "/d"}, resp.Paths[0])
}

func TestRelations_Validation(t *testing.T) {
	deps := newTestDeps(t)
	r := newTestRouterAll(t, deps)

	// Missing source -> 422.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/relations",
		bytes.NewBufferString(`{"source":"","target":"/b"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestRelations_NilDepsReturns501(t *testing.T) {
	r := newTestRouterAll(t, &Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/relations", nil)
	req.Header.Set(identity.HeaderAccount, "acct")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
