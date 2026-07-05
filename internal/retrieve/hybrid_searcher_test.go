package retrieve

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/models/embedder"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// newTestSearcher wires a DefaultHybridSearcher against an in-memory
// vectordb and an in-memory ragfs. The vectordb is pre-populated with the
// given rows in the named collection.
func newTestSearcher(t *testing.T, dim int, collection string, rows []vectordb.Vector) (*DefaultHybridSearcher, *vectordb.MemoryAdapter, ragfs.FileSystem) {
	t.Helper()
	embed := embedder.NewLocal(config.EmbedderConfig{Dim: dim}, nil)
	vdb := vectordb.NewMemoryAdapter()
	require.NoError(t, vdb.EnsureCollection(context.Background(), vectordb.CollectionSchema{
		Name: collection, Dim: dim, Distance: "cosine",
	}))
	if len(rows) > 0 {
		require.NoError(t, vdb.Upsert(context.Background(), collection, rows))
	}
	fs := memfs.New("test")
	require.NoError(t, fs.Mkdir(context.Background(), "/accounts/acme", 0o755))
	require.NoError(t, fs.Mkdir(context.Background(), "/accounts/acme/docs", 0o755))
	return NewDefaultHybridSearcher(vdb, embed, fs), vdb, fs
}

func TestHybridSearchDenseOnly(t *testing.T) {
	t.Parallel()
	dim := 8
	collection := "ov_acme__abstract"
	rows := []vectordb.Vector{
		{ID: "r1", Embedding: normVec(dim, 1), Metadata: map[string]any{
			"uri": "viking://acme/file/docs/a.txt", "content": "alpha",
		}},
		{ID: "r2", Embedding: normVec(dim, 2), Metadata: map[string]any{
			"uri": "viking://acme/file/docs/b.txt", "content": "beta",
		}},
	}
	s, _, _ := newTestSearcher(t, dim, collection, rows)
	res, err := s.HybridSearch(context.Background(), HybridSearchRequest{
		Account:    "acme",
		Collection: collection,
		Query:      "alpha",
		TopK:       2,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.NotEmpty(t, res.Hits)
}

func TestHybridSearchEmptyQuery(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestSearcher(t, 8, "c", nil)
	_, err := s.HybridSearch(context.Background(), HybridSearchRequest{
		Collection: "c",
		Query:      "",
	})
	require.Error(t, err)
}

func TestHybridSearchNilVectorDB(t *testing.T) {
	t.Parallel()
	s := &DefaultHybridSearcher{}
	_, err := s.HybridSearch(context.Background(), HybridSearchRequest{
		Query: "q", Collection: "c",
	})
	require.Error(t, err)
}

func TestHybridSearchDegradesOnSparseError(t *testing.T) {
	t.Parallel()
	dim := 8
	collection := "ov_acme__abstract"
	rows := []vectordb.Vector{{
		ID: "r1", Embedding: normVec(dim, 1),
		Metadata: map[string]any{"uri": "viking://acme/file/docs/a.txt"},
	}}
	s, _, _ := newTestSearcher(t, dim, collection, rows)
	// Greps on a non-existent root will error -> sparse leg degrades silently.
	res, err := s.HybridSearch(context.Background(), HybridSearchRequest{
		Account:    "acme",
		Collection: collection,
		Query:      "alpha",
		TopK:       5,
		GrepRoot:   "/accounts/acme/nonexistent",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
}

func TestHybridSearchSparseMatchesFile(t *testing.T) {
	t.Parallel()
	dim := 8
	collection := "ov_acme__abstract"
	rows := []vectordb.Vector{{
		ID: "r1", Embedding: normVec(dim, 1),
		Metadata: map[string]any{"uri": "/accounts/acme/docs/a.txt", "content": "alpha"},
	}}
	s, _, fs := newTestSearcher(t, dim, collection, rows)
	// Write a file with content matching the query.
	require.NoError(t, writeFile(fs, "/accounts/acme/docs/a.txt", "alpha beta gamma"))
	require.NoError(t, writeFile(fs, "/accounts/acme/docs/b.txt", "delta epsilon zeta"))

	res, err := s.HybridSearch(context.Background(), HybridSearchRequest{
		Account:    "acme",
		Collection: collection,
		Query:      "alpha",
		TopK:       5,
		GrepRoot:   "/accounts/acme/docs",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	// We expect at least one hit from either dense or sparse.
	assert.NotEmpty(t, res.Hits)
}

func TestFuseRRFDedupesByID(t *testing.T) {
	t.Parallel()
	dense := []vectordb.Vector{
		{ID: "a", Score: 0.9, Metadata: map[string]any{"src": "dense"}},
	}
	sparse := []vectordb.Vector{
		{ID: "a", Score: 0.8, Metadata: map[string]any{"src": "sparse"}},
	}
	out := fuseRRF(dense, sparse, 0)
	require.Len(t, out, 1)
	// Metadata should be merged (dense wins for conflicting keys).
	assert.Equal(t, "dense", out[0].Metadata["src"])
}

func TestMergeHitPreservesBothMetadata(t *testing.T) {
	t.Parallel()
	d := vectordb.Vector{ID: "a", Metadata: map[string]any{"k1": "v1"}}
	s := vectordb.Vector{ID: "a", Metadata: map[string]any{"k2": "v2"}}
	merged := mergeHit(d, s)
	assert.Equal(t, "v1", merged.Metadata["k1"])
	assert.Equal(t, "v2", merged.Metadata["k2"])
}

func TestTokenizeLowercases(t *testing.T) {
	t.Parallel()
	got := tokenize("RAG Rag rAg")
	for _, tok := range got {
		assert.Equal(t, strings.ToLower(tok), tok)
	}
}

// writeFile writes content to fs at path. Helper for sparse search tests.
func writeFile(fs ragfs.FileSystem, path, content string) error {
	return fs.Write(context.Background(), path, strings.NewReader(content), 0o644)
}

// normVec returns a deterministic dim-length vector where every slot is
// val/dim (L2-normalized so cosine similarity is well-defined).
func normVec(dim int, val float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = val
	}
	// Normalize.
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	if sum == 0 {
		return v
	}
	inv := 1.0 / float32(sqrtF64(float64(sum)))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// sqrtF64 is math.Sqrt without importing math (keeps the test file
// dependency-light).
func sqrtF64(x float64) float64 {
	if x <= 0 {
		return 0
	}
	g := x
	for i := 0; i < 20; i++ {
		g = 0.5 * (g + x/g)
	}
	return g
}
