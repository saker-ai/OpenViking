package retrieve

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/models/embedder"
	"github.com/saker-ai/ctxhub/internal/models/rerank"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// stubIntent is a test-only IntentAnalyzer.
type stubIntent struct {
	out *Intent
	err error
}

func (s *stubIntent) Analyze(_ context.Context, q string) (*Intent, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.out != nil {
		return s.out, nil
	}
	return &Intent{OriginalQuery: q, RewrittenQuery: q}, nil
}

var _ IntentAnalyzer = (*stubIntent)(nil)

// stubSearcher is a test-only HybridSearcher.
type stubSearcher struct {
	hitsByCollection map[string][]vectordb.Vector
	err              error
	calls            []HybridSearchRequest
}

func (s *stubSearcher) HybridSearch(_ context.Context, req HybridSearchRequest) (*SearchResult, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	hits := s.hitsByCollection[req.Collection]
	out := make([]vectordb.Vector, len(hits))
	copy(out, hits)
	return &SearchResult{Hits: out}, nil
}

var _ HybridSearcher = (*stubSearcher)(nil)

// stubReranker is a test-only Reranker.
type stubReranker struct {
	fn func(ctx context.Context, query string, docs []Document, topN int) ([]Document, error)
}

func (s *stubReranker) Rerank(ctx context.Context, q string, d []Document, n int) ([]Document, error) {
	if s.fn == nil {
		return d, nil
	}
	return s.fn(ctx, q, d, n)
}

var _ Reranker = (*stubReranker)(nil)

func newTestRetriever(t *testing.T) (*HierarchicalRetriever, *stubSearcher, *stubReranker) {
	t.Helper()
	embed := embedder.NewLocal(config.EmbedderConfig{Dim: 8}, nil)
	vdb := vectordb.NewMemoryAdapter()
	searcher := &stubSearcher{hitsByCollection: map[string][]vectordb.Vector{}}
	rer := &stubReranker{}
	r := NewHierarchicalRetriever(&stubIntent{}, searcher, rer, embed, vdb, nil, "ov_")
	r.DefaultTopK = 5
	return r, searcher, rer
}

func TestRetrieveValidatesQuery(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRetriever(t)
	_, err := r.Retrieve(context.Background(), RetrieveRequest{Account: "a"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation))
}

func TestRetrieveValidatesAccount(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRetriever(t)
	_, err := r.Retrieve(context.Background(), RetrieveRequest{Query: "q"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation))
}

func TestRetrieveEmptyFunnel(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRetriever(t)
	resp, err := r.Retrieve(context.Background(), RetrieveRequest{
		Account: "acme",
		Query:   "what is rag",
		TopK:    3,
	})
	require.NoError(t, err)
	assert.Equal(t, "what is rag", resp.Query)
	assert.Empty(t, resp.Documents)
	require.NotNil(t, resp.Stats)
	assert.NotZero(t, resp.Stats.Latency)
}

func TestRetrieveL0Only(t *testing.T) {
	t.Parallel()
	r, searcher, _ := newTestRetriever(t)
	absCol, err := vectordb.CollectionName("ov_", "acme", KindAbstract)
	require.NoError(t, err)
	searcher.hitsByCollection[absCol] = []vectordb.Vector{
		{ID: "abs1", Score: 0.9, Metadata: map[string]any{
			"uri": "viking://acme/file/docs/x.txt",
		}},
		{ID: "abs2", Score: 0.6, Metadata: map[string]any{
			"uri": "viking://acme/file/docs/y.txt",
		}},
	}
	resp, err := r.Retrieve(context.Background(), RetrieveRequest{
		Account: "acme",
		Query:   "rag",
		TopK:    2,
	})
	require.NoError(t, err)
	// L0 ran, but L1/L2 had no candidates in stubs -> only L0 docs returned.
	require.NotEmpty(t, resp.Documents)
	assert.Equal(t, "viking://acme/file/docs/x.txt", resp.Documents[0].URI)
	assert.Equal(t, Level0Abstract, resp.Documents[0].Level)
	assert.Equal(t, KindAbstract, resp.Documents[0].Kind)
}

