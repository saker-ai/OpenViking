package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"

	// Pure-Go SQLite driver; no cgo required.
	_ "modernc.org/sqlite"
)

// CursorRecord is the durable state row for one (harness, session). It
// carries the read-position pointer plus commit/idempotency metadata.
type CursorRecord struct {
	Harness           string
	NativeSessionID   string
	OVSessionID       string
	Cursor            *Cursor
	Locator           string
	Title             string
	LastAppendedCount int
	PendingTokens     int
	NeedsCommit       bool
	PendingCursor     *Cursor
	PendingCount      int
	PendingBaseline   int
	LastCommittedAt   string
	UpdatedAt         string
}

// CursorStore is the durable per-(harness, session) state store backed by
// a SQLite database under stateDir. It is goroutine-safe via a sync.Mutex
// around the underlying *sql.DB (modernc.org/sqlite serializes writes
// internally, but we add the mutex to keep the public API simple).
//
// Schema mirrors openviking/ingest/cursor_store.py::_SCHEMA. Two tables:
//
//	ingest_cursors — one row per (harness, native_session_id)
type CursorStore struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

const cursorSchema = `
CREATE TABLE IF NOT EXISTS ingest_cursor (
    harness             TEXT NOT NULL,
    native_session_id   TEXT NOT NULL,
    ov_session_id       TEXT,
    cursor_kind         TEXT NOT NULL,
    cursor_value        TEXT NOT NULL,
    locator             TEXT,
    title               TEXT,
    last_appended_count INTEGER NOT NULL DEFAULT 0,
    pending_tokens      INTEGER NOT NULL DEFAULT 0,
    needs_commit        INTEGER NOT NULL DEFAULT 0,
    pend_cursor         TEXT,
    pend_count          INTEGER NOT NULL DEFAULT 0,
    pend_baseline       INTEGER NOT NULL DEFAULT 0,
    last_committed_at   TEXT,
    updated_at          TEXT,
    PRIMARY KEY (harness, native_session_id)
);
`

