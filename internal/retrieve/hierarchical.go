// Package retrieve — hierarchical L0/L1/L2 retriever.
//
// The HierarchicalRetriever orchestrates the funnel described in design
// §7.6.2:
//
//	IntentAnalyzer.Analyze(query) -> Intent{rewrite, subqueries, level_hint}
//	  |
//	  v
//	[L0 abstract]  HybridSearch across *.abstract (dense via vectordb +
//	               sparse via ragfs.Grep, merged by RRF)
//	  | resolve candidate dirs (parents of L0 hits)
//	  v
//	[L1 overview]  HybridSearch within candidate dirs' *.overview
//	  | resolve candidate resources (URIs whose overview matched)
//	  v
//	[L2 chunk]     HybridSearch within candidate resources' *.chunk
//	  |
//	  v
//	Rerank(query, all L2 hits, topN)
//	  |
//	  v
//	MemoryLifecycle.Apply(hotness weighting)
//	  |
//	  v
//	return Documents + Stats span
//
// The funnel can be short-circuited by Intent.LevelHint or by the
// caller's RetrieveRequest.Level (e.g. Level=L1 skips L0 and L2).
//
// All external dependencies are injected via the constructor; tests use
// stubs and never hit the network.
package retrieve

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/models/embedder"
	"github.com/saker-ai/ctxhub/internal/models/rerank"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// RRFConstant is the Reciprocal Rank Fusion constant (k = 60 is standard).
const RRFConstant = 60.0

// CollectionKind is the vectordb collection kind suffix for each layer.
const (
	KindAbstract = "abstract"
	KindOverview = "overview"
	KindChunk    = "chunk"
)

// HierarchicalRetriever orchestrates the L0/L1/L2 funnel.
type HierarchicalRetriever struct {
	Intent   IntentAnalyzer
	Searcher HybridSearcher
	Reranker Reranker
	Memory   *MemoryLifecycle
	Embedder embedder.Embedder
	VectorDB vectordb.CollectionAdapter
	FS       ragfs.FileSystem
	Stats    *StatsCollector
	Prefix   string // vectordb collection prefix, e.g. "ov_"
	// DefaultTopK when req.TopK <= 0.
	DefaultTopK int
}

// NewHierarchicalRetriever constructs a retriever with the given deps.
// Optional deps (Memory, Stats) may be nil; the retriever degrades
// gracefully to a no-op for those layers.
func NewHierarchicalRetriever(
	intent IntentAnalyzer,
	searcher HybridSearcher,
	reranker Reranker,
	embed embedder.Embedder,
	vdb vectordb.CollectionAdapter,
	fs ragfs.FileSystem,
	prefix string,
) *HierarchicalRetriever {
	return &HierarchicalRetriever{
		Intent:      intent,
		Searcher:    searcher,
		Reranker:    reranker,
		Embedder:    embed,
		VectorDB:    vdb,
		FS:          fs,
		Prefix:      prefix,
		Stats:       NewStatsCollector(),
		DefaultTopK: 10,
	}
}

