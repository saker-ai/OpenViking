package cache

import (
	"context"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

// Memory is an in-process LRU cache with per-key TTL. It is safe for
// concurrent use. Corresponds to cache/memory.rs.
type Memory struct {
	cache *lru.Cache[string, *entry]
	mu    sync.Mutex // guards BatchGet/BatchPut atomicity
}

type entry struct {
	val []byte
	exp time.Time // zero == no expiry
}

// NewMemory constructs an in-memory cache with the given capacity (max
// entries). A capacity of 0 means "unbounded" (we use 1<<20 as the
// practical unbounded default since the LRU library requires > 0).
func NewMemory(capacity int) *Memory {
	if capacity <= 0 {
		capacity = 1 << 20
	}
	c, _ := lru.New[string, *entry](capacity)
	return &Memory{cache: c}
}

// Get returns the cached bytes for key. Returns ErrCacheMiss on miss or
// when the entry has expired (TTL is enforced lazily on read).
func (m *Memory) Get(ctx context.Context, key string) ([]byte, error) {
	e, ok := m.cache.Get(key)
	if !ok || e == nil {
		return nil, ErrCacheMiss
	}
	if !e.exp.IsZero() && time.Now().After(e.exp) {
		m.cache.Remove(key)
		return nil, ErrCacheMiss
	}
	out := make([]byte, len(e.val))
	copy(out, e.val)
	return out, nil
}

// Put stores val under key with the given ttl. A ttl of 0 means "no
// expiry" (persistent until evicted by LRU).
func (m *Memory) Put(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	exp := time.Time{}
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	cp := make([]byte, len(val))
	copy(cp, val)
	m.cache.Add(key, &entry{val: cp, exp: exp})
	return nil
}

// Delete removes key from the cache. Missing keys are not an error.
func (m *Memory) Delete(ctx context.Context, key string) error {
	m.cache.Remove(key)
	return nil
}

// BatchGet returns the available keys in one pass. Missing keys are
// omitted from the result map.
func (m *Memory) BatchGet(ctx context.Context, keys []string) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]byte, len(keys))
	for _, k := range keys {
		v, err := m.Get(ctx, k)
		if err == nil {
			out[k] = v
		}
	}
	return out, nil
}

// BatchPut stores all items with the same ttl.
func (m *Memory) BatchPut(ctx context.Context, items map[string][]byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range items {
		if err := m.Put(ctx, k, v, ttl); err != nil {
			return err
		}
	}
	return nil
}

// Len returns the current number of entries (including possibly expired
// ones not yet lazily evicted).
func (m *Memory) Len() int { return m.cache.Len() }
