package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestStubMemoryStore_GetMissing(t *testing.T) {
	s := NewStubMemoryStore()
	_, err := s.Get(context.Background(), "k")
	if !errors.Is(err, ErrMemoryNotFound) {
		t.Errorf("err = %v, want ErrMemoryNotFound", err)
	}
}

func TestStubMemoryStore_UpsertGetDelete(t *testing.T) {
	s := NewStubMemoryStore()
	ctx := context.Background()

	if err := s.Upsert(ctx, "k", "v1"); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "v1" {
		t.Errorf("got = %q, want v1", got)
	}

	if err := s.Upsert(ctx, "k", "v2"); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}
	got, err = s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if got != "v2" {
		t.Errorf("got = %q, want v2", got)
	}

	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "k"); !errors.Is(err, ErrMemoryNotFound) {
		t.Errorf("after Delete, err = %v, want ErrMemoryNotFound", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
}

func TestStubMemoryStore_DeleteMissing(t *testing.T) {
	// Deleting a missing key is not an error.
	s := NewStubMemoryStore()
	if err := s.Delete(context.Background(), "nope"); err != nil {
		t.Errorf("Delete missing: %v", err)
	}
}

func TestMemoryLifecycle_RememberRecall(t *testing.T) {
	store := NewStubMemoryStore()
	m := NewMemoryLifecycle(store)
	ctx := context.Background()

	key, err := m.Remember(ctx, "user", "preferences", "u1", "likes tea")
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if key != "user/preferences/u1" {
		t.Errorf("key = %q", key)
	}

	got, err := m.Recall(ctx, "user", "preferences", "u1")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if got != "likes tea" {
		t.Errorf("got = %q", got)
	}

	// The store should also have the value (cache is a copy, not a
	// replacement for the store).
	stored, err := store.Get(ctx, "user/preferences/u1")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if stored != "likes tea" {
		t.Errorf("stored = %q", stored)
	}
}

func TestMemoryLifecycle_RecallMissing(t *testing.T) {
	store := NewStubMemoryStore()
	m := NewMemoryLifecycle(store)
	_, err := m.Recall(context.Background(), "user", "preferences", "missing")
	if !errors.Is(err, ErrMemoryNotFound) {
		t.Errorf("err = %v, want ErrMemoryNotFound", err)
	}
}

func TestMemoryLifecycle_Forget(t *testing.T) {
	store := NewStubMemoryStore()
	m := NewMemoryLifecycle(store)
	ctx := context.Background()

	_, _ = m.Remember(ctx, "user", "preferences", "u1", "v")
	if err := m.Forget(ctx, "user", "preferences", "u1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := m.Recall(ctx, "user", "preferences", "u1"); !errors.Is(err, ErrMemoryNotFound) {
		t.Errorf("after Forget, err = %v, want ErrMemoryNotFound", err)
	}
	if store.Len() != 0 {
		t.Errorf("store.Len = %d, want 0", store.Len())
	}
}

func TestMemoryLifecycle_Snapshot(t *testing.T) {
	store := NewStubMemoryStore()
	m := NewMemoryLifecycle(store)
	ctx := context.Background()

	_, _ = m.Remember(ctx, "user", "b", "1", "v1")
	_, _ = m.Remember(ctx, "user", "a", "2", "v2")

	snap := m.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("len = %d, want 2", len(snap))
	}
	// Snapshot is sorted by key.
	if snap[0].Key != "user/a/2" {
		t.Errorf("snap[0].Key = %q", snap[0].Key)
	}
	if snap[1].Key != "user/b/1" {
		t.Errorf("snap[1].Key = %q", snap[1].Key)
	}
}

func TestMemoryLifecycle_RecallCaches(t *testing.T) {
	// Recall should hit the in-memory cache after the first store hit.
	// We verify by deleting from the store directly and checking that
	// Recall still returns the cached value.
	store := NewStubMemoryStore()
	m := NewMemoryLifecycle(store)
	ctx := context.Background()

	_, _ = m.Remember(ctx, "scope", "kind", "id", "cached")
	_ = store.Delete(ctx, "scope/kind/id")

	got, err := m.Recall(ctx, "scope", "kind", "id")
	if err != nil {
		t.Fatalf("Recall after store delete: %v", err)
	}
	if got != "cached" {
		t.Errorf("got = %q, want cached", got)
	}
}

func TestMemoryLifecycle_NilStore(t *testing.T) {
	m := NewMemoryLifecycle(nil)
	ctx := context.Background()

	if _, err := m.Remember(ctx, "s", "k", "i", "v"); !errors.Is(err, ErrMemoryNoStore) {
		t.Errorf("Remember err = %v, want ErrMemoryNoStore", err)
	}
	if _, err := m.Recall(ctx, "s", "k", "i"); !errors.Is(err, ErrMemoryNoStore) {
		t.Errorf("Recall err = %v, want ErrMemoryNoStore", err)
	}
	if err := m.Forget(ctx, "s", "k", "i"); !errors.Is(err, ErrMemoryNoStore) {
		t.Errorf("Forget err = %v, want ErrMemoryNoStore", err)
	}
}

func TestComposeMemoryKey(t *testing.T) {
	cases := []struct {
		scope, kind, id string
		want            string
	}{
		{"user", "preferences", "u1", "user/preferences/u1"},
		{"user", "", "u1", "user/u1"},
		{"", "kind", "id", "kind/id"},
		{"  user  ", "kind", "id", "user/kind/id"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			got := ComposeMemoryKey(c.scope, c.kind, c.id)
			if got != c.want {
				t.Errorf("got = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStubMemoryStore_Concurrent(t *testing.T) {
	s := NewStubMemoryStore()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := strings.Repeat("k", i%5)
			_ = s.Upsert(ctx, key, "v")
			_, _ = s.Get(ctx, key)
			_ = s.Delete(ctx, key)
		}(i)
	}
	wg.Wait()
}

// TestMemoryLifecycle_InterfaceSatisfaction is a compile-time check
// that the stub store satisfies the MemoryStore seam.
func TestMemoryLifecycle_InterfaceSatisfaction(t *testing.T) {
	var _ MemoryStore = NewStubMemoryStore()
}

// fakeErroringStore is a MemoryStore whose Get returns a non-NotFound
// error, so we can verify the lifecycle wraps it correctly.
type fakeErroringStore struct{}

func (fakeErroringStore) Get(_ context.Context, _ string) (string, error) {
	return "", errFake{"boom"}
}
func (fakeErroringStore) Upsert(_ context.Context, _, _ string) error { return nil }
func (fakeErroringStore) Delete(_ context.Context, _ string) error    { return nil }

type errFake struct{ msg string }

func (e errFake) Error() string { return e.msg }

func TestMemoryLifecycle_RecallWrapsStoreError(t *testing.T) {
	m := NewMemoryLifecycle(fakeErroringStore{})
	_, err := m.Recall(context.Background(), "s", "k", "i")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "memory: get") {
		t.Errorf("err = %q, want it to wrap with memory: get", err.Error())
	}
}
