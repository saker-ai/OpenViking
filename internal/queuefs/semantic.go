package queuefs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // pure-Go SQLite driver; no cgo

	"github.com/saker-ai/ctxhub/internal/queuefs/migrations"
)

// SemanticStore is a SQLite-backed queue store implementing the queuefs
// semantic layer: priority ordering, per-account isolation, delayed tasks,
// lease/claim/renew/complete/abandon with TTL, a dead-letter queue after N
// failed attempts, and list/peek admin operations.
//
// It is the Go equivalent of the Python AGFS QueueFS plugin behaviors:
// priority + FIFO ordering within a priority, lease-based at-least-once
// delivery, and DLQ after exhausting retries.
//
// The store is safe for concurrent use. A Go-level mutex serializes access
// from the owning process; SQLite's _txlock=immediate plus busy_timeout=5000
// serializes cross-process writers. At most one worker holds a lease on a
// given task at any time.
//
// path may be ":memory:" for an in-memory database (useful for tests) or a
// filesystem path for a durable store. The schema is created idempotently
// on construction via the embedded migrations.
type SemanticStore struct {
	db *sql.DB
	mu sync.Mutex
}

// SemanticOption configures a SemanticStore at construction. No options are
// currently defined; the type is reserved for future expansion.
type SemanticOption func(*SemanticStore)

// NewSemanticStore opens (or creates) the SQLite database at path and runs
// migrations. path may be ":memory:" for an in-memory database. The
// returned store is safe for concurrent use.
func NewSemanticStore(path string, _ ...SemanticOption) (*SemanticStore, error) {
	if path == "" {
		return nil, fmt.Errorf("queuefs: semantic store requires a non-empty path")
	}
	dsn := normalizeSQLiteDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("queuefs: open sqlite %s: %w", path, err)
	}
	// SQLite is single-writer; serialize the pool so :memory: (without
	// cache=shared) and file-backed stores behave identically under
	// concurrent access from the same process.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("queuefs: sqlite pragmas: %w", err)
	}
	s := &SemanticStore{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("queuefs: semantic migrate: %w", err)
	}
	return s, nil
}

// normalizeSQLiteDSN rewrites ":memory:" into a shared-cache file DSN and
// appends _txlock=immediate so all BEGIN transactions acquire a write lock
// immediately (avoids "database is locked" under concurrent writers).
func normalizeSQLiteDSN(path string) string {
	if path == ":memory:" {
		return "file::memory:?cache=shared&_txlock=immediate"
	}
	if !strings.Contains(path, "?") {
		return path + "?_txlock=immediate"
	}
	if !strings.Contains(path, "_txlock=") {
		return path + "&_txlock=immediate"
	}
	return path
}

