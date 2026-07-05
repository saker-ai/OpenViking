package vectordb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// newHTTPTestServer returns an httptest.Server whose handler dispatches to
// the per-op handlers. Each handler is a function from request body to
// (response body, status). The server records the number of requests per op.
func newHTTPTestServer(t *testing.T, handlers map[string]func(body []byte) (resp any, status int)) (*httptest.Server, map[string]*int32) {
	t.Helper()
	counts := make(map[string]*int32)
	for op := range handlers {
		counts[op] = new(int32)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Dispatch by the "op" form value or by path suffix.
		op := r.URL.Query().Get("op")
		if op == "" {
			op = strings.TrimPrefix(r.URL.Path, "/")
		}
		counter, ok := counts[op]
		if !ok {
			// No handler registered for this op; return 404 so the adapter
			// surfaces a wrapped error.
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(counter, 1)
		body, _ := io.ReadAll(r.Body)
		h, ok := handlers[op]
		if !ok {
			http.NotFound(w, r)
			return
		}
		resp, status := h(body)
		if status == 0 {
			status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if resp != nil {
			_ = json.NewEncoder(w).Encode(resp)
		}
	})
	return httptest.NewServer(mux), counts
}

func TestHTTPAdapter_EnsureCollection_Upsert_Search_Get_Count(t *testing.T) {
	t.Parallel()
	var stored []Vector
	handlers := map[string]func([]byte) (any, int){
		"ensure_collection": func(_ []byte) (any, int) { return httpRespEmpty{OK: true}, 200 },
		"upsert": func(b []byte) (any, int) {
			var req httpReqUpsert
			require.NoError(t, json.Unmarshal(b, &req))
			stored = append(stored, req.Rows...)
			return httpRespEmpty{OK: true}, 201
		},
		"search": func(b []byte) (any, int) {
			var req httpReqSearch
			require.NoError(t, json.Unmarshal(b, &req))
			hits := make([]Vector, 0, len(stored))
			for _, r := range stored {
				hits = append(hits, Vector{ID: r.ID, Score: 0.5, Metadata: cloneMetadata(r.Metadata)})
			}
			return httpRespSearch{Hits: hits}, 200
		},
		"get": func(b []byte) (any, int) {
			var req httpReqGet
			require.NoError(t, json.Unmarshal(b, &req))
			for _, r := range stored {
				if r.ID == req.ID {
					return httpRespGet{Vector: r}, 200
				}
			}
			return nil, 404
		},
		"count": func(_ []byte) (any, int) {
			return httpRespCount{Count: int64(len(stored))}, 200
		},
		"delete": func(b []byte) (any, int) {
			var req httpReqDelete
			require.NoError(t, json.Unmarshal(b, &req))
			keep := stored[:0]
			for _, r := range stored {
				drop := false
				for _, id := range req.IDs {
					if r.ID == id {
						drop = true
						break
					}
				}
				if !drop {
					keep = append(keep, r)
				}
			}
			stored = keep
			return httpRespEmpty{OK: true}, 200
		},
		"drop_collection": func(_ []byte) (any, int) { return httpRespEmpty{OK: true}, 200 },
		"list_collections": func(_ []byte) (any, int) {
			return httpRespList{Collections: []string{"ov_acme__doc"}}, 200
		},
	}
	srv, counts := newHTTPTestServer(t, handlers)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { stored = nil })

	cfg := config.HTTPVectorConfig{
		URL:                 srv.URL,
		APIKey:              "secret",
		EnsureCollectionURL: srv.URL + "?op=ensure_collection",
		DropCollectionURL:   srv.URL + "?op=drop_collection",
		ListCollectionsURL:  srv.URL + "?op=list_collections",
		UpsertURL:           srv.URL + "?op=upsert",
		SearchURL:           srv.URL + "?op=search",
		DeleteURL:           srv.URL + "?op=delete",
		GetURL:              srv.URL + "?op=get",
		CountURL:            srv.URL + "?op=count",
	}
	a, err := NewHTTPAdapter(context.Background(), cfg, srv.Client())
	require.NoError(t, err)
	defer a.Close()

	ctx := context.Background()
	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "ov_acme__doc", Dim: 3, Distance: "cosine"}))
	require.Equal(t, int32(1), atomic.LoadInt32(counts["ensure_collection"]))

	rows := []Vector{
		{ID: "d1", Embedding: []float32{1, 0, 0}, Metadata: map[string]any{"account": "acme", "kind": "doc", "uri": "viking://d/1"}},
		{ID: "d2", Embedding: []float32{0, 1, 0}, Metadata: map[string]any{"account": "acme", "kind": "doc", "uri": "viking://d/2"}},
	}
	require.NoError(t, a.Upsert(ctx, "ov_acme__doc", rows))
	require.Equal(t, int32(1), atomic.LoadInt32(counts["upsert"]))

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

	names, err := a.ListCollections(ctx)
	require.NoError(t, err)
	assert.Contains(t, names, "ov_acme__doc")

	require.NoError(t, a.DropCollection(ctx, "ov_acme__doc"))
}

