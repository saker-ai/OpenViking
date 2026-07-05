package vectordb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/hnsw"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// LocalAdapter is an on-disk CollectionAdapter backed by github.com/coder/hnsw.
//
// API quirks of coder/hnsw (documented for future maintainers):
//
//   - Persistence uses LoadSavedGraph + SavedGraph.Save which atomically write
//     the graph to a single file. Plain Graph.Export/Import on a fresh
//     NewGraph silently yields an empty graph (Import reads the header but
//     does not restore the node map); always use LoadSavedGraph for files.
//   - Node[K] only carries a Key and a Vector; there is no metadata slot.
//     We store metadata (account/kind/uri/...) in a sidecar JSON file
//     "<collection>.meta.json" next to "<collection>.bin".
//   - coder/hnsw exposes CosineDistance, EuclideanDistance, and
//     InnerProductDistance. We map schema.Distance{cosine,l2,ip} to these.
//   - The graph does not return a Score on Search; we re-compute the score
//     (cosine similarity) from the returned Node.Value so callers get a
//     comparable similarity.
type LocalAdapter struct {
	path   string
	prefix string

	mu          sync.Mutex
	collections map[string]*localCollection
}

type localCollection struct {
	name  string
	dim   int
	dist  string
	graph *hnsw.SavedGraph[string]
	mu    sync.RWMutex
	meta  map[string]map[string]any // id -> metadata
	dirty bool
}

// NewLocalAdapter opens (or creates) a LocalAdapter rooted at path.
// path must already exist (the factory creates it).
func NewLocalAdapter(path, prefix string) (*LocalAdapter, error) {
	if path == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty local path"))
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		return nil, domain.Wrap(domain.CodeVectorDBError, 500,
			fmt.Errorf("vectordb: local path %s not a directory", path))
	}
	a := &LocalAdapter{
		path:        path,
		prefix:      prefix,
		collections: make(map[string]*localCollection),
	}
	return a, nil
}

// graphPath returns the .bin file path for a collection.
func (a *LocalAdapter) graphPath(name string) string {
	return filepath.Join(a.path, name+".bin")
}

// metaPath returns the sidecar metadata file path for a collection.
func (a *LocalAdapter) metaPath(name string) string {
	return filepath.Join(a.path, name+".meta.json")
}

// loadCollectionLocked opens (or creates) the on-disk graph+meta for name.
// Caller must hold a.mu.
func (a *LocalAdapter) loadCollectionLocked(name string, dim int, dist string) (*localCollection, error) {
	if c, ok := a.collections[name]; ok {
		return c, nil
	}
	sg, err := hnsw.LoadSavedGraph[string](a.graphPath(name))
	if err != nil {
		return nil, domain.Wrap(domain.CodeVectorDBError, 500,
			fmt.Errorf("vectordb: load graph %s: %w", name, err))
	}
	if sg == nil {
		return nil, domain.Wrap(domain.CodeVectorDBError, 500,
			fmt.Errorf("vectordb: load graph %s returned nil", name))
	}
	g := sg.Graph
	g.M = 16
	g.EfSearch = 64
	g.Distance = pickDistance(dist)
	// Load sidecar metadata (best-effort: missing file is OK on first create).
	meta := make(map[string]map[string]any)
	if b, err := os.ReadFile(a.metaPath(name)); err == nil {
		if err := json.Unmarshal(b, &meta); err != nil {
			return nil, domain.Wrap(domain.CodeVectorDBError, 500,
				fmt.Errorf("vectordb: parse meta %s: %w", name, err))
		}
	}
	c := &localCollection{
		name:  name,
		dim:   dim,
		dist:  dist,
		graph: sg,
		meta:  meta,
	}
	a.collections[name] = c
	return c, nil
}

func init() {
	// coder/hnsw ships only Cosine + Euclidean distance. We register a
	// negated inner-product distance so the "ip" schema option works and so
	// Export/Import can resolve the func by name. RegisterDistanceFunc is
	// idempotent (overwrites the map entry).
	hnsw.RegisterDistanceFunc("innerproduct", innerProductDistance)
}

// innerProductDistance returns -dot(a,b) so that "smaller distance" in HNSW
// corresponds to "larger inner product" (i.e. closer).
func innerProductDistance(a, b []float32) float32 {
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return -dot
}

