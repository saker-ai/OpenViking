// Package retrieve — default HybridSearcher implementation.
//
// DefaultHybridSearcher wraps a vectordb.CollectionAdapter (dense) and a
// ragfs.FileSystem (sparse via Grep). The two result lists are merged
// using Reciprocal Rank Fusion (RRF) with k=60, the standard constant
// from the TREC community.
//
// Design note (per design §7.6.3): vectordb.Search returns dense hits
// ranked by cosine similarity; ragfs.Grep returns sparse regex matches
// with line numbers but no score. We approximate BM25 by counting term
// frequency per matched file (a file with N matching lines scores
// 1 - 1/(1+N), bounded in [0, 1)). The two lists are then fused by RRF
// to produce a single ranked hit list.
//
// Tests use a vectordb.MemoryAdapter and a ragfs memory backend; no
// network calls.
package retrieve

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/models/embedder"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// DefaultHybridSearcher is the standard HybridSearcher wired into
// HierarchicalRetriever. It expects an embedder to convert the query
// text into a dense vector for vectordb.Search.
type DefaultHybridSearcher struct {
	VectorDB vectordb.CollectionAdapter
	Embedder embedder.Embedder
	FS       ragfs.FileSystem
}

// NewDefaultHybridSearcher constructs a searcher. embedder and vdb must
// be non-nil; fs may be nil (in which case sparse search is skipped).
func NewDefaultHybridSearcher(vdb vectordb.CollectionAdapter, embed embedder.Embedder, fs ragfs.FileSystem) *DefaultHybridSearcher {
	return &DefaultHybridSearcher{VectorDB: vdb, Embedder: embed, FS: fs}
}

// HybridSearch implements HybridSearcher.
func (s *DefaultHybridSearcher) HybridSearch(ctx context.Context, req HybridSearchRequest) (*SearchResult, error) {
	if s == nil || s.VectorDB == nil {
		return nil, domain.Wrap(domain.CodeVectorDBError, 500,
			fmt.Errorf("retrieve: nil vectordb adapter"))
	}
	if req.Query == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("retrieve: query is required"))
	}

	// Dense leg.
	denseHits, err := s.denseSearch(ctx, req)
	if err != nil {
		return nil, err
	}

	// Sparse leg (optional).
	sparseHits, err := s.sparseSearch(ctx, req)
	if err != nil {
		// Sparse failure is non-fatal; degrade to dense-only.
		sparseHits = nil
	}

	merged := fuseRRF(denseHits, sparseHits, req.TopK)
	return &SearchResult{Hits: merged}, nil
}