// Close releases the underlying SQLite handle. Idempotent.
func (s *SemanticStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// migrate applies all embedded SQL migrations in lexicographic order. Each
// migration is idempotent (CREATE TABLE IF NOT EXISTS), so re-running on an
// existing database is safe.
func (s *SemanticStore) migrate() error {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := fs.ReadFile(migrations.FS, f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if _, err := s.db.Exec(string(b)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}
	return nil
}

// EnqueueOption tunes an Enqueue call.
type EnqueueOption func(*enqueueCfg)

type enqueueCfg struct {
	account    string
	delayUntil time.Time
}

// WithAccount overrides the task's Account field for this Enqueue call.
// Tasks enqueued with different Account values are invisible to each
// other's Claim calls. Empty is allowed and treated as the "default"
// namespace.
func WithAccount(account string) EnqueueOption {
	return func(c *enqueueCfg) { c.account = account }
}

// WithDelayUntil overrides the task's DelayUntil field for this Enqueue
// call. The task is not eligible for Claim until now >= delayUntil. Zero
// means immediately visible.
func WithDelayUntil(t time.Time) EnqueueOption {
	return func(c *enqueueCfg) { c.delayUntil = t }
}

// Enqueue inserts a task. The task's ID must be non-empty; on conflict the
// existing row is kept (INSERT OR IGNORE) and Enqueue returns nil — this
// makes Enqueue idempotent under at-least-once delivery. The task's
// Priority selects the ordering within the account. MaxRetries caps the
// number of retries (total attempts = MaxRetries + 1) before the task is
// dead-lettered.
//
// Account and DelayUntil may be set on the Task or overridden via options;
// options win.
func (s *SemanticStore) Enqueue(ctx context.Context, task *Task, opts ...EnqueueOption) error {
	if task == nil {
		return fmt.Errorf("queuefs: nil task")
	}
	if task.ID == "" {
		return fmt.Errorf("queuefs: empty task id")
	}
	if task.Type == "" {
		return fmt.Errorf("queuefs: empty task type")
	}
	cfg := enqueueCfg{account: task.Account, delayUntil: task.DelayUntil}
	for _, o := range opts {
		o(&cfg)
	}
	var delayStr string
	if !cfg.delayUntil.IsZero() {
		delayStr = cfg.delayUntil.UTC().Format(time.RFC3339Nano)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO queuefs_tasks
    (id, account_id, type, payload, priority, max_retries, attempts,
     delay_until, enqueued_at, status)
VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, 'pending')
ON CONFLICT(id) DO NOTHING`,
		task.ID, cfg.account, task.Type, task.Payload,
		int(task.Priority), task.MaxRetries, delayStr, now)
	if err != nil {
		return fmt.Errorf("queuefs: enqueue %s: %w", task.ID, err)
	}
	return nil
}

// ClaimOption tunes a Claim call.
type ClaimOption func(*claimCfg)

type claimCfg struct {
	leaseTTL time.Duration
}

// WithLeaseTTL sets the lease duration. The lease must be Renewed before
// it expires, else RecoverStale will re-queue the task. Default 30s.
func WithLeaseTTL(d time.Duration) ClaimOption {
	return func(c *claimCfg) { c.leaseTTL = d }
}

// Claim atomically selects the highest-priority, oldest, visible pending
// task for the given account, marks it 'leased', and returns a Lease.
// Returns (nil, nil) if no task is eligible.
//
// Ordering: priority DESC (higher first), then enqueued_at ASC (FIFO
// within a priority). Tasks with delay_until > now are skipped.
func (s *SemanticStore) Claim(ctx context.Context, account string, opts ...ClaimOption) (*Lease, error) {
	cfg := claimCfg{leaseTTL: 30 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}
	now := time.Now()
	nowStr := now.UTC().Format(time.RFC3339Nano)
	leaseID := uuid.NewString()
	expires := now.Add(cfg.leaseTTL)
	expiresStr := expires.UTC().Format(time.RFC3339Nano)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("queuefs: claim begin: %w", err)
	}
	defer tx.Rollback() // safe: noop after Commit

	var (
		id         string
		typ        string
		payload    []byte
		priority   int
		maxRetries int
		attempts   int
		lastError  sql.NullString
		delayUntil sql.NullString
	)
	row := tx.QueryRowContext(ctx, `
SELECT id, type, payload, priority, max_retries, attempts, last_error, delay_until
FROM queuefs_tasks
WHERE account_id = ? AND status = 'pending'
  AND (delay_until = '' OR delay_until <= ?)
ORDER BY priority DESC, enqueued_at ASC
LIMIT 1`, account, nowStr)
	if err := row.Scan(&id, &typ, &payload, &priority, &maxRetries, &attempts, &lastError, &delayUntil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("queuefs: claim select: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
UPDATE queuefs_tasks
SET status = 'leased', lease_id = ?, lease_expires = ?
WHERE id = ? AND status = 'pending'`, leaseID, expiresStr, id)
	if err != nil {
		return nil, fmt.Errorf("queuefs: claim update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		// Raced (should not happen under _txlock=immediate but be safe).
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("queuefs: claim commit: %w", err)
	}

	task := &Task{
		ID:         id,
		Type:       typ,
		Payload:    payload,
		Priority:   Priority(priority),
		MaxRetries: maxRetries,
		Attempts:   attempts,
		Account:    account,
		LastError:  lastError.String,
	}
	if delayUntil.Valid && delayUntil.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, delayUntil.String); err == nil {
			task.DelayUntil = t
		}
	}
	return &Lease{
		ID:        leaseID,
		TaskID:    id,
		Account:   account,
		Task:      task,
		ExpiresAt: expires,
	}, nil
}

// Renew extends the lease by ttl. Returns the new expiration time.
// Returns ErrLeaseNotFound if the lease does not exist or the task is no
// longer in 'leased' status (e.g. it was completed, abandoned, or recovered
// by RecoverStale).
func (s *SemanticStore) Renew(ctx context.Context, leaseID string, ttl time.Duration) (time.Time, error) {
	if leaseID == "" {
		return time.Time{}, fmt.Errorf("queuefs: empty lease id")
	}
	if ttl <= 0 {
		return time.Time{}, fmt.Errorf("queuefs: lease ttl must be positive")
	}
	expires := time.Now().Add(ttl)
	expiresStr := expires.UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
UPDATE queuefs_tasks
SET lease_expires = ?
WHERE lease_id = ? AND status = 'leased'`, expiresStr, leaseID)
	if err != nil {
		return time.Time{}, fmt.Errorf("queuefs: renew: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return time.Time{}, ErrLeaseNotFound
	}
	return expires, nil
}

// Complete deletes the task (the lease is released and the task is
// permanently removed). Returns ErrLeaseNotFound if the lease does not
// exist.
func (s *SemanticStore) Complete(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return fmt.Errorf("queuefs: empty lease id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM queuefs_tasks WHERE lease_id = ? AND status = 'leased'`, leaseID)
	if err != nil {
		return fmt.Errorf("queuefs: complete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrLeaseNotFound
	}
	return nil
}