func pickDistance(dist string) hnsw.DistanceFunc {
	switch dist {
	case "", "cosine":
		return hnsw.CosineDistance
	case "l2":
		return hnsw.EuclideanDistance
	case "ip":
		return innerProductDistance
	}
	return hnsw.CosineDistance
}

// EnsureCollection creates the on-disk graph+meta files for schema.Name if
// absent. Calling it on an existing collection is a no-op (Dim/Distance are
// not re-checked against the existing files).
func (a *LocalAdapter) EnsureCollection(_ context.Context, schema CollectionSchema) error {
	if err := validateSchema(schema); err != nil {
		return err
	}
	dist := schema.Distance
	if dist == "" {
		dist = "cosine"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.loadCollectionLocked(schema.Name, schema.Dim, dist)
	if err != nil {
		return err
	}
	// Persist sidecar metadata so the file exists even before any Upsert.
	if err := a.persistMetaLocked(c); err != nil {
		return err
	}
	return nil
}

// DropCollection deletes the .bin and .meta.json files for name. Missing
// files are treated as success.
func (a *LocalAdapter) DropCollection(_ context.Context, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.collections, name)
	for _, p := range []string{a.graphPath(name), a.metaPath(name)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return domain.Wrap(domain.CodeVectorDBError, 500,
				fmt.Errorf("vectordb: remove %s: %w", p, err))
		}
	}
	return nil
}

// ListCollections scans path for *.bin files and returns their base names.
func (a *LocalAdapter) ListCollections(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(a.path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeVectorDBError, 500,
			fmt.Errorf("vectordb: list %s: %w", a.path, err))
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".bin") {
			out = append(out, strings.TrimSuffix(n, ".bin"))
		}
	}
	return out, nil
}

// Upsert inserts or replaces rows. The hnsw graph is mutated in memory and
// the sidecar metadata is appended; both are persisted via Save() before
// returning so that a crash never loses committed rows.
func (a *LocalAdapter) Upsert(_ context.Context, collection string, rows []Vector) error {
	if collection == "" {
		return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty collection name"))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.loadCollectionLocked(collection, 0, "cosine")
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g := c.graph.Graph
	for _, r := range rows {
		if r.ID == "" {
			return domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: row id must not be empty"))
		}
		if c.dim > 0 && len(r.Embedding) != c.dim {
			return domain.Wrap(domain.CodeValidationFailed, 422,
				errors.New("vectordb: embedding dim mismatch"))
		}
		if c.dim == 0 {
			c.dim = len(r.Embedding)
		}
		emb := append([]float32(nil), r.Embedding...)
		g.Add(hnsw.MakeNode(r.ID, emb))
		c.meta[r.ID] = cloneMetadata(r.Metadata)
	}
	c.dirty = true
	if err := a.persistLocked(c); err != nil {
		return err
	}
	return nil
}

// Delete removes rows by ID. coder/hnsw's Delete returns bool (whether the
// node existed); we treat false as success since callers may pass stale IDs.
func (a *LocalAdapter) Delete(_ context.Context, collection string, ids []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.collections[collection]
	if !ok {
		// Try to lazy-load so Delete works without prior EnsureCollection.
		c2, err := a.loadCollectionLocked(collection, 0, "cosine")
		if err != nil {
			// Missing on-disk graph = nothing to delete.
			return nil
		}
		c = c2
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g := c.graph.Graph
	for _, id := range ids {
		g.Delete(id)
		delete(c.meta, id)
	}
	c.dirty = true
	return a.persistLocked(c)
}

// Search returns the TopK rows nearest to Query, post-filtered by
// params.Filter. Because coder/hnsw cannot apply metadata filters natively,
// we over-fetch (k*8 + 32) and post-filter; if the post-filtered set is
// smaller than TopK we return what we have rather than recursing.
func (a *LocalAdapter) Search(_ context.Context, params SearchParams) (*SearchResult, error) {
	if len(params.Query) == 0 {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422, errors.New("vectordb: empty query vector"))
	}
	topK := params.TopK
	if topK <= 0 {
		topK = 10
	}
	a.mu.Lock()
	c, ok := a.collections[params.Collection]
	if !ok {
		c2, err := a.loadCollectionLocked(params.Collection, 0, "cosine")
		if err != nil {
			a.mu.Unlock()
			return &SearchResult{Hits: nil}, nil
		}
		c = c2
	}
	a.mu.Unlock()

	c.mu.RLock()
	defer c.mu.RUnlock()
	g := c.graph.Graph
	if c.dim > 0 && len(params.Query) != c.dim {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			errors.New("vectordb: query dim mismatch"))
	}
	// Over-fetch to compensate for post-filtering.
	k := topK*8 + 32
	nodes := g.Search(params.Query, k)
	hits := make([]Vector, 0, len(nodes))
	qNorm := l2Norm(params.Query)
	for _, n := range nodes {
		row := Vector{
			ID:        n.Key,
			Embedding: append([]float32(nil), n.Value...),
			Metadata:  cloneMetadata(c.meta[n.Key]),
		}
		if !params.Filter.Matches(row) {
			continue
		}
		row.Score = cosineSimilarity(params.Query, qNorm, n.Value, l2Norm(n.Value))
		hits = append(hits, row)
		if len(hits) >= topK {
			break
		}
	}
	// Re-sort by descending score because hnsw returns approximate order.
	sortByScoreDesc(hits)
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return &SearchResult{Hits: hits}, nil
}

