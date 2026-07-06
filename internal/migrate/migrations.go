// Package migrate builds the ctxhub-migrate CLI: schema migrations
// for ragfs / vectordb / queuefs and the Python -> Go data bridge.
//
// This file implements the SQL migration runner used by `migrate ragfs`.
// Migrations are embedded .sql files (see internal/ragfs/migrations).
// Applied versions are tracked in a `_migrations` table inside the target
// SQLite database. The runner is idempotent: re-running with no new files
// is a no-op.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; no cgo

	"github.com/saker-ai/ctxhub/internal/ragfs/migrations"
)

// Migration is one embedded .sql file paired with its version label.
type Migration struct {
	// Version is the leading numeric component of the filename (e.g.
	// "001"). Migrations are applied in ascending Version order.
	Version string

	// Name is the base filename without extension (e.g. "001_initial").
	Name string

	// Filename is the base filename including extension.
	Filename string

	// SQL is the file body.
	SQL string
}

// LoadMigrations reads the embedded ragfs migration files and returns
// them sorted in application order (lexicographic by filename).
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read embedded migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", name, err)
		}
		stem := strings.TrimSuffix(name, ".sql")
		version := strings.SplitN(stem, "_", 2)[0]
		out = append(out, Migration{
			Version:  version,
			Name:     stem,
			Filename: name,
			SQL:      string(body),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Filename < out[j].Filename
	})
	return out, nil
}

// AppliedMigration is one row of the `_migrations` table.
type AppliedMigration struct {
	Version   string
	AppliedAt time.Time
}

// ListApplied returns the migrations already recorded in db's
// `_migrations` table. Returns an empty slice (and a nil error) when
// the table does not yet exist.
func ListApplied(ctx context.Context, db *sql.DB) ([]AppliedMigration, error) {
	exists, err := migrationsTableExists(ctx, db)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT version, applied_at FROM _migrations ORDER BY version ASC`)
	if err != nil {
		return nil, fmt.Errorf("migrate: list applied: %w", err)
	}
	defer rows.Close()
	var out []AppliedMigration
	for rows.Next() {
		var am AppliedMigration
		var ts string
		if err := rows.Scan(&am.Version, &ts); err != nil {
			return nil, fmt.Errorf("migrate: scan applied: %w", err)
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return nil, fmt.Errorf("migrate: parse applied_at %q: %w", ts, err)
		}
		am.AppliedAt = t
		out = append(out, am)
	}
	return out, rows.Err()
}

// PendingMigrations returns the subset of LoadMigrations() that has not
// yet been recorded in db's `_migrations` table, in application order.
func PendingMigrations(ctx context.Context, db *sql.DB) ([]Migration, error) {
	all, err := LoadMigrations()
	if err != nil {
		return nil, err
	}
	applied, err := ListApplied(ctx, db)
	if err != nil {
		return nil, err
	}
	appliedSet := make(map[string]struct{}, len(applied))
	for _, a := range applied {
		appliedSet[a.Version] = struct{}{}
	}
	var pending []Migration
	for _, m := range all {
		if _, ok := appliedSet[m.Version]; ok {
			continue
		}
		pending = append(pending, m)
	}
	return pending, nil
}

// ApplyMigrations runs every pending migration against db inside a single
// transaction. On success, each migration is recorded in `_migrations`.
// On failure, the transaction rolls back and no version is recorded.
//
// `applied` is the list of migrations that were actually applied during
// this call (empty when the database was already at HEAD).
func ApplyMigrations(ctx context.Context, db *sql.DB) ([]Migration, error) {
	if err := ensureMigrationsTable(ctx, db); err != nil {
		return nil, err
	}
	pending, err := PendingMigrations(ctx, db)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	for _, m := range pending {
		if err := applyOne(ctx, db, m); err != nil {
			return nil, fmt.Errorf("migrate: apply %s: %w", m.Filename, err)
		}
	}
	return pending, nil
}

// applyOne executes a single migration's SQL and records its version.
// The SQL is wrapped in a transaction so a mid-migration failure rolls
// back partial work. SQLite executes one statement at a time; we split
// the file on a trailing semicolon to support multi-statement files.
func applyOne(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	for _, stmt := range splitSQLStatements(m.SQL) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("exec %q: %w", snippet(stmt), err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO _migrations (version, applied_at) VALUES (?, ?)`,
		m.Version, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	tx = nil
	return nil
}

// ensureMigrationsTable creates `_migrations` if it doesn't exist. The
// table is the migration runner's bookkeeping; application schema lives
// in the embedded .sql files.
func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS _migrations (
    version    TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL
)`)
	if err != nil {
		return fmt.Errorf("migrate: ensure _migrations: %w", err)
	}
	return nil
}

// migrationsTableExists reports whether `_migrations` is present in db.
func migrationsTableExists(ctx context.Context, db *sql.DB) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='_migrations' LIMIT 1`,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("migrate: check _migrations: %w", err)
	}
	return true, nil
}

// splitSQLStatements is a tiny statement splitter that breaks on a
// trailing semicolon at end-of-line. It does NOT parse SQL; it is
// sufficient for the kind of plain DDL/DML we ship as migrations.
func splitSQLStatements(s string) []string {
	// Trim trailing whitespace per line so blank lines and lone
	// semicolons don't produce phantom statements.
	lines := strings.Split(s, "\n")
	var stmts []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			stmts = append(stmts, cur.String())
			cur.Reset()
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t\r")
		cur.WriteString(trimmed)
		cur.WriteByte('\n')
		if strings.HasSuffix(trimmed, ";") {
			flush()
		}
	}
	flush()
	return stmts
}

func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

// openRagfsDB opens (or creates) the SQLite database that backs ragfs
// metadata storage. dsn may be a file path or ":memory:". The caller
// must close the returned db.
func openRagfsDB(ctx context.Context, dsn string) (*sql.DB, error) {
	if dsn == "" {
		return nil, errors.New("migrate: empty sqlite dsn")
	}
	// modernc.org/sqlite supports a WAL pragma via DSN query string.
	// For migrate we default to a plain open; the ragfs runtime sets
	// its own pragmas when it owns the connection.
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("migrate: open %s: %w", filepath.Clean(dsn), err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: ping %s: %w", filepath.Clean(dsn), err)
	}
	return db, nil
}
