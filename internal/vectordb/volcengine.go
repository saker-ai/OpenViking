package vectordb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// VolcengineAdapter is a CollectionAdapter backed by the Volcengine Ark
// vector store API (https://ark.cn-beijing.volces.com/api/v3).
//
// Ark exposes an OpenAI-compatible /api/v3 endpoint for embeddings and chat.
// For vector search, Ark provides a managed vector store API under the same
// base path with Bearer <api_key> auth. The wire format assumed here mirrors
// the OpenAI-compatible pattern: a vector store is created via POST
// /vectorstores, and per-store data operations live under
// /vectorstores/{store_id}/{op}.
//
// API assumptions (documented because the vector store API is not fully
// publicly specified as of writing):
//
//   - POST /vectorstores  body: {"name": "...", "dim": N, "distance": "cosine"}
//     -> 200/201 {"id": "..."}
//   - DELETE /vectorstores/{id}                                  -> 204
//   - GET  /vectorstores                                         -> 200 {"data": [{"id": "...", "name": "..."}, ...]}
//   - POST /vectorstores/{id}/upsert   body: {"rows": [...]}     -> 204
//   - POST /vectorstores/{id}/search   body: {"query": [...], "top_k": K, "filter": {...}}
//     -> 200 {"hits": [...]}
//   - POST /vectorstores/{id}/delete   body: {"ids": [...]}      -> 204
//   - POST /vectorstores/{id}/fetch    body: {"id": "..."}       -> 200 {"vector": {...}}
//   - GET  /vectorstores/{id}/count                            -> 200 {"count": N}
//
// Each hit/vector in the response is a Vector struct (see types.go). The
// adapter tolerates extra fields in the response JSON.
//
// When StoreID is set in config, EnsureCollection is a no-op (the store is
// assumed to exist); when StoreID is empty, EnsureCollection creates a new
// store and caches the returned ID per collection name. Subsequent data
// operations look up the cached ID.
type VolcengineAdapter struct {
	doer    Doer
	baseURL string
	apiKey  string
	prefix  string

	mu        sync.RWMutex
	storeIDs  map[string]string // collection name -> store ID
	pinnedID  string            // configured StoreID, used for every collection when set
}

// NewVolcengineAdapter builds a VolcengineAdapter. doer may be nil; a
// default *http.Client with a 30s timeout is used.
func NewVolcengineAdapter(_ context.Context, cfg config.VolcengineConfig, prefix string, doer Doer) (*VolcengineAdapter, error) {
	if cfg.APIKey == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vectordb: volcengine api_key is empty"))
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://ark.cn-beijing.volces.com/api/v3"
	}
	if doer == nil {
		doer = newHTTPDoer(30 * time.Second)
	}
	return &VolcengineAdapter{
		doer:    doer,
		baseURL: strings.TrimRight(base, "/"),
		apiKey:  cfg.APIKey,
		prefix:  prefix,
		storeIDs: make(map[string]string),
		pinnedID: cfg.StoreID,
	}, nil
}

