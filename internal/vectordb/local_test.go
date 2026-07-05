package vectordb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// newLocalAdapter opens a LocalAdapter rooted at a fresh temp dir.
func newLocalAdapter(t *testing.T) (*LocalAdapter, string) {
	t.Helper()
	dir := t.TempDir()
	a, err := NewLocalAdapter(dir, "ov_")
	require.NoError(t, err)
	return a, dir
}

func TestLocalEnsureCollectionAndList(t *testing.T) {
	t.Parallel()
	a, dir := newLocalAdapter(t)
	defer a.Close()
	ctx := context.Background()
	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "ov_acct__file", Dim: 4, Distance: "cosine"}))

	// The .bin and .meta.json files must exist.
	_, err := os.Stat(filepath.Join(dir, "ov_acct__file.bin"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "ov_acct__file.meta.json"))
	require.NoError(t, err)

	// ListCollections reads from disk.
	names, err := a.ListCollections(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"ov_acct__file"}, names)

	// DropCollection removes files.
	require.NoError(t, a.DropCollection(ctx, "ov_acct__file"))
	_, err = os.Stat(filepath.Join(dir, "ov_acct__file.bin"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestLocalUpsertSearchDeleteCount(t *testing.T) {
	t.Parallel()
	a, _ := newLocalAdapter(t)
	defer a.Close()
	ctx := context.Background()
	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "c1", Dim: 3, Distance: "cosine"}))

	rows := []Vector{
		{ID: "a", Embedding: []float32{1, 0, 0}, Metadata: map[string]any{"account": "acct", "kind": "file", "uri": "viking://acct/file/a"}},
		{ID: "b", Embedding: []float32{0, 1, 0}, Metadata: map[string]any{"account": "acct", "kind": "file", "uri": "viking://acct/file/b"}},
		{ID: "c", Embedding: []float32{0, 0, 1}, Metadata: map[string]any{"account": "acct", "kind": "dir", "uri": "viking://acct/dir/c"}},
	}
	require.NoError(t, a.Upsert(ctx, "c1", rows))

	n, err := a.Count(ctx, "c1")
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)

	// Search nearest to {1,0,0} should return 'a' first with score ~1.
	res, err := a.Search(ctx, SearchParams{Collection: "c1", Query: []float32{1, 0, 0}, TopK: 2})
	require.NoError(t, err)
	require.Len(t, res.Hits, 2, "hits: %v", res.Hits)
	assert.Equal(t, "a", res.Hits[0].ID)
	assert.InDelta(t, 1.0, res.Hits[0].Score, 1e-5)

	// Filter by kind=dir returns only 'c'.
	res, err = a.Search(ctx, SearchParams{
		Collection: "c1",
		Query:      []float32{1, 0, 0},
		TopK:       10,
		Filter:     Filter{Kind: "dir"},
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "c", res.Hits[0].ID)

	// Delete 'b'.
	require.NoError(t, a.Delete(ctx, "c1", []string{"b"}))
	n, err = a.Count(ctx, "c1")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	// Get returns metadata (embedding may be nil due to hnsw not exposing Get).
	got, err := a.Get(ctx, "c1", "a")
	require.NoError(t, err)
	assert.Equal(t, "a", got.ID)
	assert.Equal(t, "acct", got.Metadata["account"])

	// Get missing.
	_, err = a.Get(ctx, "c1", "nope")
	require.Error(t, err)
	var ae *domain.AppError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, domain.CodeResourceNotFound, ae.Code)
}

// TestLocalPersistReload is the key correctness test: write rows with one
// LocalAdapter instance, close it, then open a fresh LocalAdapter on the
// same dir and verify the rows are visible. This mirrors the production
// lifecycle where the server restarts.
func TestLocalPersistReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()

	writeRows := func() {
		a, err := NewLocalAdapter(dir, "ov_")
		require.NoError(t, err)
		defer a.Close()
		require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "ov_acct__file", Dim: 3}))
		require.NoError(t, a.Upsert(ctx, "ov_acct__file", []Vector{
			{ID: "x1", Embedding: []float32{1, 0, 0}, Metadata: map[string]any{"account": "acct", "kind": "file", "uri": "viking://acct/file/x1"}},
			{ID: "x2", Embedding: []float32{0, 1, 0}, Metadata: map[string]any{"account": "acct", "kind": "file", "uri": "viking://acct/file/x2"}},
			{ID: "x3", Embedding: []float32{0, 0, 1}, Metadata: map[string]any{"account": "acct", "kind": "file", "uri": "viking://acct/file/x3"}},
		}))
	}
	writeRows()

	// Open a fresh instance and reload from disk.
	a2, err := NewLocalAdapter(dir, "ov_")
	require.NoError(t, err)
	defer a2.Close()

	names, err := a2.ListCollections(ctx)
	require.NoError(t, err)
	assert.Contains(t, names, "ov_acct__file")

	n, err := a2.Count(ctx, "ov_acct__file")
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)

	// Search should still return x1 first for query {1,0,0}.
	res, err := a2.Search(ctx, SearchParams{Collection: "ov_acct__file", Query: []float32{1, 0, 0}, TopK: 3})
	require.NoError(t, err)
	require.NotEmpty(t, res.Hits)
	assert.Equal(t, "x1", res.Hits[0].ID)
	assert.InDelta(t, 1.0, res.Hits[0].Score, 1e-5)

	// Filter survives across restarts.
	res, err = a2.Search(ctx, SearchParams{
		Collection: "ov_acct__file",
		Query:      []float32{0, 1, 0},
		TopK:       10,
		Filter:     Filter{URIPrefix: "viking://acct/file/x2"},
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "x2", res.Hits[0].ID)

	// Delete from the reloaded instance persists.
	require.NoError(t, a2.Delete(ctx, "ov_acct__file", []string{"x1"}))
	n, err = a2.Count(ctx, "ov_acct__file")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	// A third instance sees the deletion.
	a3, err := NewLocalAdapter(dir, "ov_")
	require.NoError(t, err)
	defer a3.Close()
	n, err = a3.Count(ctx, "ov_acct__file")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}

func TestLocalRejectsBadInput(t *testing.T) {
	t.Parallel()
	a, _ := newLocalAdapter(t)
	defer a.Close()
	ctx := context.Background()
	require.Error(t, a.EnsureCollection(ctx, CollectionSchema{Name: "", Dim: 3}))
	require.Error(t, a.EnsureCollection(ctx, CollectionSchema{Name: "c", Dim: 0}))
	require.Error(t, a.Upsert(ctx, "", []Vector{{ID: "x", Embedding: []float32{1, 0, 0}}}))

	// Empty embedding on a non-empty collection.
	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "c2", Dim: 3}))
	require.Error(t, a.Upsert(ctx, "c2", []Vector{{ID: "x", Embedding: nil}}))
}

// TestLocalSearchMissingCollectionReturnsEmpty verifies that searching a
// collection that was never EnsureCollection'd returns an empty result
// rather than an error.
func TestLocalSearchMissingCollectionReturnsEmpty(t *testing.T) {
	t.Parallel()
	a, _ := newLocalAdapter(t)
	defer a.Close()
	res, err := a.Search(context.Background(), SearchParams{
		Collection: "never_created",
		Query:      []float32{1, 0, 0},
		TopK:       5,
	})
	require.NoError(t, err)
	assert.Empty(t, res.Hits)
}

// Compile-time assertion that LocalAdapter satisfies CollectionAdapter.
var _ CollectionAdapter = (*LocalAdapter)(nil)