func TestHTTPAdapter_FallbackURL(t *testing.T) {
	t.Parallel()
	// No per-op overrides; adapter falls back to URL + "/<op>".
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"collections":[]}`))
	}))
	t.Cleanup(srv.Close)
	cfg := config.HTTPVectorConfig{URL: srv.URL}
	a, err := NewHTTPAdapter(context.Background(), cfg, srv.Client())
	require.NoError(t, err)
	defer a.Close()
	_, err = a.ListCollections(context.Background())
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(seenPath, "/list_collections"), "path=%q", seenPath)
}

func TestHTTPAdapter_ServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(srv.Close)
	cfg := config.HTTPVectorConfig{URL: srv.URL, CountURL: srv.URL + "?op=count"}
	a, err := NewHTTPAdapter(context.Background(), cfg, srv.Client())
	require.NoError(t, err)
	defer a.Close()
	_, err = a.Count(context.Background(), "ov_acme__doc")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeVectorDBError, appErr.Code)
}

func TestHTTPAdapter_NotFoundMapsTo404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	cfg := config.HTTPVectorConfig{URL: srv.URL, GetURL: srv.URL}
	a, err := NewHTTPAdapter(context.Background(), cfg, srv.Client())
	require.NoError(t, err)
	defer a.Close()
	_, err = a.Get(context.Background(), "ov_acme__doc", "missing")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeResourceNotFound, appErr.Code)
}

func TestHTTPAdapter_RequiresURL(t *testing.T) {
	t.Parallel()
	_, err := NewHTTPAdapter(context.Background(), config.HTTPVectorConfig{}, nil)
	require.Error(t, err)
}

func TestHTTPAdapter_SearchRejectsEmptyQuery(t *testing.T) {
	t.Parallel()
	cfg := config.HTTPVectorConfig{URL: "http://example.invalid", SearchURL: "http://example.invalid/search"}
	a, err := NewHTTPAdapter(context.Background(), cfg, nil)
	require.NoError(t, err)
	defer a.Close()
	_, err = a.Search(context.Background(), SearchParams{Collection: "c", Query: nil, TopK: 5})
	require.Error(t, err)
}

func TestHTTPAdapter_NilDoerRejected(t *testing.T) {
	t.Parallel()
	// httpJSON surfaces a validation error when doer is nil. We exercise
	// this via the internal helper directly since NewHTTPAdapter installs a
	// default doer when nil is passed.
	err := httpJSON(context.Background(), nil, http.MethodPost,
		"http://example.invalid/", "", struct{}{}, nil, 200)
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeValidationFailed, appErr.Code)
}

func TestReadSnippet(t *testing.T) {
	assert.Equal(t, "hi", readSnippet(strings.NewReader("hi"), 10))
	assert.Equal(t, "<unreadable body>", readSnippet(errReader{}, 10))
}

// errReader is a tiny io.Reader that always errors.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