func TestRetrieveLevelHintShortCircuits(t *testing.T) {
	t.Parallel()
	r, searcher, _ := newTestRetriever(t)
	absCol, _ := vectordb.CollectionName("ov_", "acme", KindAbstract)
	ovCol, _ := vectordb.CollectionName("ov_", "acme", KindOverview)
	searcher.hitsByCollection[absCol] = []vectordb.Vector{{
		ID: "abs1", Score: 0.9, Metadata: map[string]any{"uri": "viking://acme/file/docs/x.txt"},
	}}
	searcher.hitsByCollection[ovCol] = []vectordb.Vector{{
		ID: "ov1", Score: 0.95, Metadata: map[string]any{"uri": "viking://acme/file/docs/x.txt"},
	}}

	r.Intent = &stubIntent{out: &Intent{
		OriginalQuery: "rag", RewrittenQuery: "rag", LevelHint: Level1Overview,
	}}
	resp, err := r.Retrieve(context.Background(), RetrieveRequest{
		Account: "acme", Query: "rag", TopK: 1,
	})
	require.NoError(t, err)
	// L1 was the only level that ran (L0 skipped due to hint).
	require.Len(t, resp.Documents, 1)
	assert.Equal(t, Level1Overview, resp.Documents[0].Level)
	// Confirm L0 collection was never queried.
	var sawAbs bool
	for _, c := range searcher.calls {
		if c.Collection == absCol {
			sawAbs = true
		}
	}
	assert.False(t, sawAbs, "L0 should have been skipped due to level hint")
}

func TestRetrieveCallerLevelShortCircuits(t *testing.T) {
	t.Parallel()
	r, searcher, _ := newTestRetriever(t)
	absCol, _ := vectordb.CollectionName("ov_", "acme", KindAbstract)
	chunkCol, _ := vectordb.CollectionName("ov_", "acme", KindChunk)
	searcher.hitsByCollection[absCol] = []vectordb.Vector{{
		ID: "abs1", Score: 0.99, Metadata: map[string]any{"uri": "viking://acme/file/x.txt"},
	}}
	searcher.hitsByCollection[chunkCol] = []vectordb.Vector{{
		ID: "ch1", Score: 0.5, Metadata: map[string]any{"uri": "viking://acme/file/x.txt"},
	}}

	resp, err := r.Retrieve(context.Background(), RetrieveRequest{
		Account: "acme", Query: "rag", TopK: 1, Level: Level2Chunk,
	})
	require.NoError(t, err)
	require.Len(t, resp.Documents, 1)
	assert.Equal(t, Level2Chunk, resp.Documents[0].Level)
	// L0 not queried.
	for _, c := range searcher.calls {
		assert.NotEqual(t, absCol, c.Collection)
	}
}

func TestRetrieveRerankReorders(t *testing.T) {
	t.Parallel()
	r, _, rer := newTestRetriever(t)
	r.Reranker = &stubReranker{fn: func(_ context.Context, _ string, docs []Document, _ int) ([]Document, error) {
		// Reverse the order to simulate rerank.
		out := make([]Document, len(docs))
		for i, d := range docs {
			out[len(docs)-1-i] = d
		}
		return out, nil
	}}
	_ = rer
	absCol, _ := vectordb.CollectionName("ov_", "acme", KindAbstract)
	r.Searcher = &stubSearcher{hitsByCollection: map[string][]vectordb.Vector{
		absCol: {
			{ID: "a", Score: 0.9, Metadata: map[string]any{"uri": "viking://acme/file/a"}},
			{ID: "b", Score: 0.5, Metadata: map[string]any{"uri": "viking://acme/file/b"}},
		},
	}}
	resp, err := r.Retrieve(context.Background(), RetrieveRequest{
		Account: "acme", Query: "q", TopK: 2,
	})
	require.NoError(t, err)
	require.Len(t, resp.Documents, 2)
	// Rerank reversed: B should come first.
	assert.Equal(t, "viking://acme/file/b", resp.Documents[0].URI)
}

