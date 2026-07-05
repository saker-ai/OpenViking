package retrieve

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryHotnessStoreGetMissing(t *testing.T) {
	t.Parallel()
	s := NewMemoryHotnessStore()
	count, last, err := s.Get(context.Background(), "acct", "viking://acct/file/x")
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.True(t, last.IsZero())
}

func TestMemoryHotnessStoreIncrement(t *testing.T) {
	t.Parallel()
	s := NewMemoryHotnessStore()
	now := time.Now()
	require.NoError(t, s.Increment(context.Background(), "a", "u1", 1, now))
	require.NoError(t, s.Increment(context.Background(), "a", "u1", 2, now.Add(time.Hour)))
	count, last, err := s.Get(context.Background(), "a", "u1")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.Equal(t, now.Add(time.Hour), last)
}

func TestMemoryHotnessStoreIncrementDoesNotRewindLastAccess(t *testing.T) {
	t.Parallel()
	s := NewMemoryHotnessStore()
	now := time.Now()
	require.NoError(t, s.Increment(context.Background(), "a", "u1", 1, now))
	require.NoError(t, s.Increment(context.Background(), "a", "u1", 1, now.Add(-time.Hour)))
	_, last, _ := s.Get(context.Background(), "a", "u1")
	assert.Equal(t, now, last, "older timestamp does not rewind lastAccess")
}

func TestMemoryHotnessStoreListSince(t *testing.T) {
	t.Parallel()
	s := NewMemoryHotnessStore()
	now := time.Now()
	_ = s.Increment(context.Background(), "a", "u1", 1, now)
	_ = s.Increment(context.Background(), "a", "u2", 1, now.Add(-48*time.Hour))
	rows, err := s.ListSince(context.Background(), "a", now.Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "u1", rows[0].URI)
}

func TestMemoryHotnessStoreListSinceAccountIsolated(t *testing.T) {
	t.Parallel()
	s := NewMemoryHotnessStore()
	now := time.Now()
	_ = s.Increment(context.Background(), "a", "u1", 1, now)
	_ = s.Increment(context.Background(), "b", "u2", 1, now)
	rows, _ := s.ListSince(context.Background(), "a", time.Time{})
	require.Len(t, rows, 1)
	assert.Equal(t, "u1", rows[0].URI)
}

func TestMemoryLifecycleNilStoreIsNoOp(t *testing.T) {
	t.Parallel()
	m := NewMemoryLifecycle(nil)
	docs := []Document{{URI: "u1", Score: 0.5}}
	out, err := m.Apply(context.Background(), "a", docs, time.Now())
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, 0.5, out[0].Score)
	assert.Equal(t, 0.0, out[0].Hotness)
}

func TestMemoryLifecycleApplyBlends(t *testing.T) {
	t.Parallel()
	store := NewMemoryHotnessStore()
	m := NewMemoryLifecycle(store)
	m.Weight = 0.5
	m.DecayHalfLife = 24 * time.Hour
	now := time.Now()
	require.NoError(t, store.Increment(context.Background(), "a", "u1", 16, now))

	docs := []Document{{URI: "u1", Score: 0.4}}
	out, err := m.Apply(context.Background(), "a", docs, now)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Greater(t, out[0].Hotness, 0.0)
	// With weight 0.5 and hotness in [0,1] and rerank 0.4,
	// final = 0.4*0.5 + hotness*0.5 >= 0.2.
	assert.Greater(t, out[0].Score, 0.2)
}

func TestMemoryLifecycleApplyMissingURI(t *testing.T) {
	t.Parallel()
	store := NewMemoryHotnessStore()
	m := NewMemoryLifecycle(store)
	now := time.Now()
	docs := []Document{{URI: "u1", Score: 0.5}}
	out, err := m.Apply(context.Background(), "a", docs, now)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, 0.0, out[0].Hotness, "no recorded access -> hotness 0")
	assert.Equal(t, 0.5, out[0].Score)
}

