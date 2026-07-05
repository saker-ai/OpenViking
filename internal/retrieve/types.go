// Package retrieve implements the hierarchical L0/L1/L2 retrieval pipeline
// described in docs/design/go-rewrite-design.md §7.6.
//
// The pipeline orchestrates:
//   - IntentAnalyzer (VLM-backed): rewrite query, generate subqueries, hint level
//   - HybridSearcher: dense via vectordb + sparse via ragfs.Grep
//   - L0 abstract -> L1 overview -> L2 chunks (the three-level funnel)
//   - Reranker (Cohere/Volcengine/local cosine)
//   - Memory lifecycle: hotness score weighting per resource
//   - Stats: latency / recall / level breakdown via OTel spans
//
// All external dependencies (VLM, embedder, reranker, vectordb adapter,
// ragfs.FileSystem, metrics) are injected via the HierarchicalRetriever
// constructor so tests can substitute stubs without touching the network.
package retrieve

import (
	"context"
	"time"

	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// Level enumerates the three retrieval layers.
type Level string

const (
	Level0Abstract Level = "L0" // abstract layer: dense+sparse hybrid across all abstracts
	Level1Overview Level = "L1" // overview layer: re-search within candidate dirs
	Level2Chunk    Level = "L2" // chunk layer: original chunk text in candidate resources
)

// Document is one retrieval hit. It is the retrieve-level analogue of
// rerank.Document, enriched with URI / Level / Layer metadata so callers
// can trace a hit back to its source resource.
type Document struct {
	URI      string         `json:"uri"`
	Content  string         `json:"content"`
	Score    float64        `json:"score"`
	Level    Level          `json:"level"`
	Kind     string         `json:"kind,omitempty"` // "abstract" | "overview" | "chunk"
	DirURI   string         `json:"dir_uri,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Hotness is populated by the memory lifecycle weighting pass and
	// reflects the resource's recent access frequency (0..1).
	Hotness float64 `json:"hotness,omitempty"`
}

// Intent is the output of IntentAnalyzer.Analyze. It carries the rewritten
// query, generated subqueries, and an optional level hint that short-
// circuits the funnel when the query is obviously abstract or detail.
type Intent struct {
	OriginalQuery  string   `json:"original_query"`
	RewrittenQuery string   `json:"rewritten_query"`
	Subqueries     []string `json:"subqueries,omitempty"`
	// LevelHint is "L0" | "L1" | "L2" or empty when the analyzer defers
	// to the default full-funnel flow.
	LevelHint Level `json:"level_hint,omitempty"`
	// Reasoning is the analyzer's explanation; surfaced in stats spans.
	Reasoning string `json:"reasoning,omitempty"`
}

// RetrieveRequest is the input to Retriever.Retrieve.
type RetrieveRequest struct {
	Account string `json:"account"`
	Query   string `json:"query"`
	// TopK is the final number of documents to return.
	TopK int `json:"top_k"`
	// Level, when non-empty, short-circuits the funnel to a single layer.
	Level Level `json:"level,omitempty"`
	// URIPrefix restricts candidates to resources whose URI starts with it.
	URIPrefix string `json:"uri_prefix,omitempty"`
	// Metadata applies an exact-match filter on each hit's metadata.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// RetrieveResponse is the output of Retriever.Retrieve.
type RetrieveResponse struct {
	Query     string     `json:"query"`
	Intent    *Intent    `json:"intent,omitempty"`
	Documents []Document `json:"documents"`
	Stats     *Stats     `json:"stats,omitempty"`
}

// HybridSearchRequest is the input to HybridSearcher.HybridSearch.
//
// Query is the original (non-embedded) text; the searcher is responsible
// for calling the embedder to produce a dense vector. SparseTerms is the
// pre-tokenized list of terms to feed to ragfs.Grep; when empty, the
// searcher tokenizes Query on whitespace.
type HybridSearchRequest struct {
	Account     string
	Collection  string // vectordb collection name (tenant-scoped)
	Query       string
	SparseTerms []string
	TopK        int
	Filter      vectordb.Filter
	// GrepRoot is the ragfs path to anchor sparse search at (e.g.
	// "/accounts/acme/resources"). When empty, sparse search is skipped.
	GrepRoot string
}

// SearchResult re-exports vectordb.SearchResult under a retrieve-friendly
// name. The retrieve package never imports vectordb.SearchResult directly
// in its public API to keep the HybridSearcher signature decoupled from
// vectordb internals; callers convert at the boundary.
type SearchResult = vectordb.SearchResult

// Retriever is the top-level interface implemented by HierarchicalRetriever.
type Retriever interface {
	Retrieve(ctx context.Context, req RetrieveRequest) (*RetrieveResponse, error)
}

// HybridSearcher wraps dense (vectordb) and sparse (ragfs.Grep) search.
//
// Design note (per design §7.6.3): for hybrid dense+sparse, we do dense
// via vectordb.CollectionAdapter.Search and sparse via ragfs.Grep
// (BM25-ish — Grep returns regex matches with line numbers, which we
// score as term frequency). The two result lists are merged via
// Reciprocal Rank Fusion (RRF) with a configurable k constant.
type HybridSearcher interface {
	HybridSearch(ctx context.Context, req HybridSearchRequest) (*SearchResult, error)
}

// Reranker is the retrieve-level rerank interface. It mirrors
// internal/models/rerank.Reranker but operates on retrieve.Document
// instead of rerank.Document so the retrieve pipeline does not depend on
// the models package's wire types.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []Document, topN int) ([]Document, error)
}

// IntentAnalyzer rewrites the query and emits a level hint.
type IntentAnalyzer interface {
	Analyze(ctx context.Context, query string) (*Intent, error)
}

// Stats summarizes one Retrieve call for telemetry.
type Stats struct {
	Latency        time.Duration           `json:"latency"`
	LatencyByLevel map[Level]time.Duration `json:"latency_by_level,omitempty"`
	HitsByLevel    map[Level]int           `json:"hits_by_level,omitempty"`
	// RecallEstimate is a coarse recall proxy: # unique candidate URIs
	// after L2 / # candidate URIs after L0. Zero when the funnel does
	// not progress past L0.
	RecallEstimate float64 `json:"recall_estimate,omitempty"`
}