func TestRetrieveDedupByURI(t *testing.T) {
	t.Parallel()
	r, searcher, _ := newTestRetriever(t)
	absCol, _ := vectordb.CollectionName("ov_", "acme", KindAbstract)
	ovCol, _ := vectordb.CollectionName("ov_", "acme", KindOverview)
	chunkCol, _ := vectordb.CollectionName("ov_", "acme", KindChunk)
	uri := "viking://acme/file/docs/x.txt"
	// Same URI appears at all three layers with different scores.
	searcher.hitsByCollection = map[string][]vectordb.Vector{
		absCol:   {{ID: "abs1", Score: 0.3, Metadata: map[string]any{"uri": uri, "content": "abs"}}},
		ovCol:    {{ID: "ov1", Score: 0.6, Metadata: map[string]any{"uri": uri, "content": "ov"}}},
		chunkCol: {{ID: "ch1", Score: 0.9, Metadata: map[string]any{"uri": uri, "content": "chunk"}}},
	}
	// Force L1/L2 to run by injecting candidate dirs (L0 hit URI's parent dir).
	// The funnel derives candidate dirs from L0 hits automatically.

	resp, err := r.Retrieve(context.Background(), RetrieveRequest{
		Account: "acme", Query: "rag", TopK: 5,
	})
	require.NoError(t, err)
	// The same URI should appear at most once in the final output.
	seen := map[string]int{}
	for _, d := range resp.Documents {
		seen[d.URI]++
	}
	for uri, n := range seen {
		assert.LessOrEqual(t, n, 1, "URI %s appeared %d times", uri, n)
	}
}

func TestRetrieveMemoryLifecycle(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRetriever(t)
	store := NewMemoryHotnessStore()
	r.Memory = NewMemoryLifecycle(store)
	r.Memory.Weight = 0.5
	r.Memory.DecayHalfLife = 24 * time.Hour

	absCol, _ := vectordb.CollectionName("ov_", "acme", KindAbstract)
	r.Searcher = &stubSearcher{hitsByCollection: map[string][]vectordb.Vector{
		absCol: {{ID: "a", Score: 0.5, Metadata: map[string]any{"uri": "viking://acme/file/a"}}},
	}}

	now := time.Now()
	// Pre-record access to give a high hotness.
	require.NoError(t, store.Increment(context.Background(), "acme", "viking://acme/file/a", 16, now))

	resp, err := r.Retrieve(context.Background(), RetrieveRequest{
		Account: "acme", Query: "q", TopK: 1,
	})
	require.NoError(t, err)
	require.Len(t, resp.Documents, 1)
	assert.Greater(t, resp.Documents[0].Hotness, 0.0)
}

func TestRetrieveRecordsStats(t *testing.T) {
	t.Parallel()
	r, searcher, _ := newTestRetriever(t)
	absCol, _ := vectordb.CollectionName("ov_", "acme", KindAbstract)
	searcher.hitsByCollection[absCol] = []vectordb.Vector{{
		ID: "a", Score: 0.9, Metadata: map[string]any{"uri": "viking://acme/file/a"},
	}}
	resp, err := r.Retrieve(context.Background(), RetrieveRequest{
		Account: "acme", Query: "q", TopK: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Stats)
	assert.NotZero(t, resp.Stats.Latency)
	assert.Contains(t, resp.Stats.HitsByLevel, Level0Abstract)
}

func TestRetrieveIntentError(t *testing.T) {
	t.Parallel()
	r, _, _ := newTestRetriever(t)
	r.Intent = &stubIntent{err: domain.Wrap(domain.CodeVLMFailed, 502,
		fmt.Errorf("boom"))}
	_, err := r.Retrieve(context.Background(), RetrieveRequest{
		Account: "acme", Query: "q",
	})
	require.Error(t, err)
}

func TestRetrieveSearchError(t *testing.T) {
	t.Parallel()
	r, searcher, _ := newTestRetriever(t)
	searcher.err = domain.Wrap(domain.CodeVectorDBError, 500, fmt.Errorf("x"))
	_, err := r.Retrieve(context.Background(), RetrieveRequest{
		Account: "acme", Query: "q",
	})
	require.Error(t, err)
}

func TestParentDir(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"viking://acme/file/docs/x.txt", "viking://acme/file/docs"},
		{"viking://acme/file/docs", "viking://acme/file"},
		{"viking://acme", "viking://acme"},
		{"", ""},
		{"/foo/bar/baz", "/foo/bar"},
		{"/foo", "/foo"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, parentDir(c.in), c.in)
	}
}

