// Package retrieve — memory lifecycle: per-resource hotness scoring.
//
// The hotness score combines two signals:
//   - Access frequency: how often the resource has been returned by Retrieve
//     over a rolling window.
//   - Time decay: older accesses contribute less than recent ones.
//
// The score is in [0, 1] and is added (with a configurable weight) to the
// rerank score during the final ranking pass.
//
// Storage: a small on-disk SQLite database keyed by account+URI. The
// schema is intentionally minimal so it can be replaced by a Redis-backed
// store in a follow-up phase without touching the retrieve pipeline.
//
// Tests use an in-memory store (MemoryHotnessStore) to avoid touching the
// filesystem.
package retrieve

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; no cgo
)

// HotnessStore is the persistence interface for hotness scores. The store
// is keyed by (account, uri); implementations must be safe for concurrent
// use.
type HotnessStore interface {
	// Get returns the current access count and last-access time for the
	// given uri. A missing row returns (0, time.Time{}, nil).
	Get(ctx context.Context, account, uri string) (count int, lastAccess time.Time, err error)
	// Increment bumps the access count by n and updates lastAccess to now.
	Increment(ctx context.Context, account, uri string, n int, now time.Time) error
	// ListSince returns all rows whose lastAccess >= since. Used by the
	// stats collector to compute decayed hotness in bulk.
	ListSince(ctx context.Context, account string, since time.Time) ([]HotnessRow, error)
}

// HotnessRow is one row in the hotness store.
type HotnessRow struct {
	URI        string
	Count      int
	LastAccess time.Time
}

// MemoryLifecycle applies hotness weighting to retrieve results.
//
// DecayHalfLife controls the exponential decay: an access N hours ago
// contributes 2^(-N / DecayHalfLife). Default 168h (one week).
//
// Weight controls how much hotness influences final ranking: final_score
// = rerank_score * (1 - Weight) + hotness * Weight. Default 0.1.
type MemoryLifecycle struct {
	Store         HotnessStore
	DecayHalfLife time.Duration
	Weight        float64
}

// NewMemoryLifecycle constructs a MemoryLifecycle with sensible defaults.
// store may be nil; in that case hotness is always 0 and the lifecycle
// becomes a no-op (useful for tests that don't care about hotness).
func NewMemoryLifecycle(store HotnessStore) *MemoryLifecycle {
	return &MemoryLifecycle{
		Store:         store,
		DecayHalfLife: 168 * time.Hour,
		Weight:        0.1,
	}
}

// Apply augments each document with a Hotness score and returns a copy
// with adjusted Score values. Documents are NOT re-sorted; the caller is
// expected to sort by the new Score if desired.
//
// Resources with no access history (hotness == 0) keep their original
// rerank score: we don't want to penalize newly-added resources just
// because they have no recorded access yet.
func (m *MemoryLifecycle) Apply(ctx context.Context, account string, docs []Document, now time.Time) ([]Document, error) {
	if m == nil || m.Store == nil || len(docs) == 0 {
		return docs, nil
	}
	out := make([]Document, len(docs))
	for i, d := range docs {
		count, last, err := m.Store.Get(ctx, account, d.URI)
		if err != nil {
			return nil, fmt.Errorf("retrieve.memory: get %s: %w", d.URI, err)
		}
		hot := m.hotness(count, last, now)
		out[i] = d
		out[i].Hotness = hot
		// Only blend when hotness > 0; otherwise keep the original score.
		if hot > 0 {
			out[i].Score = blend(d.Score, hot, m.Weight)
		}
	}
	return out, nil
}

// RecordAccess bumps the access count for each returned URI. Called by
// the retriever after the final ranking pass.
func (m *MemoryLifecycle) RecordAccess(ctx context.Context, account string, docs []Document, now time.Time) error {
	if m == nil || m.Store == nil {
		return nil
	}
	for _, d := range docs {
		if err := m.Store.Increment(ctx, account, d.URI, 1, now); err != nil {
			return fmt.Errorf("retrieve.memory: increment %s: %w", d.URI, err)
		}
	}
	return nil
}

// hotness computes the decayed hotness score in [0, 1] from count and
// lastAccess.
func (m *MemoryLifecycle) hotness(count int, lastAccess, now time.Time) float64 {
	if count <= 0 || lastAccess.IsZero() {
		return 0
	}
	age := now.Sub(lastAccess)
	if age < 0 {
		age = 0
	}
	// Exponential decay: 2^(-age / halfLife).
	decay := math.Pow(0.5, float64(age)/float64(m.DecayHalfLife))
	// Frequency saturates at 16 accesses (log2(16) = 4 -> ~1.0).
	freq := 1.0 - math.Pow(0.5, float64(count)/4.0)
	if freq > 1 {
		freq = 1
	}
	// Combine: decay alone is meaningless without frequency; we use the
	// geometric mean so a high count with old access still scores > 0.
	return math.Sqrt(decay * freq)
}

// blend returns rerank*(1-w) + hotness*w.
func blend(rerank, hotness, w float64) float64 {
	if w <= 0 {
		return rerank
	}
	if w >= 1 {
		return hotness
	}
	return rerank*(1-w) + hotness*w
}

