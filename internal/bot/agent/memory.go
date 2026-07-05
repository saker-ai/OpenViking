// Package agent: memory lifecycle.
//
// MemoryLifecycle wraps the (forthcoming) internal/session/memory/
// subsystem behind a small interface so the agent loop can be built
// before P0-2 lands. This is the Go counterpart of the lifecycle
// surface of bot/vikingbot/agent/memory.py.
//
// Waiting on P0-2: the concrete MemoryUpdater implementation that
// will satisfy MemoryStore. Until then, callers inject a stub (e.g.
// the in-memory store in tests). The Python MemoryStore exposes a
// much richer surface (peer profiles, experience recall, type-quota
// search) — those methods are intentionally NOT in the seam because
// they depend on internal/session/memory/ types that have not landed
// yet. The seam captures only the Get/Upsert/Delete core so the agent
// loop can wire persistent context across turns today.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// MemoryStore is the seam between the agent loop and the (forthcoming)
// internal/session/memory/ subsystem. The real implementation
// (MemoryUpdater) will be plugged in once P0-2 lands.
//
// The interface is intentionally minimal: Get/Upsert/Delete keyed by
// string. The keys are opaque to the store; the agent loop uses
// "<scope>/<kind>/<id>" strings (e.g. "user/u1/preferences/fruit").
type MemoryStore interface {
	// Get returns the value stored at key. A missing key must return
	// ErrMemoryNotFound so callers can distinguish "no memory" from
	// "store error".
	Get(ctx context.Context, key string) (string, error)
	// Upsert writes value at key, overwriting any prior value.
	Upsert(ctx context.Context, key, value string) error
	// Delete removes the value at key. A missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// ErrMemoryNotFound is returned by MemoryStore.Get when no value is
// stored at the requested key.
var ErrMemoryNotFound = errors.New("memory: not found")

// MemoryEntry is one stored memory record surfaced to the agent loop.
type MemoryEntry struct {
	Key       string
	Value     string
	UpdatedAt time.Time
}

// MemoryLifecycle wraps a MemoryStore and adds higher-level operations:
// scoped key composition, batch upsert, and a "remember this" helper
// that the agent loop can call from a tool handler.
//
// The zero value is NOT usable; use NewMemoryLifecycle.
type MemoryLifecycle struct {
	store MemoryStore
	mu    sync.RWMutex
	cache map[string]MemoryEntry
}

// NewMemoryLifecycle returns a lifecycle wrapper backed by store. The
// cache is in-memory and per-process; it is not durable — durable
// storage is the store's responsibility.
func NewMemoryLifecycle(store MemoryStore) *MemoryLifecycle {
	return &MemoryLifecycle{
		store: store,
		cache: make(map[string]MemoryEntry),
	}
}

// Remember writes value at the composed key (scope/kind/id) and
// updates the in-memory cache. Returns the composed key.
func (m *MemoryLifecycle) Remember(ctx context.Context, scope, kind, id, value string) (string, error) {
	if m.store == nil {
		return "", ErrMemoryNoStore
	}
	key := ComposeMemoryKey(scope, kind, id)
	if err := m.store.Upsert(ctx, key, value); err != nil {
		return "", fmt.Errorf("memory: upsert %s: %w", key, err)
	}
	m.mu.Lock()
	m.cache[key] = MemoryEntry{Key: key, Value: value, UpdatedAt: time.Now()}
	m.mu.Unlock()
	return key, nil
}

// Recall fetches the value at the composed key. Cache hits avoid a
// round-trip to the store; cache misses delegate to the store and
// populate the cache on success.
func (m *MemoryLifecycle) Recall(ctx context.Context, scope, kind, id string) (string, error) {
	if m.store == nil {
		return "", ErrMemoryNoStore
	}
	key := ComposeMemoryKey(scope, kind, id)
	m.mu.RLock()
	if e, ok := m.cache[key]; ok {
		m.mu.RUnlock()
		return e.Value, nil
	}
	m.mu.RUnlock()
	val, err := m.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, ErrMemoryNotFound) {
			return "", ErrMemoryNotFound
		}
		return "", fmt.Errorf("memory: get %s: %w", key, err)
	}
	m.mu.Lock()
	m.cache[key] = MemoryEntry{Key: key, Value: val, UpdatedAt: time.Now()}
	m.mu.Unlock()
	return val, nil
}

// Forget deletes the value at the composed key and drops the cache
// entry. A missing key is not an error.
func (m *MemoryLifecycle) Forget(ctx context.Context, scope, kind, id string) error {
	if m.store == nil {
		return ErrMemoryNoStore
	}
	key := ComposeMemoryKey(scope, kind, id)
	if err := m.store.Delete(ctx, key); err != nil {
		return fmt.Errorf("memory: delete %s: %w", key, err)
	}
	m.mu.Lock()
	delete(m.cache, key)
	m.mu.Unlock()
	return nil
}

// Snapshot returns a copy of the in-memory cache. The order is
// alphabetical by key for stable test output.
func (m *MemoryLifecycle) Snapshot() []MemoryEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]MemoryEntry, 0, len(m.cache))
	for _, e := range m.cache {
		out = append(out, e)
	}
	sortMemoryEntries(out)
	return out
}

// ErrMemoryNoStore is returned when the lifecycle has no backing store
// (the field was nil at construction time).
var ErrMemoryNoStore = errors.New("memory: no store configured")

// ComposeMemoryKey returns the canonical key for a (scope, kind, id)
// triple. Components are joined with "/"; empty components are dropped.
// The leading slash is omitted so callers can prefix with their own
// scope root.
func ComposeMemoryKey(scope, kind, id string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{scope, kind, id} {
		p = strings.TrimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/")
}

// sortMemoryEntries sorts by key for stable Snapshot output.
func sortMemoryEntries(s []MemoryEntry) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1].Key > s[j].Key {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

// StubMemoryStore is an in-memory MemoryStore used by tests and as a
// fallback when no real store is wired. It is safe for concurrent use.
type StubMemoryStore struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewStubMemoryStore returns an empty stub store.
func NewStubMemoryStore() *StubMemoryStore {
	return &StubMemoryStore{data: make(map[string]string)}
}

// Get implements MemoryStore.
func (s *StubMemoryStore) Get(_ context.Context, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return "", ErrMemoryNotFound
	}
	return v, nil
}

// Upsert implements MemoryStore.
func (s *StubMemoryStore) Upsert(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

// Delete implements MemoryStore.
func (s *StubMemoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

// Len returns the number of stored entries. For test assertions.
func (s *StubMemoryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// Compile-time assertions that the stub satisfies the seam.
var (
	_ MemoryStore = (*StubMemoryStore)(nil)
)
