// Package cache provides metadata cache backends for ragfs.
//
// The CacheProvider interface mirrors the Rust cache/provider.rs trait.
// Two implementations are provided:
//
//   - Memory: an in-process LRU cache (internal/ragfs/cache/memory.go)
//   - Redis:   a Redis-backed cache (internal/ragfs/cache/redis.go)
//
// Backends are selected via ragfs.cache.provider in ov.conf. The cache is
// best-effort: misses fall through to the underlying FileSystem.
package cache

import (
	"context"
	"errors"
	"time"
)

// CacheProvider is the contract every ragfs metadata cache backend must
// implement. Corresponds to cache/provider.rs::CacheProvider.
type CacheProvider interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	BatchGet(ctx context.Context, keys []string) (map[string][]byte, error)
	BatchPut(ctx context.Context, items map[string][]byte, ttl time.Duration) error
}

// ErrCacheMiss is returned by Get when a key is not in the cache.
var ErrCacheMiss = errors.New("cache: miss")