// Abandon releases the lease and either requeues the task (with backoff)
// or dead-letters it if attempts have been exhausted.
//
// If task.MaxRetries >= attempts+1, the task is moved back to 'pending'
// with delay_until = now + backoff(attempts+1) and attempts incremented.
// Otherwise the task is moved to the dead_letter table.
//
// taskErr (if non-nil) is recorded as last_error on the task / dead_letter
// row for diagnostics.
func (s *SemanticStore) Abandon(ctx context.Context, leaseID string, taskErr error) error {
	if leaseID == "" {
		return fmt.Errorf("queuefs: empty lease id")
	}
	errMsg := ""
	if taskErr != nil {
		errMsg = taskErr.Error()
	}
	now := time.Now()
	nowStr := now.UTC().Format(time.RFC3339Nano)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("queuefs: abandon begin: %w", err)
	}
	defer tx.Rollback()

	var (
		id         string
		account    string
		typ        string
		payload    []byte
		priority   int
		maxRetries int
		attempts   int
	)
	err = tx.QueryRowContext(ctx, `
SELECT id, account_id, type, payload, priority, max_retries, attempts
FROM queuefs_tasks WHERE lease_id = ? AND status = 'leased'`, leaseID).
		Scan(&id, &account, &typ, &payload, &priority, &maxRetries, &attempts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseNotFound
		}
		return fmt.Errorf("queuefs: abandon select: %w", err)
	}

	newAttempts := attempts + 1
	if newAttempts > maxRetries {
		// Dead-letter: move to DLQ table, then delete the task.
		if _, err := tx.ExecContext(ctx, `
INSERT INTO queuefs_dead_letter
    (id, account_id, type, payload, priority, attempts, last_error, dead_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, account, typ, payload, priority, newAttempts, errMsg, nowStr); err != nil {
			return fmt.Errorf("queuefs: abandon insert dlq: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM queuefs_tasks WHERE id = ?`, id); err != nil {
			return fmt.Errorf("queuefs: abandon delete task: %w", err)
		}
	} else {
		// Requeue with exponential backoff.
		backoff := backoffDuration(newAttempts)
		delayUntil := now.Add(backoff).UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `
UPDATE queuefs_tasks
SET status = 'pending', lease_id = NULL, lease_expires = NULL,
    attempts = ?, delay_until = ?, last_error = ?
WHERE id = ?`, newAttempts, delayUntil, errMsg, id); err != nil {
			return fmt.Errorf("queuefs: abandon requeue: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("queuefs: abandon commit: %w", err)
	}
	return nil
}