// Retrieve implements Retriever.
func (r *HierarchicalRetriever) Retrieve(ctx context.Context, req RetrieveRequest) (*RetrieveResponse, error) {
	if r == nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("retrieve: nil receiver"))
	}
	if req.Query == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("retrieve: query is required"))
	}
	if req.Account == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("retrieve: account is required"))
	}
	topK := req.TopK
	if topK <= 0 {
		topK = r.DefaultTopK
	}

	agg := NewAggregator(time.Now())
	defer func() { _ = agg.Stats(time.Now()) }()

	// Stage 1: intent analysis.
	ctx, endSpan := r.Stats.StartSpan(ctx, "retrieve.intent",
		attribute.String("retrieve.account", req.Account))
	intent, err := r.Intent.Analyze(ctx, req.Query)
	endSpan(err)
	if err != nil {
		return nil, err
	}
	query := req.Query
	if intent != nil && intent.RewrittenQuery != "" {
		query = intent.RewrittenQuery
	}

	// Stage 2: L0 (unless short-circuited).
	level := req.Level
	if level == "" && intent != nil {
		level = intent.LevelHint
	}

	var allDocs []Document
	candidateDirs := map[string]struct{}{}
	candidateResources := map[string]struct{}{}

	l0Start := time.Now()
	if level == "" || level == Level0Abstract {
		l0Docs, err := r.runLevel(ctx, Level0Abstract, KindAbstract, req, query)
		if err != nil {
			return nil, err
		}
		agg.RecordLevel(Level0Abstract, time.Since(l0Start), len(l0Docs))
		allDocs = append(allDocs, l0Docs...)
		// Resolve candidate dirs from L0 hits.
		for _, d := range l0Docs {
			if d.DirURI != "" {
				candidateDirs[d.DirURI] = struct{}{}
			} else if d.URI != "" {
				candidateDirs[parentDir(d.URI)] = struct{}{}
			}
		}
	}

	// Stage 3: L1 (unless short-circuited).
	l1Start := time.Now()
	if level == "" || level == Level1Overview {
		if len(candidateDirs) == 0 && level == Level1Overview {
			// Caller forced L1 but no L0 was run; search globally.
			candidateDirs[""] = struct{}{}
		}
		l1Docs, err := r.runLevelFiltered(ctx, Level1Overview, KindOverview, req, query, candidateDirs)
		if err != nil {
			return nil, err
		}
		agg.RecordLevel(Level1Overview, time.Since(l1Start), len(l1Docs))
		allDocs = append(allDocs, l1Docs...)
		// Resolve candidate resources from L1 hits (the URIs whose
		// overview matched become the L2 search set).
		for _, d := range l1Docs {
			if d.URI != "" {
				candidateResources[d.URI] = struct{}{}
			}
		}
		// Also seed candidate resources from L0 hits when L1 produced none.
		// This keeps the funnel from collapsing on a fresh index where L1
		// overviews have not yet been generated.
		if len(candidateResources) == 0 {
			for _, d := range allDocs {
				if d.URI != "" {
					candidateResources[d.URI] = struct{}{}
				}
			}
		}
	}

	// Stage 4: L2 (unless short-circuited).
	l2Start := time.Now()
	if level == "" || level == Level2Chunk {
		if len(candidateResources) == 0 && level == Level2Chunk {
			// Caller forced L2; use L0 candidate dirs as the resource set.
			for d := range candidateDirs {
				candidateResources[d] = struct{}{}
			}
		}
		l2Docs, err := r.runLevelFiltered(ctx, Level2Chunk, KindChunk, req, query, candidateResources)
		if err != nil {
			return nil, err
		}
		agg.RecordLevel(Level2Chunk, time.Since(l2Start), len(l2Docs))
		allDocs = append(allDocs, l2Docs...)
	}

	// Deduplicate by URI (keeping the best score per URI across levels).
	allDocs = dedupAndSort(allDocs)

	// Stage 5: rerank.
	reranked := allDocs
	if r.Reranker != nil && len(allDocs) > 0 {
		ctx, endSpan := r.Stats.StartSpan(ctx, "retrieve.rerank")
		out, err := r.Reranker.Rerank(ctx, query, allDocs, topK)
		endSpan(err)
		if err != nil {
			return nil, err
		}
		reranked = out
	}

	// Stage 6: memory lifecycle weighting.
	now := time.Now()
	if r.Memory != nil {
		var err error
		reranked, err = r.Memory.Apply(ctx, req.Account, reranked, now)
		if err != nil {
			return nil, err
		}
		// Re-sort by blended score.
		sort.SliceStable(reranked, func(i, j int) bool {
			return reranked[i].Score > reranked[j].Score
		})
		if len(reranked) > topK {
			reranked = reranked[:topK]
		}
		_ = r.Memory.RecordAccess(ctx, req.Account, reranked, now)
	}

	agg.RecordCandidates(len(allDocs), len(reranked))
	stats := agg.Stats(time.Now())
	return &RetrieveResponse{
		Query:     req.Query,
		Intent:    intent,
		Documents: reranked,
		Stats:     stats,
	}, nil
}

// runLevel executes hybrid search at one level for the given kind
// (abstract/overview/chunk) and returns the hits.
func (r *HierarchicalRetriever) runLevel(
	ctx context.Context,
	level Level, kind string,
	req RetrieveRequest, query string,
) ([]Document, error) {
	return r.searchLayer(ctx, level, kind, req, query, "")
}

// runLevelFiltered is like runLevel but restricts the search to URIs that
// start with one of the candidate prefixes (directories for L1, resources
// for L2). When candidates is empty or contains only "", the search is
// unrestricted (the funnel's L1 fallback when L0 produced no dirs).
func (r *HierarchicalRetriever) runLevelFiltered(
	ctx context.Context,
	level Level, kind string,
	req RetrieveRequest, query string,
	candidates map[string]struct{},
) ([]Document, error) {
	if len(candidates) == 0 {
		return r.searchLayer(ctx, level, kind, req, query, "")
	}
	var all []Document
	for prefix := range candidates {
		docs, err := r.searchLayer(ctx, level, kind, req, query, prefix)
		if err != nil {
			return nil, err
		}
		all = append(all, docs...)
	}
	return all, nil
}