func TestMemoryLifecycleRecordAccess(t *testing.T) {
	t.Parallel()
	store := NewMemoryHotnessStore()
	m := NewMemoryLifecycle(store)
	now := time.Now()
	docs := []Document{{URI: "u1"}, {URI: "u2"}}
	require.NoError(t, m.RecordAccess(context.Background(), "a", docs, now))
	count, _, _ := store.Get(context.Background(), "a", "u1")
	assert.Equal(t, 1, count)
	count, _, _ = store.Get(context.Background(), "a", "u2")
	assert.Equal(t, 1, count)
}

func TestMemoryLifecycleHotnessDecaysOverTime(t *testing.T) {
	t.Parallel()
	m := NewMemoryLifecycle(NewMemoryHotnessStore())
	m.DecayHalfLife = 24 * time.Hour
	now := time.Now()
	// Same count, different lastAccess -> different decay.
	hotRecent := m.hotness(16, now, now)
	hotOld := m.hotness(16, now.Add(-48*time.Hour), now)
	assert.Greater(t, hotRecent, hotOld)
}

func TestMemoryLifecycleHotnessZeroForZeroCount(t *testing.T) {
	t.Parallel()
	m := NewMemoryLifecycle(NewMemoryHotnessStore())
	now := time.Now()
	assert.Equal(t, 0.0, m.hotness(0, now, now))
	assert.Equal(t, 0.0, m.hotness(5, time.Time{}, now))
}

func TestMemoryLifecycleWeightClamps(t *testing.T) {
	t.Parallel()
	// weight = 0 -> returns rerank
	assert.Equal(t, 0.5, blend(0.5, 0.99, 0))
	// weight = 1 -> returns hotness
	assert.Equal(t, 0.99, blend(0.5, 0.99, 1))
	// weight in middle
	assert.InDelta(t, 0.745, blend(0.5, 0.99, 0.5), 1e-3)
}

func TestSQLiteHotnessStoreCRUD(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "hotness.db")
	store, err := NewSQLiteHotnessStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	// Missing row returns zero.
	count, last, err := store.Get(ctx, "acct", "viking://acct/resources/x")
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.True(t, last.IsZero())

	// Increment creates the row.
	require.NoError(t, store.Increment(ctx, "acct", "viking://acct/resources/x", 3, now))
	count, last, err = store.Get(ctx, "acct", "viking://acct/resources/x")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.Equal(t, now.UTC(), last.UTC())

	// Second increment upserts.
	later := now.Add(time.Hour)
	require.NoError(t, store.Increment(ctx, "acct", "viking://acct/resources/x", 2, later))
	count, last, err = store.Get(ctx, "acct", "viking://acct/resources/x")
	require.NoError(t, err)
	assert.Equal(t, 5, count)
	assert.Equal(t, later.UTC(), last.UTC())

	// ListSince returns rows ordered by last_access desc.
	require.NoError(t, store.Increment(ctx, "acct", "viking://acct/resources/y", 1, later.Add(time.Minute)))
	rows, err := store.ListSince(ctx, "acct", now)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "viking://acct/resources/y", rows[0].URI)

	// Different account is isolated.
	count, _, err = store.Get(ctx, "other", "viking://acct/resources/x")
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestMemoryLifecycleApplyStoreErrorPropagates(t *testing.T) {
	t.Parallel()
	m := NewMemoryLifecycle(&errHotnessStore{})
	_, err := m.Apply(context.Background(), "a",
		[]Document{{URI: "u1"}}, time.Now())
	require.Error(t, err)
}

func TestMemoryLifecycleRecordAccessStoreErrorPropagates(t *testing.T) {
	t.Parallel()
	m := NewMemoryLifecycle(&errHotnessStore{})
	err := m.RecordAccess(context.Background(), "a",
		[]Document{{URI: "u1"}}, time.Now())
	require.Error(t, err)
}

// errHotnessStore is a HotnessStore that always errors. Used to assert
// that lifecycle errors propagate.
type errHotnessStore struct{}

func (errHotnessStore) Get(context.Context, string, string) (int, time.Time, error) {
	return 0, time.Time{}, errors.New("store offline")
}

func (errHotnessStore) Increment(context.Context, string, string, int, time.Time) error {
	return errors.New("store offline")
}

func (errHotnessStore) ListSince(context.Context, string, time.Time) ([]HotnessRow, error) {
	return nil, errors.New("store offline")
}

var _ HotnessStore = errHotnessStore{}