// denseSearch embeds the query and calls vectordb.Search.
func (s *DefaultHybridSearcher) denseSearch(ctx context.Context, req HybridSearchRequest) ([]vectordb.Vector, error) {
	if s.Embedder == nil {
		return nil, nil
	}
	vecs, err := s.Embedder.Embed(ctx, []string{req.Query}, "")
	if err != nil {
		return nil, fmt.Errorf("retrieve: embed query: %w", err)
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	res, err := s.VectorDB.Search(ctx, vectordb.SearchParams{
		Collection: req.Collection,
		Query:      vecs[0],
		TopK:       req.TopK,
		Filter:     req.Filter,
	})
	if err != nil {
		return nil, fmt.Errorf("retrieve: vectordb search: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	return res.Hits, nil
}

// sparseSearch runs ragfs.Grep with a per-term alternation pattern and
// scores files by term-frequency saturation.
func (s *DefaultHybridSearcher) sparseSearch(ctx context.Context, req HybridSearchRequest) ([]vectordb.Vector, error) {
	if s.FS == nil || req.GrepRoot == "" {
		return nil, nil
	}
	terms := req.SparseTerms
	if len(terms) == 0 {
		terms = tokenize(req.Query)
	}
	if len(terms) == 0 {
		return nil, nil
	}
	pattern := "(?i)" + strings.Join(escapeTerms(terms), "|")
	re := regexp.MustCompile(pattern) // safe: pattern is constructed from escaped terms

	matches, err := s.FS.Grep(ctx, pattern, req.GrepRoot, true)
	if err != nil {
		return nil, fmt.Errorf("retrieve: grep: %w", err)
	}
	// Count matches per file path.
	tfByPath := map[string]int{}
	for _, m := range matches {
		// Grep returns full paths; collapse line-level matches by file.
		tfByPath[m.Path]++
	}
	if len(tfByPath) == 0 {
		return nil, nil
	}

	// Build a synthetic dense-vector-shaped hit from each matched file.
	// We can't reconstruct the original embedding, so we set Score to the
	// term-frequency saturation and leave Embedding nil — RRF only uses
	// rank, not the raw score.
	out := make([]vectordb.Vector, 0, len(tfByPath))
	for path, n := range tfByPath {
		score := float32(1.0 - 1.0/(1.0+float64(n)))
		out = append(out, vectordb.Vector{
			ID:    path,
			Score: score,
			Metadata: map[string]any{
				"uri":     path,
				"kind":    req.Filter.Kind,
				"account": req.Filter.Account,
				"content": firstMatchLine(matches, path, re),
			},
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// fuseRRF merges dense and sparse hits by Reciprocal Rank Fusion.
// k=60 is the standard constant. Hits are deduplicated by ID; the
// highest-scoring copy wins.
func fuseRRF(dense, sparse []vectordb.Vector, topK int) []vectordb.Vector {
	scores := map[string]float32{}
	byID := map[string]vectordb.Vector{}

	for rank, h := range dense {
		w := 1.0 / (RRFConstant + float64(rank+1))
		scores[h.ID] += float32(w)
		byID[h.ID] = mergeHit(byID[h.ID], h)
	}
	for rank, h := range sparse {
		w := 1.0 / (RRFConstant + float64(rank+1))
		scores[h.ID] += float32(w)
		byID[h.ID] = mergeHit(byID[h.ID], h)
	}

	out := make([]vectordb.Vector, 0, len(byID))
	for id, h := range byID {
		h.Score = scores[id]
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if topK > 0 && topK < len(out) {
		out = out[:topK]
	}
	return out
}

// mergeHit copies non-zero fields from src into dst (dst wins for
// conflicting fields). Used by fuseRRF to preserve metadata across the
// dense and sparse legs.
func mergeHit(dst, src vectordb.Vector) vectordb.Vector {
	if dst.ID == "" {
		dst.ID = src.ID
	}
	if len(dst.Embedding) == 0 {
		dst.Embedding = src.Embedding
	}
	if dst.Metadata == nil && src.Metadata != nil {
		dst.Metadata = map[string]any{}
	}
	for k, v := range src.Metadata {
		if _, ok := dst.Metadata[k]; !ok {
			dst.Metadata[k] = v
		}
	}
	return dst
}

// tokenize splits s on whitespace and punctuation, returning lowercase
// tokens of length >= 2. Stop-words are NOT filtered here; the regex
// alternation handles relevance via term frequency.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	out := []string{}
	current := strings.Builder{}
	flush := func() {
		if current.Len() >= 2 {
			out = append(out, current.String())
		}
		current.Reset()
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			current.WriteRune(r)
		case r >= '0' && r <= '9':
			current.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// escapeTerms escapes each token for use in a Go regexp alternation.
func escapeTerms(terms []string) []string {
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = regexp.QuoteMeta(t)
	}
	return out
}

// firstMatchLine returns the line of the first match at the given path,
// for use as the synthetic hit's "content" metadata.
func firstMatchLine(matches []ragfs.GrepMatch, path string, re *regexp.Regexp) string {
	for _, m := range matches {
		if m.Path != path {
			continue
		}
		return m.Line
	}
	return ""
}

// Compile-time assertion.
var _ HybridSearcher = (*DefaultHybridSearcher)(nil)