// searchLayer performs a single hybrid search call against the vectordb
// collection for (account, kind) and converts hits to retrieve.Document.
func (r *HierarchicalRetriever) searchLayer(
	ctx context.Context,
	level Level, kind string,
	req RetrieveRequest, query, uriPrefix string,
) ([]Document, error) {
	collection, err := vectordb.CollectionName(r.Prefix, req.Account, kind)
	if err != nil {
		return nil, err
	}
	searchReq := HybridSearchRequest{
		Account:    req.Account,
		Collection: collection,
		Query:      query,
		TopK:       r.DefaultTopK * 2, // over-fetch; rerank trims
		Filter: vectordb.Filter{
			Account:   req.Account,
			Kind:      kind,
			URIPrefix: uriPrefix,
			Metadata:  req.Metadata,
		},
	}
	if r.FS != nil {
		searchReq.GrepRoot = "/accounts/" + req.Account
	}
	res, err := r.Searcher.HybridSearch(ctx, searchReq)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	out := make([]Document, 0, len(res.Hits))
	for _, h := range res.Hits {
		uri, _ := h.Metadata["uri"].(string)
		dirURI := parentDir(uri)
		content, _ := h.Metadata["content"].(string)
		out = append(out, Document{
			URI:      uri,
			Content:  content,
			Score:    float64(h.Score),
			Level:    level,
			Kind:     kind,
			DirURI:   dirURI,
			Metadata: h.Metadata,
		})
	}
	return out, nil
}

// dedupAndSort keeps the best-scoring Document per URI and returns the
// result sorted by descending score.
func dedupAndSort(docs []Document) []Document {
	byURI := make(map[string]Document, len(docs))
	for _, d := range docs {
		if d.URI == "" {
			continue
		}
		ex, ok := byURI[d.URI]
		if !ok || d.Score > ex.Score {
			byURI[d.URI] = d
		}
	}
	out := make([]Document, 0, len(byURI))
	for _, d := range byURI {
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}

// parentDir returns the parent directory URI of a viking:// URI.
// e.g. "viking://acme/file/docs/x.txt" -> "viking://acme/file/docs".
// "viking://acme" (no path beyond authority) returns itself.
func parentDir(uri string) string {
	if uri == "" {
		return ""
	}
	// Locate the scheme://authority boundary so we never strip into it.
	schemeEnd := -1
	if i := strings.Index(uri, "://"); i >= 0 {
		rest := uri[i+3:]
		authSlash := strings.IndexByte(rest, '/')
		if authSlash < 0 {
			// No path beyond authority: nothing to strip.
			return uri
		}
		schemeEnd = i + 3 + authSlash
	}
	idx := strings.LastIndexByte(uri, '/')
	if idx <= 0 || idx <= schemeEnd {
		return uri
	}
	return uri[:idx]
}

// Compile-time assertion.
var _ Retriever = (*HierarchicalRetriever)(nil)

// AdapterReranker wraps a models/rerank.Reranker, converting between
// retrieve.Document and rerank.Document at the boundary. Use it to plug a
// real rerank provider into the retriever without leaking the models
// package's wire types into the retrieve pipeline.
type AdapterReranker struct {
	Inner rerank.Reranker
}

// NewAdapterReranker wraps r as a retrieve.Reranker.
func NewAdapterReranker(r rerank.Reranker) *AdapterReranker { return &AdapterReranker{Inner: r} }

// Rerank implements retrieve.Reranker.
func (a *AdapterReranker) Rerank(ctx context.Context, query string, docs []Document, topN int) ([]Document, error) {
	if a == nil || a.Inner == nil {
		return docs, nil
	}
	rd := make([]rerank.Document, len(docs))
	for i, d := range docs {
		rd[i] = rerank.Document{
			ID:       d.URI,
			Content:  d.Content,
			Metadata: d.Metadata,
			Score:    d.Score,
		}
	}
	out, err := a.Inner.Rerank(ctx, query, rd, topN)
	if err != nil {
		return nil, err
	}
	res := make([]Document, 0, len(out))
	for _, d := range out {
		// Find the original retrieve.Document by URI to preserve level/dir.
		var orig Document
		for _, o := range docs {
			if o.URI == d.ID {
				orig = o
				break
			}
		}
		orig.Score = d.Score
		res = append(res, orig)
	}
	return res, nil
}

// Compile-time assertion.
var _ Reranker = (*AdapterReranker)(nil)