// resolveStoreID returns the vector store ID to use for a collection. When
// a pinned StoreID is configured, it is used for every collection. Otherwise
// the cached ID from a prior EnsureCollection is used; if absent, the
// adapter calls EnsureCollection on-the-fly.
func (a *VolcengineAdapter) resolveStoreID(ctx context.Context, collection string) (string, error) {
	if a.pinnedID != "" {
		return a.pinnedID, nil
	}
	a.mu.RLock()
	id, ok := a.storeIDs[collection]
	a.mu.RUnlock()
	if ok {
		return id, nil
	}
	// Lazy create so callers that skip EnsureCollection still work.
	if err := a.EnsureCollection(ctx, CollectionSchema{Name: collection, Dim: 1}); err != nil {
		return "", err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.storeIDs[collection], nil
}

// volcCreateStoreReq is the body for POST /vectorstores.
type volcCreateStoreReq struct {
	Name     string `json:"name"`
	Dim      int    `json:"dim"`
	Distance string `json:"distance,omitempty"`
}

// volcCreateStoreResp is the response from POST /vectorstores.
type volcCreateStoreResp struct {
	ID string `json:"id"`
}

// volcListStoresResp is the response from GET /vectorstores.
type volcListStoresResp struct {
	Data []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"data"`
}

// volcUpsertReq is the body for POST /vectorstores/{id}/upsert.
type volcUpsertReq struct {
	Rows []Vector `json:"rows"`
}

// volcSearchReq is the body for POST /vectorstores/{id}/search.
type volcSearchReq struct {
	Query  []float32 `json:"query"`
	TopK   int       `json:"top_k"`
	Filter Filter    `json:"filter"`
}

// volcSearchResp is the response from POST /vectorstores/{id}/search.
type volcSearchResp struct {
	Hits []Vector `json:"hits"`
}

// volcDeleteReq is the body for POST /vectorstores/{id}/delete.
type volcDeleteReq struct {
	IDs []string `json:"ids"`
}

// volcFetchReq is the body for POST /vectorstores/{id}/fetch.
type volcFetchReq struct {
	ID string `json:"id"`
}

// volcFetchResp is the response from POST /vectorstores/{id}/fetch.
type volcFetchResp struct {
	Vector Vector `json:"vector"`
}

// volcCountResp is the response from GET /vectorstores/{id}/count.
type volcCountResp struct {
	Count int64 `json:"count"`
}

// EnsureCollection creates the vector store (if no pinned StoreID is set)
// and caches the returned ID for the collection name. With a pinned StoreID
// this is a no-op.
func (a *VolcengineAdapter) EnsureCollection(ctx context.Context, schema CollectionSchema) error {
	if err := validateSchema(schema); err != nil {
		return err
	}
	if a.pinnedID != "" {
		return nil
	}
	a.mu.RLock()
	_, ok := a.storeIDs[schema.Name]
	a.mu.RUnlock()
	if ok {
		return nil
	}
	dist := schema.Distance
	if dist == "" {
		dist = "cosine"
	}
	body := volcCreateStoreReq{Name: a.prefix + schema.Name, Dim: schema.Dim, Distance: dist}
	var resp volcCreateStoreResp
	url := a.baseURL + "/vectorstores"
	if err := httpJSON(ctx, a.doer, http.MethodPost, url, a.apiKey, body, &resp, 200, 201); err != nil {
		return err
	}
	if resp.ID == "" {
		return domain.Wrap(domain.CodeVectorDBError, 502,
			fmt.Errorf("vectordb: volcengine create store returned empty id for %s", schema.Name))
	}
	a.mu.Lock()
	a.storeIDs[schema.Name] = resp.ID
	a.mu.Unlock()
	return nil
}

// DropCollection deletes the vector store. With a pinned StoreID this is a
// no-op (the store is shared and outlives any one collection).
func (a *VolcengineAdapter) DropCollection(ctx context.Context, name string) error {
	if a.pinnedID != "" {
		return nil
	}
	a.mu.Lock()
	id, ok := a.storeIDs[name]
	delete(a.storeIDs, name)
	a.mu.Unlock()
	if !ok {
		return nil
	}
	url := a.baseURL + "/vectorstores/" + id
	return httpJSON(ctx, a.doer, http.MethodDelete, url, a.apiKey, nil, nil, 200, 202, 204, 404)
}

// ListCollections lists the vector stores owned by this API key. With a
// pinned StoreID, it returns just the pinned store's ID-as-name.
func (a *VolcengineAdapter) ListCollections(ctx context.Context) ([]string, error) {
	if a.pinnedID != "" {
		return []string{a.pinnedID}, nil
	}
	var resp volcListStoresResp
	url := a.baseURL + "/vectorstores"
	if err := httpJSON(ctx, a.doer, http.MethodGet, url, a.apiKey, nil, &resp, 200); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Data))
	for _, s := range resp.Data {
		// Strip the configured prefix so callers see tenant-scoped names.
		out = append(out, strings.TrimPrefix(s.Name, a.prefix))
	}
	return out, nil
}

// Upsert inserts or replaces rows by ID.
func (a *VolcengineAdapter) Upsert(ctx context.Context, collection string, rows []Vector) error {
	if collection == "" {
		return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty collection name"))
	}
	if len(rows) == 0 {
		return nil
	}
	id, err := a.resolveStoreID(ctx, collection)
	if err != nil {
		return err
	}
	body := volcUpsertReq{Rows: rows}
	url := a.baseURL + "/vectorstores/" + id + "/upsert"
	return httpJSON(ctx, a.doer, http.MethodPost, url, a.apiKey, body, nil, 200, 201, 204)
}

// Delete removes rows by ID. Unknown IDs are ignored.
func (a *VolcengineAdapter) Delete(ctx context.Context, collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	id, err := a.resolveStoreID(ctx, collection)
	if err != nil {
		return err
	}
	body := volcDeleteReq{IDs: ids}
	url := a.baseURL + "/vectorstores/" + id + "/delete"
	return httpJSON(ctx, a.doer, http.MethodPost, url, a.apiKey, body, nil, 200, 202, 204, 404)
}

// Search returns the TopK rows nearest to Query.
func (a *VolcengineAdapter) Search(ctx context.Context, params SearchParams) (*SearchResult, error) {
	if len(params.Query) == 0 {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty query vector"))
	}
	topK := params.TopK
	if topK <= 0 {
		topK = 10
	}
	id, err := a.resolveStoreID(ctx, params.Collection)
	if err != nil {
		return nil, err
	}
	body := volcSearchReq{
		Query:  append([]float32(nil), params.Query...),
		TopK:   topK,
		Filter: params.Filter,
	}
	var resp volcSearchResp
	url := a.baseURL + "/vectorstores/" + id + "/search"
	if err := httpJSON(ctx, a.doer, http.MethodPost, url, a.apiKey, body, &resp, 200); err != nil {
		return nil, err
	}
	return &SearchResult{Hits: resp.Hits}, nil
}

// Get fetches a single row by ID via the /fetch endpoint.
func (a *VolcengineAdapter) Get(ctx context.Context, collection, id string) (*Vector, error) {
	if id == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty id"))
	}
	storeID, err := a.resolveStoreID(ctx, collection)
	if err != nil {
		return nil, err
	}
	body := volcFetchReq{ID: id}
	var resp volcFetchResp
	url := a.baseURL + "/vectorstores/" + storeID + "/fetch"
	if err := httpJSON(ctx, a.doer, http.MethodPost, url, a.apiKey, body, &resp, 200); err != nil {
		return nil, err
	}
	return &resp.Vector, nil
}

// Count returns the number of rows in the store.
func (a *VolcengineAdapter) Count(ctx context.Context, collection string) (int64, error) {
	id, err := a.resolveStoreID(ctx, collection)
	if err != nil {
		return 0, err
	}
	var resp volcCountResp
	url := a.baseURL + "/vectorstores/" + id + "/count"
	if err := httpJSON(ctx, a.doer, http.MethodGet, url, a.apiKey, nil, &resp, 200); err != nil {
		return 0, err
	}
	return resp.Count, nil
}

// Close is a no-op for HTTP-based backends.
func (a *VolcengineAdapter) Close() error { return nil }