// Peek returns the highest-priority, oldest, visible pending task for the
// given account without claiming it. Returns (nil, nil) if no task is
// eligible. Useful for admin dashboards.
func (s *SemanticStore) Peek(ctx context.Context, account string) (*Task, error) {
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		id         string
		typ        string
		payload    []byte
		priority   int
		maxRetries int
		attempts   int
		delayUntil sql.NullString
		lastError  sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
SELECT id, type, payload, priority, max_retries, attempts, delay_until, last_error
FROM queuefs_tasks
WHERE account_id = ? AND status = 'pending'
  AND (delay_until = '' OR delay_until <= ?)
ORDER BY priority DESC, enqueued_at ASC
LIMIT 1`, account, nowStr).
		Scan(&id, &typ, &payload, &priority, &maxRetries, &attempts, &delayUntil, &lastError)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("queuefs: peek: %w", err)
	}
	task := &Task{
		ID:         id,
		Type:       typ,
		Payload:    payload,
		Priority:   Priority(priority),
		MaxRetries: maxRetries,
		Attempts:   attempts,
		Account:    account,
		LastError:  lastError.String,
	}
	if delayUntil.Valid && delayUntil.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, delayUntil.String); err == nil {
			task.DelayUntil = t
		}
	}
	return task, nil
}

// List returns all pending and leased tasks for the given account, ordered
// by priority DESC then enqueued_at ASC. Pass "" to list across all
// accounts. Tasks in the dead-letter queue are not included; use
// DeadLetters for those.
func (s *SemanticStore) List(ctx context.Context, account string) ([]TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		rows *sql.Rows
		err  error
	)
	if account == "" {
		rows, err = s.db.QueryContext(ctx, `
SELECT id, account_id, type, payload, priority, max_retries, attempts,
       delay_until, enqueued_at, status, lease_id, lease_expires, last_error
FROM queuefs_tasks
ORDER BY account_id ASC, priority DESC, enqueued_at ASC`)
	} else {
		rows, err = s.db.QueryContext(ctx, `
SELECT id, account_id, type, payload, priority, max_retries, attempts,
       delay_until, enqueued_at, status, lease_id, lease_expires, last_error
FROM queuefs_tasks
WHERE account_id = ?
ORDER BY priority DESC, enqueued_at ASC`, account)
	}
	if err != nil {
		return nil, fmt.Errorf("queuefs: list: %w", err)
	}
	defer rows.Close()
	var out []TaskRecord
	for rows.Next() {
		var (
			r            TaskRecord
			priorityInt  int
			delayUntil   sql.NullString
			enqueuedAt   string
			leaseID      sql.NullString
			leaseExpires sql.NullString
			lastError    sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Account, &r.Type, &r.Payload, &priorityInt,
			&r.MaxRetries, &r.Attempts, &delayUntil, &enqueuedAt, &r.Status,
			&leaseID, &leaseExpires, &lastError); err != nil {
			return nil, fmt.Errorf("queuefs: list scan: %w", err)
		}
		r.Priority = Priority(priorityInt)
		if delayUntil.Valid && delayUntil.String != "" {
			r.DelayUntil, _ = time.Parse(time.RFC3339Nano, delayUntil.String)
		}
		r.EnqueuedAt, _ = time.Parse(time.RFC3339Nano, enqueuedAt)
		r.LeaseID = leaseID.String
		if leaseExpires.Valid && leaseExpires.String != "" {
			r.LeaseExpiresAt, _ = time.Parse(time.RFC3339Nano, leaseExpires.String)
		}
		r.LastError = lastError.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecoverStale re-queues tasks whose leases have expired before the given
// time. Returns the number of tasks recovered. Used on startup (and
// periodically) to honor at-least-once delivery: if a worker crashed
// mid-processing, its leased tasks become eligible again.
//
// Recovered tasks keep their attempt count and last_error; their delay_until
// is NOT reset, so a task that was retrying with backoff stays invisible
// until its backoff expires.
func (s *SemanticStore) RecoverStale(ctx context.Context, before time.Time) (int, error) {
	beforeStr := before.UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
UPDATE queuefs_tasks
SET status = 'pending', lease_id = NULL, lease_expires = NULL
WHERE status = 'leased'
  AND lease_expires IS NOT NULL
  AND lease_expires <> ''
  AND lease_expires < ?`, beforeStr)
	if err != nil {
		return 0, fmt.Errorf("queuefs: recover stale: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeadLetters returns permanently failed tasks for the given account,
// ordered by dead_at DESC. Pass "" for all accounts.
func (s *SemanticStore) DeadLetters(ctx context.Context, account string) ([]DeadLetter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		rows *sql.Rows
		err  error
	)
	if account == "" {
		rows, err = s.db.QueryContext(ctx, `
SELECT id, account_id, type, payload, priority, attempts, last_error, dead_at
FROM queuefs_dead_letter
ORDER BY dead_at DESC`)
	} else {
		rows, err = s.db.QueryContext(ctx, `
SELECT id, account_id, type, payload, priority, attempts, last_error, dead_at
FROM queuefs_dead_letter
WHERE account_id = ?
ORDER BY dead_at DESC`, account)
	}
	if err != nil {
		return nil, fmt.Errorf("queuefs: dead letters: %w", err)
	}
	defer rows.Close()
	var out []DeadLetter
	for rows.Next() {
		var (
			d        DeadLetter
			deadAt   string
			lastErr  sql.NullString
			priority int
		)
		if err := rows.Scan(&d.ID, &d.Account, &d.Type, &d.Payload, &priority,
			&d.Attempts, &lastErr, &deadAt); err != nil {
			return nil, fmt.Errorf("queuefs: dead letters scan: %w", err)
		}
		d.Priority = Priority(priority)
		d.LastError = lastErr.String
		d.DeadAt, _ = time.Parse(time.RFC3339Nano, deadAt)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDeadLetter permanently removes a dead-letter entry by task ID.
// Returns nil if the ID does not exist (idempotent).
func (s *SemanticStore) DeleteDeadLetter(ctx context.Context, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("queuefs: empty task id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM queuefs_dead_letter WHERE id = ?`, taskID); err != nil {
		return fmt.Errorf("queuefs: delete dead letter: %w", err)
	}
	return nil
}
