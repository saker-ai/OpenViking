package vectordb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// volcTestServer is a stub for the Ark /api/v3 vector store API. It records
// every request and dispatches by method + path suffix to per-op handlers.
type volcTestServer struct {
	t        *testing.T
	mu       sync.Mutex
	stores   map[string]string // store ID -> collection name
	rows     map[string][]Vector
	nextID   int
	requests []recReq
}

type recReq struct {
	method string
	path   string
	body   []byte
}

func newVolcTestServer(t *testing.T) (*httptest.Server, *volcTestServer) {
	t.Helper()
	srv := &volcTestServer{
		t:      t,
		stores: make(map[string]string),
		rows:   make(map[string][]Vector),
		nextID: 1,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/vectorstores", func(w http.ResponseWriter, r *http.Request) {
		srv.handleStores(w, r)
	})
	mux.HandleFunc("/vectorstores/", func(w http.ResponseWriter, r *http.Request) {
		srv.handleStoreOps(w, r)
	})
	return httptest.NewServer(mux), srv
}

func (s *volcTestServer) handleStores(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	s.requests = append(s.requests, recReq{method: r.Method, path: r.URL.Path, body: body})
	switch r.Method {
	case http.MethodPost:
		var req volcCreateStoreReq
		require.NoError(s.t, json.Unmarshal(body, &req))
		id := "store-" + itoa(s.nextID)
		s.nextID++
		s.stores[id] = req.Name
		s.rows[id] = nil
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"` + id + `"}`))
	case http.MethodGet:
		// List stores.
		type store struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		out := struct {
			Data []store `json:"data"`
		}{}
		for id, name := range s.stores {
			out.Data = append(out.Data, store{ID: id, Name: name})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

func (s *volcTestServer) handleStoreOps(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	s.requests = append(s.requests, recReq{method: r.Method, path: r.URL.Path, body: body})

	// Path shape: /vectorstores/{id}/{op}
	rest := strings.TrimPrefix(r.URL.Path, "/vectorstores/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 2 {
		// /vectorstores/{id} — DELETE store.
		if r.Method == http.MethodDelete {
			id := parts[0]
			delete(s.stores, id)
			delete(s.rows, id)
			w.WriteHeader(204)
			return
		}
		http.NotFound(w, r)
		return
	}
	id, op := parts[0], parts[1]
	_, ok := s.stores[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch op {
	case "upsert":
		var req volcUpsertReq
		require.NoError(s.t, json.Unmarshal(body, &req))
		for _, row := range req.Rows {
			s.upsertRow(id, row)
		}
		w.WriteHeader(204)
	case "search":
		var req volcSearchReq
		require.NoError(s.t, json.Unmarshal(body, &req))
		hits := make([]Vector, 0, len(s.rows[id]))
		for _, row := range s.rows[id] {
			if !req.Filter.Matches(row) {
				continue
			}
			clone := Vector{
				ID:       row.ID,
				Score:    0.9,
				Metadata: cloneMetadata(row.Metadata),
			}
			hits = append(hits, clone)
			if len(hits) >= req.TopK {
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(volcSearchResp{Hits: hits})
	case "delete":
		var req volcDeleteReq
		require.NoError(s.t, json.Unmarshal(body, &req))
		keep := s.rows[id][:0]
		for _, row := range s.rows[id] {
			drop := false
			for _, did := range req.IDs {
				if row.ID == did {
					drop = true
					break
				}
			}
			if !drop {
				keep = append(keep, row)
			}
		}
		s.rows[id] = keep
		w.WriteHeader(204)
	case "fetch":
		var req volcFetchReq
		require.NoError(s.t, json.Unmarshal(body, &req))
		for _, row := range s.rows[id] {
			if row.ID == req.ID {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_ = json.NewEncoder(w).Encode(volcFetchResp{Vector: row})
				return
			}
		}
		http.NotFound(w, r)
	case "count":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(volcCountResp{Count: int64(len(s.rows[id]))})
	default:
		http.NotFound(w, r)
	}
}

func (s *volcTestServer) upsertRow(id string, row Vector) {
	for i, r := range s.rows[id] {
		if r.ID == row.ID {
			s.rows[id][i] = row
			return
		}
	}
	s.rows[id] = append(s.rows[id], row)
}

// itoa is a tiny strconv.Itoa alternative to keep the test file imports light.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestVolcengineAdapter_FullFlow(t *testing.T) {
	t.Parallel()
	srv, stub := newVolcTestServer(t)
	t.Cleanup(srv.Close)
	cfg := config.VolcengineConfig{
		BaseURL: srv.URL,
		APIKey:  "ark-test-key",
	}
	a, err := NewVolcengineAdapter(context.Background(), cfg, "ov_", srv.Client())
	require.NoError(t, err)
	defer a.Close()
	ctx := context.Background()

	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "ov_acme__doc", Dim: 3, Distance: "cosine"}))
	require.Len(t, stub.stores, 1)

	rows := []Vector{
		{ID: "d1", Embedding: []float32{1, 0, 0}, Metadata: map[string]any{"account": "acme", "kind": "doc", "uri": "viking://d/1"}},
		{ID: "d2", Embedding: []float32{0, 1, 0}, Metadata: map[string]any{"account": "acme", "kind": "doc", "uri": "viking://d/2"}},
	}
	require.NoError(t, a.Upsert(ctx, "ov_acme__doc", rows))

	res, err := a.Search(ctx, SearchParams{
		Collection: "ov_acme__doc",
		Query:      []float32{1, 0, 0},
		TopK:       5,
		Filter:     Filter{Account: "acme"},
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 2)
	assert.Equal(t, "d1", res.Hits[0].ID)

	got, err := a.Get(ctx, "ov_acme__doc", "d2")
	require.NoError(t, err)
	assert.Equal(t, "d2", got.ID)

	n, err := a.Count(ctx, "ov_acme__doc")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	require.NoError(t, a.Delete(ctx, "ov_acme__doc", []string{"d1"}))
	n, err = a.Count(ctx, "ov_acme__doc")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	require.NoError(t, a.DropCollection(ctx, "ov_acme__doc"))
	require.Empty(t, stub.stores)
}

func TestVolcengineAdapter_PinnedStoreID(t *testing.T) {
	t.Parallel()
	srv, stub := newVolcTestServer(t)
	t.Cleanup(srv.Close)
	// Pre-create one store via the stub directly so we have an ID.
	stub.mu.Lock()
	stub.stores["store-pinned"] = "ov_pinned"
	stub.rows["store-pinned"] = nil
	stub.mu.Unlock()

	cfg := config.VolcengineConfig{
		BaseURL: srv.URL,
		APIKey:  "ark-test-key",
		StoreID: "store-pinned",
	}
	a, err := NewVolcengineAdapter(context.Background(), cfg, "ov_", srv.Client())
	require.NoError(t, err)
	defer a.Close()
	ctx := context.Background()

	// EnsureCollection is a no-op when StoreID is set.
	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "ov_x__doc", Dim: 3}))
	// No new stores were created.
	require.Len(t, stub.stores, 1)

	// Upsert + Count use the pinned store.
	require.NoError(t, a.Upsert(ctx, "ov_x__doc", []Vector{{ID: "p1", Embedding: []float32{1}}}))
	n, err := a.Count(ctx, "ov_x__doc")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	// DropCollection is a no-op with pinned ID.
	require.NoError(t, a.DropCollection(ctx, "ov_x__doc"))
	require.Len(t, stub.stores, 1)
}

func TestVolcengineAdapter_RequiresAPIKey(t *testing.T) {
	t.Parallel()
	_, err := NewVolcengineAdapter(context.Background(), config.VolcengineConfig{}, "ov_", nil)
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeValidationFailed, appErr.Code)
}

func TestVolcengineAdapter_SearchRejectsEmptyQuery(t *testing.T) {
	t.Parallel()
	srv, _ := newVolcTestServer(t)
	t.Cleanup(srv.Close)
	cfg := config.VolcengineConfig{BaseURL: srv.URL, APIKey: "k"}
	a, err := NewVolcengineAdapter(context.Background(), cfg, "ov_", srv.Client())
	require.NoError(t, err)
	defer a.Close()
	_, err = a.Search(context.Background(), SearchParams{Collection: "c", Query: nil, TopK: 5})
	require.Error(t, err)
}

func TestVolcengineAdapter_ServerErrorWrapped(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	t.Cleanup(srv.Close)
	cfg := config.VolcengineConfig{BaseURL: srv.URL, APIKey: "k", StoreID: "s1"}
	a, err := NewVolcengineAdapter(context.Background(), cfg, "ov_", srv.Client())
	require.NoError(t, err)
	defer a.Close()
	_, err = a.Count(context.Background(), "c")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeVectorDBError, appErr.Code)
}

func TestVolcengineAdapter_NotFoundOnFetch(t *testing.T) {
	t.Parallel()
	srv, _ := newVolcTestServer(t)
	t.Cleanup(srv.Close)
	cfg := config.VolcengineConfig{BaseURL: srv.URL, APIKey: "k", StoreID: "missing"}
	a, err := NewVolcengineAdapter(context.Background(), cfg, "ov_", srv.Client())
	require.NoError(t, err)
	defer a.Close()
	// The store "missing" doesn't exist in the stub; fetch returns 404.
	_, err = a.Get(context.Background(), "c", "x")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeResourceNotFound, appErr.Code)
}

func TestVolcengineAdapter_ListCollections(t *testing.T) {
	t.Parallel()
	srv, stub := newVolcTestServer(t)
	t.Cleanup(srv.Close)
	stub.mu.Lock()
	stub.stores["s1"] = "ov_acme__doc"
	stub.stores["s2"] = "ov_acme__file"
	stub.mu.Unlock()
	cfg := config.VolcengineConfig{BaseURL: srv.URL, APIKey: "k"}
	a, err := NewVolcengineAdapter(context.Background(), cfg, "ov_", srv.Client())
	require.NoError(t, err)
	defer a.Close()
	names, err := a.ListCollections(context.Background())
	require.NoError(t, err)
	assert.Contains(t, names, "acme__doc")
	assert.Contains(t, names, "acme__file")
}

func TestVolcengineAdapterInterface(t *testing.T) {
	var _ CollectionAdapter = (*VolcengineAdapter)(nil)
}