func TestDedupAndSort(t *testing.T) {
	t.Parallel()
	docs := []Document{
		{URI: "a", Score: 0.3, Level: Level0Abstract},
		{URI: "a", Score: 0.9, Level: Level2Chunk},
		{URI: "b", Score: 0.5, Level: Level1Overview},
		{URI: "b", Score: 0.4, Level: Level0Abstract},
	}
	out := dedupAndSort(docs)
	require.Len(t, out, 2)
	assert.Equal(t, "a", out[0].URI)
	assert.InDelta(t, 0.9, out[0].Score, 1e-6)
	assert.Equal(t, Level2Chunk, out[0].Level)
	assert.Equal(t, "b", out[1].URI)
	assert.InDelta(t, 0.5, out[1].Score, 1e-6)
}

func TestFuseRRFDusion(t *testing.T) {
	t.Parallel()
	dense := []vectordb.Vector{
		{ID: "a", Score: 0.9},
		{ID: "b", Score: 0.5},
	}
	sparse := []vectordb.Vector{
		{ID: "b", Score: 0.8},
		{ID: "c", Score: 0.4},
	}
	merged := fuseRRF(dense, sparse, 0)
	// b appears in both lists -> highest fused score.
	assert.Equal(t, "b", merged[0].ID)
	// All three IDs present.
	ids := map[string]bool{}
	for _, h := range merged {
		ids[h.ID] = true
	}
	assert.True(t, ids["a"])
	assert.True(t, ids["b"])
	assert.True(t, ids["c"])
}

func TestFuseRRFRespectsTopK(t *testing.T) {
	t.Parallel()
	dense := []vectordb.Vector{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}
	out := fuseRRF(dense, nil, 2)
	require.Len(t, out, 2)
}

func TestTokenize(t *testing.T) {
	t.Parallel()
	got := tokenize("Hello, RAG world! 123")
	assert.Contains(t, got, "hello")
	assert.Contains(t, got, "rag")
	assert.Contains(t, got, "world")
	assert.Contains(t, got, "123")
}

func TestTokenizeFiltersShort(t *testing.T) {
	t.Parallel()
	got := tokenize("a b cd ef")
	// 1-char tokens are filtered.
	assert.NotContains(t, got, "a")
	assert.NotContains(t, got, "b")
	assert.Contains(t, got, "cd")
	assert.Contains(t, got, "ef")
}

func TestEscapeTerms(t *testing.T) {
	t.Parallel()
	got := escapeTerms([]string{"foo", "bar.baz"})
	assert.Equal(t, "foo", got[0])
	assert.Equal(t, `bar\.baz`, got[1])
}

func TestAdapterRerankerPreservesLevel(t *testing.T) {
	t.Parallel()
	// Use a real rerank.LocalReranker (deterministic, no network).
	a := NewAdapterReranker(rerank.NewLocal())
	docs := []Document{
		{URI: "u1", Content: "alpha beta gamma", Level: Level2Chunk, Score: 0.5},
		{URI: "u2", Content: "delta epsilon zeta", Level: Level1Overview, Score: 0.4},
	}
	out, err := a.Rerank(context.Background(), "alpha", docs, 2)
	require.NoError(t, err)
	require.Len(t, out, 2)
	// Both originals are returned, each preserving its level metadata.
	byURI := map[string]Document{}
	for _, d := range out {
		byURI[d.URI] = d
	}
	assert.Equal(t, Level2Chunk, byURI["u1"].Level)
	assert.Equal(t, Level1Overview, byURI["u2"].Level)
}

func TestAdapterRerankerNilInner(t *testing.T) {
	t.Parallel()
	a := NewAdapterReranker(nil)
	docs := []Document{{URI: "u1", Content: "c1"}}
	out, err := a.Rerank(context.Background(), "q", docs, 1)
	require.NoError(t, err)
	require.Len(t, out, 1)
}

// Sanity: the package compiles with the right interface implementations.
func TestCompileTimeInterfaces(t *testing.T) {
	t.Parallel()
	var _ Retriever = (*HierarchicalRetriever)(nil)
	var _ HybridSearcher = (*DefaultHybridSearcher)(nil)
	var _ Reranker = (*AdapterReranker)(nil)
	var _ IntentAnalyzer = (*VLMIntentAnalyzer)(nil)
}