// Get fetches a single row by ID. coder/hnsw does not expose a direct Get,
// so we look up metadata in the sidecar map; the embedding is reconstructed
// from a Search(k=1) against the row's own embedding if we have it, or
// omitted (nil) if the caller only needs metadata.
func (a *LocalAdapter) Get(_ context.Context, collection, id string) (*Vector, error) {
	a.mu.Lock()
	c, ok := a.collections[collection]
	if !ok {
		c2, err := a.loadCollectionLocked(collection, 0, "cosine")
		if err != nil {
			a.mu.Unlock()
			return nil, domain.Wrap(domain.CodeResourceNotFound, 404,
				errors.New("vectordb: collection not found"))
		}
		c = c2
	}
	a.mu.Unlock()
	c.mu.RLock()
	defer c.mu.RUnlock()
	meta, ok := c.meta[id]
	if !ok {
		return nil, domain.Wrap(domain.CodeResourceNotFound, 404,
			errors.New("vectordb: row not found"))
	}
	return &Vector{
		ID:        id,
		Embedding: nil, // not cheaply retrievable from hnsw
		Metadata:  cloneMetadata(meta),
	}, nil
}

// Count returns the number of rows in the collection (sourced from the
// sidecar metadata map, not the graph).
func (a *LocalAdapter) Count(_ context.Context, collection string) (int64, error) {
	a.mu.Lock()
	c, ok := a.collections[collection]
	if !ok {
		c2, err := a.loadCollectionLocked(collection, 0, "cosine")
		if err != nil {
			a.mu.Unlock()
			return 0, nil
		}
		c = c2
	}
	a.mu.Unlock()
	c.mu.RLock()
	defer c.mu.RUnlock()
	return int64(len(c.meta)), nil
}

// Close persists any dirty collections and releases them.
func (a *LocalAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var firstErr error
	for _, c := range a.collections {
		if !c.dirty {
			continue
		}
		if err := a.persistLocked(c); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	a.collections = nil
	return firstErr
}

// persistLocked writes the graph and metadata to disk. Caller must hold
// c.mu AND a.mu (the Save() call touches the file).
func (a *LocalAdapter) persistLocked(c *localCollection) error {
	if err := c.graph.Save(); err != nil {
		return domain.Wrap(domain.CodeVectorDBError, 500,
			fmt.Errorf("vectordb: save graph %s: %w", c.name, err))
	}
	return a.persistMetaLocked(c)
}

// persistMetaLocked writes the sidecar metadata JSON. Caller must hold c.mu.
func (a *LocalAdapter) persistMetaLocked(c *localCollection) error {
	b, err := json.Marshal(c.meta)
	if err != nil {
		return domain.Wrap(domain.CodeVectorDBError, 500,
			fmt.Errorf("vectordb: marshal meta %s: %w", c.name, err))
	}
	if err := os.WriteFile(a.metaPath(c.name), b, 0o644); err != nil {
		return domain.Wrap(domain.CodeVectorDBError, 500,
			fmt.Errorf("vectordb: write meta %s: %w", c.name, err))
	}
	return nil
}

// sortByScoreDesc sorts hits by descending Score (insertion sort; small N).
func sortByScoreDesc(hits []Vector) {
	for i := 1; i < len(hits); i++ {
		j := i
		for j > 0 && hits[j-1].Score < hits[j].Score {
			hits[j-1], hits[j] = hits[j], hits[j-1]
			j--
		}
	}
}
