package vectordb

import "context"

// Vector is a single record stored in a vector collection.
//
// ID is unique within a collection. Embedding is the dense float32 vector.
// Metadata carries arbitrary key-value pairs (account, kind, uri, plus any
// user-supplied fields) used for filtering and surfaced back to callers.
//
// Score is populated by Search to report similarity to the query; backends
// must leave it zero for Upsert payloads and overwrite it on read paths.
type Vector struct {
	ID        string         `json:"id"`
	Embedding []float32      `json:"embedding"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Score     float32        `json:"score,omitempty"`
}

// Filter restricts Search results by metadata. The zero Filter matches all
// rows. Conditions are AND-combined: a row must satisfy every non-zero
// field to be returned.
//
// Account and Kind are exact matches on the corresponding metadata keys
// ("account", "kind"). URIPrefix is a string-prefix match on the "uri"
// metadata key. Metadata is an exact key=value match for every entry.
type Filter struct {
	Account   string         `json:"account,omitempty"`
	Kind      string         `json:"kind,omitempty"`
	URIPrefix string         `json:"uri_prefix,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Matches reports whether v satisfies f. Backends with no native filter
// support (memory, local hnsw) use this to post-filter Search results.
func (f Filter) Matches(v Vector) bool {
	if f.Account != "" {
		if s, _ := v.Metadata["account"].(string); s != f.Account {
			return false
		}
	}
	if f.Kind != "" {
		if s, _ := v.Metadata["kind"].(string); s != f.Kind {
			return false
		}
	}
	if f.URIPrefix != "" {
		s, _ := v.Metadata["uri"].(string)
		if !startsWith(s, f.URIPrefix) {
			return false
		}
	}
	for k, want := range f.Metadata {
		got, ok := v.Metadata[k]
		if !ok || !equalAny(got, want) {
			return false
		}
	}
	return true
}

func startsWith(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return s[:len(prefix)] == prefix
}

// equalAny compares two arbitrary values for Filter.Metadata matching.
// It intentionally only handles the JSON-friendly scalar types that
// metadata may carry (string, bool, float64, json.Number-like ints).
func equalAny(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case float32:
		bv, ok := b.(float64)
		return ok && float64(av) == bv
	case int:
		switch bv := b.(type) {
		case int:
			return av == bv
		case int64:
			return int64(av) == bv
		case float64:
			return float64(av) == bv
		}
	case int64:
		switch bv := b.(type) {
		case int64:
			return av == bv
		case int:
			return av == int64(bv)
		case float64:
			return float64(av) == bv
		}
	}
	return false
}

// SearchParams is the input to CollectionAdapter.Search.
//
// Collection is the tenant-scoped name returned by CollectionName. Query is
// the dense embedding to search against. TopK bounds the result size; backends
// may return fewer hits if fewer match. Filter, if non-zero, restricts the
// candidate set before TopK is applied.
type SearchParams struct {
	Collection string
	Query      []float32
	TopK       int
	Filter     Filter
}

// SearchResult is the output of CollectionAdapter.Search. Hits are ordered
// by descending similarity (best first).
type SearchResult struct {
	Hits []Vector `json:"hits"`
}

// CollectionSchema describes a collection to be created via EnsureCollection.
type CollectionSchema struct {
	Name     string // tenant-scoped collection name
	Dim      int    // embedding dimension
	Distance string // "cosine" (default), "l2", or "ip" (inner product)
}

// CollectionState is the snapshot returned by GetState (currently exposed via
// Count; reserved for richer introspection in later phases).
type CollectionState struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
	Dim   int    `json:"dim"`
}

// CollectionAdapter is the backend-agnostic interface implemented by every
// vectordb backend (memory, local, qdrant). No backend-specific types appear
// in the signatures so callers can switch backends via config without code
// changes.
//
// All methods are safe for concurrent use. ctx is propagated to remote
// backends (qdrant) and respected by local backends on best-effort basis.
type CollectionAdapter interface {
	// EnsureCollection creates the named collection if absent. Calling it on
	// an existing collection with the same Dim/Distance is a no-op.
	EnsureCollection(ctx context.Context, schema CollectionSchema) error

	// DropCollection removes the named collection and all of its rows.
	// Missing collections are treated as success.
	DropCollection(ctx context.Context, name string) error

	// ListCollections returns the names of all collections visible to this
	// adapter (tenant-scoped names included).
	ListCollections(ctx context.Context) ([]string, error)

	// Upsert inserts or replaces rows by ID. A row whose ID already exists
	// is fully replaced (embedding + metadata).
	Upsert(ctx context.Context, collection string, rows []Vector) error

	// Delete removes the rows with the given IDs. Unknown IDs are ignored.
	Delete(ctx context.Context, collection string, ids []string) error

	// Search returns the TopK rows nearest to Query (filtered by Filter).
	// Hits are ordered best-first; Score is the similarity in [-1, 1] for
	// cosine, or backend-defined for other distances.
	Search(ctx context.Context, params SearchParams) (*SearchResult, error)

	// Get fetches a single row by ID. Returns an error wrapping
	// domain.ErrNotFound when the row does not exist.
	Get(ctx context.Context, collection, id string) (*Vector, error)

	// Count returns the number of rows in the collection.
	Count(ctx context.Context, collection string) (int64, error)

	// Close releases any backend resources (files, gRPC conn, etc.).
	Close() error
}

// Migrator is an optional interface implemented by backends that need to
// run schema-level migrations beyond EnsureCollection (e.g. adding index
// hints, altering column types). Backends that don't implement Migrator
// fall back to EnsureCollection for migration: the migrate CLI treats
// EnsureCollection as the canonical "ensure current schema" step.
//
// The local backend (coder/hnsw) intentionally returns a no-op from
// Migrate because its on-disk format is self-versioning via the hnsw
// SavedGraph format.
type Migrator interface {
	Migrate(ctx context.Context, schema CollectionSchema) error
}
