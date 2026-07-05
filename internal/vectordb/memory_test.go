package vectordb

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

func newMemoryAdapterWithCollection(t *testing.T, name string, dim int) *MemoryAdapter {
	t.Helper()
	a := NewMemoryAdapter()
	require.NoError(t, a.EnsureCollection(context.Background(), CollectionSchema{
		Name:     name,
		Dim:      dim,
		Distance: "cosine",
	}))
	return a
}

func TestMemoryEnsureCollectionIdempotent(t *testing.T) {
	t.Parallel()
	a := NewMemoryAdapter()
	ctx := context.Background()
	schema := CollectionSchema{Name: "c1", Dim: 4, Distance: "cosine"}
	require.NoError(t, a.EnsureCollection(ctx, schema))
	require.NoError(t, a.EnsureCollection(ctx, schema)) // no-op on existing
	// Dim mismatch is a conflict.
	require.Error(t, a.EnsureCollection(ctx, CollectionSchema{Name: "c1", Dim: 8}))
}

func TestMemoryEnsureCollectionValidation(t *testing.T) {
	t.Parallel()
	a := NewMemoryAdapter()
	ctx := context.Background()
	require.Error(t, a.EnsureCollection(ctx, CollectionSchema{Name: "", Dim: 4}))
	require.Error(t, a.EnsureCollection(ctx, CollectionSchema{Name: "x", Dim: 0}))
	require.Error(t, a.EnsureCollection(ctx, CollectionSchema{Name: "x", Dim: 4, Distance: "manhattan"}))
}

func TestMemoryUpsertSearchDeleteCount(t *testing.T) {
	t.Parallel()
	const name = "acct__file"
	a := newMemoryAdapterWithCollection(t, name, 3)
	ctx := context.Background()

	rows := []Vector{
		{
			ID:        "r1",
			Embedding: []float32{1.0, 0.0, 0.0},
			Metadata:  map[string]any{"account": "acct", "kind": "file", "uri": "viking://acct/file/r1"},
		},
		{
			ID:        "r2",
			Embedding: []float32{0.0, 1.0, 0.0},
			Metadata:  map[string]any{"account": "acct", "kind": "file", "uri": "viking://acct/file/r2"},
		},
		{
			ID:        "r3",
			Embedding: []float32{0.0, 0.0, 1.0},
			Metadata:  map[string]any{"account": "acct", "kind": "dir", "uri": "viking://acct/dir/r3"},
		},
	}
	require.NoError(t, a.Upsert(ctx, name, rows))

	// Count.
	n, err := a.Count(ctx, name)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)

	// Search nearest to {1,0,0}: r1 should be top.
	res, err := a.Search(ctx, SearchParams{
		Collection: name,
		Query:      []float32{1.0, 0.0, 0.0},
		TopK:       2,
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 2)
	assert.Equal(t, "r1", res.Hits[0].ID)
	assert.InDelta(t, 1.0, res.Hits[0].Score, 1e-6)
	// Second hit is one of r2/r3 with score ~0.
	assert.Contains(t, []string{"r2", "r3"}, res.Hits[1].ID)

	// Filter by kind=file excludes r3.
	res, err = a.Search(ctx, SearchParams{
		Collection: name,
		Query:      []float32{1.0, 0.0, 0.0},
		TopK:       10,
		Filter:     Filter{Account: "acct", Kind: "file"},
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 2)
	assert.Equal(t, "r1", res.Hits[0].ID)

	// Filter by uri_prefix.
	res, err = a.Search(ctx, SearchParams{
		Collection: name,
		Query:      []float32{0.0, 0.0, 1.0},
		TopK:       10,
		Filter:     Filter{URIPrefix: "viking://acct/dir/"},
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "r3", res.Hits[0].ID)

	// Filter by metadata key-value.
	res, err = a.Search(ctx, SearchParams{
		Collection: name,
		Query:      []float32{0.0, 1.0, 0.0},
		TopK:       10,
		Filter:     Filter{Metadata: map[string]any{"kind": "dir"}},
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "r3", res.Hits[0].ID)

	// Delete one row.
	require.NoError(t, a.Delete(ctx, name, []string{"r2"}))
	n, err = a.Count(ctx, name)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	// Get the surviving row.
	got, err := a.Get(ctx, name, "r1")
	require.NoError(t, err)
	assert.Equal(t, "r1", got.ID)
	assert.Equal(t, []float32{1.0, 0.0, 0.0}, got.Embedding)
	assert.Equal(t, "acct", got.Metadata["account"])

	// Get missing row.
	_, err = a.Get(ctx, name, "missing")
	require.Error(t, err)
	var ae *domain.AppError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, domain.CodeResourceNotFound, ae.Code)

	// Upsert replaces by ID (replace r1's metadata).
	require.NoError(t, a.Upsert(ctx, name, []Vector{{
		ID:        "r1",
		Embedding: []float32{1.0, 0.0, 0.0},
		Metadata:  map[string]any{"account": "acct", "kind": "file", "uri": "viking://acct/file/r1", "tag": "updated"},
	}}))
	got, err = a.Get(ctx, name, "r1")
	require.NoError(t, err)
	assert.Equal(t, "updated", got.Metadata["tag"])

	// Empty collection name and dim mismatch errors.
	require.Error(t, a.Upsert(ctx, "", rows))
	require.Error(t, a.Upsert(ctx, name, []Vector{{ID: "x", Embedding: []float32{1, 2, 3, 4}}}))
}

func TestMemorySearchMissingCollectionIsEmpty(t *testing.T) {
	t.Parallel()
	a := NewMemoryAdapter()
	res, err := a.Search(context.Background(), SearchParams{
		Collection: "nope",
		Query:      []float32{1, 0, 0},
		TopK:       5,
	})
	require.NoError(t, err)
	assert.Empty(t, res.Hits)
}

func TestMemoryListAndDrop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := NewMemoryAdapter()
	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "c1", Dim: 4}))
	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "c2", Dim: 4}))
	names, err := a.ListCollections(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"c1", "c2"}, names)

	require.NoError(t, a.DropCollection(ctx, "c1"))
	names, err = a.ListCollections(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"c2"}, names)

	// Drop missing is a no-op.
	require.NoError(t, a.DropCollection(ctx, "missing"))
	// Close releases state.
	require.NoError(t, a.Close())
}

func TestMemorySearchRejectsBadQuery(t *testing.T) {
	t.Parallel()
	a := newMemoryAdapterWithCollection(t, "c", 3)
	_, err := a.Search(context.Background(), SearchParams{Collection: "c", Query: nil, TopK: 1})
	require.Error(t, err)
	// Dim mismatch.
	_, err = a.Search(context.Background(), SearchParams{Collection: "c", Query: []float32{1, 2}, TopK: 1})
	require.Error(t, err)
}

func TestMemoryUpsertMissingCollection(t *testing.T) {
	t.Parallel()
	a := NewMemoryAdapter()
	err := a.Upsert(context.Background(), "missing", []Vector{{ID: "x", Embedding: []float32{1, 0, 0}}})
	require.Error(t, err)
	var ae *domain.AppError
	errors.As(err, &ae)
	assert.Equal(t, domain.CodeConflict, ae.Code)
}

// Compile-time assertion that MemoryAdapter satisfies CollectionAdapter.
var _ CollectionAdapter = (*MemoryAdapter)(nil)
