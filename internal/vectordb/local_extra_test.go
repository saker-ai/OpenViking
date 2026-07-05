package vectordb

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// TestLocalGetMissingCollection verifies Get returns a 404 AppError when
// the collection does not exist on disk.
func TestLocalGetMissingCollection(t *testing.T) {
	dir := t.TempDir()
	a, err := NewLocalAdapter(dir, "")
	require.NoError(t, err)
	ctx := context.Background()

	_, err = a.Get(ctx, "missing", "id1")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 404, appErr.Status)
}

// TestLocalGetMissingID verifies Get returns a 404 when the collection
// exists but the ID is absent.
func TestLocalGetMissingID(t *testing.T) {
	dir := t.TempDir()
	a, err := NewLocalAdapter(dir, "")
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{
		Name: "c", Dim: 4, Distance: "cosine",
	}))
	_, err = a.Get(ctx, "c", "nope")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 404, appErr.Status)
}

// TestLocalGetExisting verifies Get returns metadata for a stored ID.
func TestLocalGetExisting(t *testing.T) {
	dir := t.TempDir()
	a, err := NewLocalAdapter(dir, "")
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{
		Name: "c", Dim: 4, Distance: "cosine",
	}))
	require.NoError(t, a.Upsert(ctx, "c", []Vector{{
		ID: "x", Embedding: []float32{1, 0, 0, 0},
		Metadata: map[string]any{"uri": "viking://doc/x"},
	}}))
	got, err := a.Get(ctx, "c", "x")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "x", got.ID)
	assert.Equal(t, "viking://doc/x", got.Metadata["uri"])
}

// TestLocalDeleteMissingCollection verifies Delete on a non-existent
// collection is a no-op (returns nil).
func TestLocalDeleteMissingCollection(t *testing.T) {
	dir := t.TempDir()
	a, err := NewLocalAdapter(dir, "")
	require.NoError(t, err)
	assert.NoError(t, a.Delete(context.Background(), "missing", []string{"id1"}))
}

// TestLocalCountMissingCollection verifies Count on a missing collection
// returns 0 with no error (idempotent lazy-load failure).
func TestLocalCountMissingCollection(t *testing.T) {
	dir := t.TempDir()
	a, err := NewLocalAdapter(dir, "")
	require.NoError(t, err)
	count, err := a.Count(context.Background(), "missing")
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}

// TestLocalDropMissingCollection verifies DropCollection on a non-existent
// collection returns nil (idempotent).
func TestLocalDropMissingCollection(t *testing.T) {
	dir := t.TempDir()
	a, err := NewLocalAdapter(dir, "")
	require.NoError(t, err)
	assert.NoError(t, a.DropCollection(context.Background(), "missing"))
}

// TestLocalListCollectionsAfterDrop verifies ListCollections reflects
// drops correctly.
func TestLocalListCollectionsAfterDrop(t *testing.T) {
	dir := t.TempDir()
	a, err := NewLocalAdapter(dir, "")
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "a", Dim: 2, Distance: "cosine"}))
	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "b", Dim: 2, Distance: "cosine"}))
	list, err := a.ListCollections(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	require.NoError(t, a.DropCollection(ctx, "a"))
	list, err = a.ListCollections(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "b", list[0])
}

// TestLocalCloseReopen verifies Close then re-open preserves data.
func TestLocalCloseReopen(t *testing.T) {
	dir := t.TempDir()
	a, err := NewLocalAdapter(dir, "")
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: "c", Dim: 3, Distance: "cosine"}))
	require.NoError(t, a.Upsert(ctx, "c", []Vector{{
		ID: "v1", Embedding: []float32{1, 0, 0},
	}}))
	require.NoError(t, a.Close())

	// Re-open; data should still be there.
	a2, err := NewLocalAdapter(dir, "")
	require.NoError(t, err)
	count, err := a2.Count(ctx, "c")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

// TestNewLocalAdapterEmptyPath verifies the empty-path validation error.
func TestNewLocalAdapterEmptyPath(t *testing.T) {
	_, err := NewLocalAdapter("", "")
	require.Error(t, err)
}

// TestNewLocalAdapterNotADirectory verifies the non-directory path error.
func TestNewLocalAdapterNotADirectory(t *testing.T) {
	dir := t.TempDir()
	filePath := dir + "/notadir"
	require.NoError(t, os.WriteFile(filePath, []byte("x"), 0o644))
	_, err := NewLocalAdapter(filePath, "")
	require.Error(t, err)
}