// MemoryHotnessStore is an in-memory HotnessStore for tests.
type MemoryHotnessStore struct {
	mu   sync.Mutex
	rows map[string]HotnessRow
}

// NewMemoryHotnessStore returns an empty in-memory hotness store.
func NewMemoryHotnessStore() *MemoryHotnessStore {
	return &MemoryHotnessStore{rows: make(map[string]HotnessRow)}
}

func key(account, uri string) string { return account + "\x00" + uri }

// Get implements HotnessStore.
func (s *MemoryHotnessStore) Get(_ context.Context, account, uri string) (int, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[key(account, uri)]
	if !ok {
		return 0, time.Time{}, nil
	}
	return r.Count, r.LastAccess, nil
}

// Increment implements HotnessStore.
func (s *MemoryHotnessStore) Increment(_ context.Context, account, uri string, n int, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(account, uri)
	r := s.rows[k]
	r.URI = uri
	r.Count += n
	if now.After(r.LastAccess) {
		r.LastAccess = now
	}
	s.rows[k] = r
	return nil
}

// ListSince implements HotnessStore.
func (s *MemoryHotnessStore) ListSince(_ context.Context, account string, since time.Time) ([]HotnessRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []HotnessRow{}
	for k, r := range s.rows {
		if !strings.HasPrefix(k, account+"\x00") {
			continue
		}
		if !r.LastAccess.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

// SQLiteHotnessStore is a file-backed HotnessStore. The schema is created
// lazily on the first call. Backed by modernc.org/sqlite (pure-Go, no cgo)
// so the binary stays static-linkable.
type SQLiteHotnessStore struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

// NewSQLiteHotnessStore opens (or creates) the SQLite file at path and
// creates the hotness schema if missing. The returned store is safe for
// concurrent use.
func NewSQLiteHotnessStore(path string) (*SQLiteHotnessStore, error) {
	if path == "" {
		return nil, fmt.Errorf("retrieve: sqlite hotness store requires a non-empty path")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("retrieve: open sqlite %s: %w", path, err)
	}
	s := &SQLiteHotnessStore{db: db, path: path}
	if err := s.ensureSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("retrieve: sqlite schema: %w", err)
	}
	return s, nil
}

// ensureSchema creates the hotness table if it does not exist. The
// (account, uri) pair is the primary key; last_access is stored as
// RFC3339 to keep the schema readable when inspected via the sqlite3 CLI.
func (s *SQLiteHotnessStore) ensureSchema() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS hotness (
    account      TEXT NOT NULL,
    uri          TEXT NOT NULL,
    access_count INTEGER NOT NULL DEFAULT 0,
    last_access  TEXT NOT NULL,
    PRIMARY KEY (account, uri)
);
CREATE INDEX IF NOT EXISTS hotness_by_last_access ON hotness(account, last_access);
`
	_, err := s.db.Exec(ddl)
	return err
}

// Get returns the access count and last-access time for (account, uri).
// A missing row returns (0, time.Time{}, nil).
func (s *SQLiteHotnessStore) Get(ctx context.Context, account, uri string) (int, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		count  int
		access string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT access_count, last_access FROM hotness WHERE account = ? AND uri = ?`,
		account, uri).Scan(&count, &access)
	if err == sql.ErrNoRows {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("sqlite hotness get: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, access)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, access)
	}
	return count, t, nil
}

// Increment bumps the access count by n and updates last_access to now.
// INSERT ... ON CONFLICT upserts so the row is created on first access.
func (s *SQLiteHotnessStore) Increment(ctx context.Context, account, uri string, n int, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO hotness (account, uri, access_count, last_access) VALUES (?, ?, ?, ?)
ON CONFLICT(account, uri) DO UPDATE SET
    access_count = access_count + excluded.access_count,
    last_access  = excluded.last_access`,
		account, uri, n, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("sqlite hotness increment: %w", err)
	}
	return nil
}

// ListSince returns all rows whose last_access >= since, ordered by
// last_access descending.
func (s *SQLiteHotnessStore) ListSince(ctx context.Context, account string, since time.Time) ([]HotnessRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx,
		`SELECT uri, access_count, last_access FROM hotness
		 WHERE account = ? AND last_access >= ?
		 ORDER BY last_access DESC`,
		account, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("sqlite hotness list: %w", err)
	}
	defer rows.Close()
	var out []HotnessRow
	for rows.Next() {
		var (
			r       HotnessRow
			access  string
		)
		if err := rows.Scan(&r.URI, &r.Count, &access); err != nil {
			return nil, fmt.Errorf("sqlite hotness scan: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, access)
		if err != nil {
			t, _ = time.Parse(time.RFC3339, access)
		}
		r.LastAccess = t
		out = append(out, r)
	}
	return out, rows.Err()
}

// Close releases the database handle. Subsequent calls error.
func (s *SQLiteHotnessStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Compile-time assertions.
var _ HotnessStore = (*MemoryHotnessStore)(nil)
var _ HotnessStore = (*SQLiteHotnessStore)(nil)