// NewCursorStore opens (or creates) a cursor store at stateDir/state.db.
// The directory is created if missing.
func NewCursorStore(stateDir string) (*CursorStore, error) {
	if stateDir == "" {
		return nil, domain.NewAppError(domain.CodeInternalError, 500,
			"ingest: cursor store requires a non-empty state dir")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	path := filepath.Join(stateDir, "state.db")
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	db.SetMaxOpenConns(1) // modernc.org/sqlite is serialized; avoid pool overhead
	if _, err := db.Exec(cursorSchema); err != nil {
		db.Close()
		return nil, domain.Wrap(domain.CodeInternalError, 500,
			fmt.Errorf("ingest: apply cursor schema: %w", err))
	}
	return &CursorStore{db: db, path: path}, nil
}

// Path returns the absolute path to the SQLite database file.
func (s *CursorStore) Path() string { return s.path }

// Close releases the database handle.
func (s *CursorStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Get fetches the cursor record for (harness, nativeSessionID). Returns
// (nil, nil) when the row does not exist.
func (s *CursorStore) Get(ctx context.Context, harness, nativeSessionID string) (*CursorRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRowContext(ctx, `
        SELECT harness, native_session_id, ov_session_id, cursor_kind, cursor_value,
               locator, title, last_appended_count, pending_tokens, needs_commit,
               pend_cursor, pend_count, pend_baseline, last_committed_at, updated_at
        FROM ingest_cursor
        WHERE harness = ? AND native_session_id = ?`, harness, nativeSessionID)
	rec, err := scanCursorRecord(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return rec, nil
}

// GetCursor returns just the cursor for (harness, nativeSessionID), or
// nil when the row does not exist.
func (s *CursorStore) GetCursor(ctx context.Context, harness, nativeSessionID, kind string) (*Cursor, error) {
	rec, err := s.Get(ctx, harness, nativeSessionID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	return rec.Cursor, nil
}

// EnsureRow inserts an initial zero-cursor row for (harness, nativeSessionID)
// when absent. Idempotent.
func (s *CursorStore) EnsureRow(ctx context.Context, harness, nativeSessionID, ovSessionID string, cursor *Cursor, locator, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor == nil {
		cursor = ZeroCursor(CursorByteOffset)
	}
	val, err := encodeCursorValue(cursor)
	if err != nil {
		return err
	}
	kind := cursor.Kind
	if kind == "" {
		kind = CursorByteOffset
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT OR IGNORE INTO ingest_cursor
            (harness, native_session_id, ov_session_id, cursor_kind, cursor_value,
             locator, title, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		harness, nativeSessionID, ovSessionID, kind, val, locator, title, nowISO())
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return nil
}

// AdvanceCursor updates the cursor (and ov_session_id / locator / title when
// non-empty) for an existing row. The row is created if absent.
func (s *CursorStore) AdvanceCursor(ctx context.Context, harness, nativeSessionID, ovSessionID string, cursor *Cursor, locator string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor == nil {
		return domain.NewAppError(domain.CodeValidationFailed, 422,
			"ingest: advance cursor with nil cursor")
	}
	val, err := encodeCursorValue(cursor)
	if err != nil {
		return err
	}
	kind := cursor.Kind
	if kind == "" {
		kind = CursorByteOffset
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO ingest_cursor
            (harness, native_session_id, ov_session_id, cursor_kind, cursor_value,
             locator, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(harness, native_session_id) DO UPDATE SET
            ov_session_id  = excluded.ov_session_id,
            cursor_kind    = excluded.cursor_kind,
            cursor_value   = excluded.cursor_value,
            locator        = excluded.locator,
            updated_at     = excluded.updated_at`,
		harness, nativeSessionID, ovSessionID, kind, val, locator, nowISO())
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return nil
}

// SetPending records a pending batch intent (target cursor + count +
// baseline server message count) BEFORE the batch is appended. This lets
// a crash mid-append be reconciled against the server's actual message
// count on the next run instead of blindly re-appending.
func (s *CursorStore) SetPending(ctx context.Context, harness, nativeSessionID string, cursor *Cursor, count, baseline int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor == nil {
		return domain.NewAppError(domain.CodeValidationFailed, 422,
			"ingest: set pending with nil cursor")
	}
	val, err := encodeCursorValue(cursor)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
        UPDATE ingest_cursor SET
            pend_cursor    = ?,
            pend_count     = ?,
            pend_baseline  = ?,
            needs_commit   = 1,
            updated_at     = ?
        WHERE harness = ? AND native_session_id = ?`,
		val, count, baseline, nowISO(), harness, nativeSessionID)
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return nil
}

// ClearPending zeroes the pending intent fields. Called after a confirmed
// append.
func (s *CursorStore) ClearPending(ctx context.Context, harness, nativeSessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
        UPDATE ingest_cursor SET
            pend_cursor    = NULL,
            pend_count     = 0,
            pend_baseline  = 0,
            updated_at     = ?
        WHERE harness = ? AND native_session_id = ?`,
		nowISO(), harness, nativeSessionID)
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return nil
}

// MarkCommitted sets needs_commit=0 and last_committed_at after a
// successful commit.
func (s *CursorStore) MarkCommitted(ctx context.Context, harness, nativeSessionID string, pendingTokens int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
        UPDATE ingest_cursor SET
            needs_commit      = 0,
            pending_tokens    = ?,
            last_committed_at = ?,
            updated_at        = ?
        WHERE harness = ? AND native_session_id = ?`,
		pendingTokens, nowISO(), nowISO(), harness, nativeSessionID)
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return nil
}

// IncrementAppended bumps last_appended_count by delta.
func (s *CursorStore) IncrementAppended(ctx context.Context, harness, nativeSessionID string, delta int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
        UPDATE ingest_cursor SET
            last_appended_count = last_appended_count + ?,
            updated_at          = ?
        WHERE harness = ? AND native_session_id = ?`,
		delta, nowISO(), harness, nativeSessionID)
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return nil
}

// scanCursorRecord reads a single row from a *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanCursorRecord(row scanner) (*CursorRecord, error) {
	var rec CursorRecord
	var (
		cursorKind, cursorValue     string
		ovSessionID, locator, title sql.NullString
		pendCursor                  sql.NullString
		lastCommittedAt, updatedAt  sql.NullString
		lastAppended, pendingTokens int
		needsCommit                 int
		pendCount, pendBaseline     int
	)
	err := row.Scan(
		&rec.Harness, &rec.NativeSessionID, &ovSessionID, &cursorKind, &cursorValue,
		&locator, &title, &lastAppended, &pendingTokens, &needsCommit,
		&pendCursor, &pendCount, &pendBaseline, &lastCommittedAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	rec.OVSessionID = ovSessionID.String
	rec.Locator = locator.String
	rec.Title = title.String
	rec.LastAppendedCount = lastAppended
	rec.PendingTokens = pendingTokens
	rec.NeedsCommit = needsCommit != 0
	rec.PendingCount = pendCount
	rec.PendingBaseline = pendBaseline
	rec.LastCommittedAt = lastCommittedAt.String
	rec.UpdatedAt = updatedAt.String
	rec.Cursor = decodeCursorValue(cursorKind, cursorValue)
	if pendCursor.Valid {
		rec.PendingCursor = decodeCursorValue(cursorKind, pendCursor.String)
	}
	return &rec, nil
}

func encodeCursorValue(c *Cursor) (string, error) {
	if c == nil {
		return "{}", nil
	}
	b, err := json.Marshal(c.Value)
	if err != nil {
		return "", domain.Wrap(domain.CodeInternalError, 500, err)
	}
	return string(b), nil
}

func decodeCursorValue(kind, raw string) *Cursor {
	if raw == "" {
		return ZeroCursor(kind)
	}
	var val map[string]any
	if err := json.Unmarshal([]byte(raw), &val); err != nil {
		return ZeroCursor(kind)
	}
	// JSON unmarshals numbers as float64; coerce offset/time to int64.
	if v, ok := val["offset"]; ok {
		if f, ok := v.(float64); ok {
			val["offset"] = int64(f)
		}
	}
	if v, ok := val["time"]; ok {
		if f, ok := v.(float64); ok {
			val["time"] = int64(f)
		}
	}
	return &Cursor{Kind: kind, Value: val}
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
