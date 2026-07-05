package vectordb

import (
	"context"
	"errors"
	"math"
	"sync"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// MemoryAdapter is an in-memory CollectionAdapter that performs brute-force
// cosine similarity search. It is intended for tests and dev deployments.
//
// The implementation is concurrency-safe via a single RWMutex: reads (Search,
// Get, Count) take the read lock, writes (Upsert, Delete, EnsureCollection,
// DropCollection) take the write lock. Brute-force search is O(N) which is
// fine for the few-thousand-row test workloads this backend is meant for.
type MemoryAdapter struct {
	mu          sync.RWMutex
	collections map[string]*memCollection
}

type memCollection struct {
	dim  int
	dist string
	rows map[string]Vector
	// norm cache keeps the L2 norm of each embedding so cosine is a single
	// dot product / (|a|*|b|). Recomputed on Upsert.
	norms map[string]float32
}

// NewMemoryAdapter returns an empty in-memory adapter.
func NewMemoryAdapter() *MemoryAdapter {
	return &MemoryAdapter{collections: make(map[string]*memCollection)}
}

// EnsureCollection creates an in-memory collection. Dim must be > 0.
func (m *MemoryAdapter) EnsureCollection(_ context.Context, schema CollectionSchema) error {
	if err := validateSchema(schema); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.collections[schema.Name]; ok {
		// Already exists: no-op if compatible, else conflict.
		if c.dim != schema.Dim {
			return domain.Wrap(domain.CodeConflict, 409,
				errors.New("vectordb: collection already exists with different dim"))
		}
		return nil
	}
	dist := schema.Distance
	if dist == "" {
		dist = "cosine"
	}
	m.collections[schema.Name] = &memCollection{
		dim:   schema.Dim,
		dist:  dist,
		rows:  make(map[string]Vector),
		norms: make(map[string]float32),
	}
	return nil
}

// DropCollection removes the collection and its rows. Missing is a no-op.
func (m *MemoryAdapter) DropCollection(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.collections, name)
	return nil
}

// ListCollections returns all collection names known to this adapter.
func (m *MemoryAdapter) ListCollections(_ context.Context) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.collections))
	for name := range m.collections {
		out = append(out, name)
	}
	return out, nil
}

// Upsert inserts or replaces rows by ID.
func (m *MemoryAdapter) Upsert(_ context.Context, collection string, rows []Vector) error {
	if collection == "" {
		return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty collection name"))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.collections[collection]
	if !ok {
		return domain.Wrap(domain.CodeConflict, 409,
			errors.New("vectordb: collection does not exist; call EnsureCollection first"))
	}
	for _, r := range rows {
		if r.ID == "" {
			return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: row id must not be empty"))
		}
		if len(r.Embedding) != c.dim {
			return domain.Wrap(domain.CodeValidationFailed, 422,
				errors.New("vectordb: embedding dim mismatch"))
		}
		// Copy embedding + metadata so later caller mutations don't poison
		// the store.
		emb := append([]float32(nil), r.Embedding...)
		meta := cloneMetadata(r.Metadata)
		c.rows[r.ID] = Vector{ID: r.ID, Embedding: emb, Metadata: meta}
		c.norms[r.ID] = l2Norm(emb)
	}
	return nil
}

// Delete removes the rows with the given IDs. Unknown IDs are ignored.
func (m *MemoryAdapter) Delete(_ context.Context, collection string, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.collections[collection]
	if !ok {
		return nil
	}
	for _, id := range ids {
		delete(c.rows, id)
		delete(c.norms, id)
	}
	return nil
}

// Search returns the TopK rows nearest to Query under cosine similarity,
// post-filtered by params.Filter.
func (m *MemoryAdapter) Search(_ context.Context, params SearchParams) (*SearchResult, error) {
	if len(params.Query) == 0 {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty query vector"))
	}
	topK := params.TopK
	if topK <= 0 {
		topK = 10
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.collections[params.Collection]
	if !ok {
		return &SearchResult{Hits: nil}, nil
	}
	if len(params.Query) != c.dim {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vectordb: query dim mismatch"))
	}
	qNorm := l2Norm(params.Query)
	hits := make([]memScored, 0, len(c.rows))
	for id, row := range c.rows {
		if !params.Filter.Matches(row) {
			continue
		}
		s := cosineSimilarity(params.Query, qNorm, row.Embedding, c.norms[id])
		hits = append(hits, memScored{v: row, score: s})
	}
	// Partial selection: keep the TopK by score descending. We do a simple
	// sort since N is small in the memory backend; for large N a heap would
	// be better.
	sortMemScoredDesc(hits)
	if len(hits) > topK {
		hits = hits[:topK]
	}
	out := make([]Vector, 0, len(hits))
	for _, h := range hits {
		// Copy so the caller can't mutate the stored row.
		rr := Vector{
			ID:        h.v.ID,
			Embedding: append([]float32(nil), h.v.Embedding...),
			Metadata:  cloneMetadata(h.v.Metadata),
			Score:     h.score,
		}
		out = append(out, rr)
	}
	return &SearchResult{Hits: out}, nil
}

// Get fetches a single row by ID.
func (m *MemoryAdapter) Get(_ context.Context, collection, id string) (*Vector, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.collections[collection]
	if !ok {
		return nil, domain.Wrap(domain.CodeResourceNotFound, 404,
			errors.New("vectordb: collection not found"))
	}
	row, ok := c.rows[id]
	if !ok {
		return nil, domain.Wrap(domain.CodeResourceNotFound, 404,
			errors.New("vectordb: row not found"))
	}
	rr := Vector{
		ID:        row.ID,
		Embedding: append([]float32(nil), row.Embedding...),
		Metadata:  cloneMetadata(row.Metadata),
	}
	return &rr, nil
}

// Count returns the number of rows in the collection.
func (m *MemoryAdapter) Count(_ context.Context, collection string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.collections[collection]
	if !ok {
		return 0, nil
	}
	return int64(len(c.rows)), nil
}

// Close drops all in-memory state.
func (m *MemoryAdapter) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collections = nil
	return nil
}

// memScored pairs a stored row with its similarity score for in-memory
// TopK selection.
type memScored struct {
	v     Vector
	score float32
}

func l2Norm(v []float32) float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return float32(math.Sqrt(sum))
}

func cosineSimilarity(a []float32, aNorm float32, b []float32, bNorm float32) float32 {
	if aNorm == 0 || bNorm == 0 {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return float32(dot) / (aNorm * bNorm)
}

func sortMemScoredDesc(hits []memScored) {
	// In-place insertion sort: small N, no allocs, stable.
	for i := 1; i < len(hits); i++ {
		j := i
		for j > 0 && hits[j-1].score < hits[j].score {
			hits[j-1], hits[j] = hits[j], hits[j-1]
			j--
		}
	}
}

func cloneMetadata(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func validateSchema(s CollectionSchema) error {
	if s.Name == "" {
		return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty collection name"))
	}
	if s.Dim <= 0 {
		return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: dim must be > 0"))
	}
	switch s.Distance {
	case "", "cosine", "l2", "ip":
	default:
		return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: distance must be cosine|l2|ip"))
	}
	return nil
}
