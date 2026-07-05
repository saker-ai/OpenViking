package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/ragfs/cache"
)

func TestMemoryCacheHitMiss(t *testing.T) {
	ctx := context.Background()
	m := cache.NewMemory(8)

	// miss
	_, err := m.Get(ctx, "k1")
	assert.ErrorIs(t, err, cache.ErrCacheMiss)

	// hit
	require.NoError(t, m.Put(ctx, "k1", []byte("v1"), 0))
	got, err := m.Get(ctx, "k1")
	require.NoError(t, err)
	assert.Equal(t, "v1", string(got))

	// delete
	require.NoError(t, m.Delete(ctx, "k1"))
	_, err = m.Get(ctx, "k1")
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
}

func TestMemoryCacheTTL(t *testing.T) {
	ctx := context.Background()
	m := cache.NewMemory(8)
	require.NoError(t, m.Put(ctx, "k", []byte("v"), 30*time.Millisecond))

	got, err := m.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v", string(got))

	time.Sleep(60 * time.Millisecond)
	_, err = m.Get(ctx, "k")
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
}

func TestMemoryCacheBatch(t *testing.T) {
	ctx := context.Background()
	m := cache.NewMemory(16)
	items := map[string][]byte{
		"a": []byte("1"),
		"b": []byte("2"),
		"c": []byte("3"),
	}
	require.NoError(t, m.BatchPut(ctx, items, 0))

	got, err := m.BatchGet(ctx, []string{"a", "b", "c", "missing"})
	require.NoError(t, err)
	assert.Equal(t, "1", string(got["a"]))
	assert.Equal(t, "2", string(got["b"]))
	assert.Equal(t, "3", string(got["c"]))
	_, ok := got["missing"]
	assert.False(t, ok)
}

func TestMemoryCacheLRUEviction(t *testing.T) {
	ctx := context.Background()
	m := cache.NewMemory(2)
	require.NoError(t, m.Put(ctx, "a", []byte("1"), 0))
	require.NoError(t, m.Put(ctx, "b", []byte("2"), 0))
	require.NoError(t, m.Put(ctx, "c", []byte("3"), 0)) // evicts "a" (LRU)

	_, err := m.Get(ctx, "a")
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
	got, err := m.Get(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, "2", string(got))
	got, err = m.Get(ctx, "c")
	require.NoError(t, err)
	assert.Equal(t, "3", string(got))
}

// Redis cache is exercised via a nil-client stub: NewRedisWithClient(nil)
// must behave as a no-op cache rather than panicking. A live-Redis
// integration test belongs in a separate, gated suite.
func TestRedisNilClientNoPanic(t *testing.T) {
	ctx := context.Background()
	r := cache.NewRedisWithClient(nil, "ov")
	// Put/Get/Delete are no-ops on a nil client.
	require.NoError(t, r.Put(ctx, "k", []byte("v"), 0))
	_, err := r.Get(ctx, "k")
	assert.ErrorIs(t, err, cache.ErrCacheMiss)
	require.NoError(t, r.Delete(ctx, "k"))
}
