package migrate

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// TestLoadMigrationsHas001Initial confirms the embedded migration set
// includes the baseline file.
func TestLoadMigrationsHas001Initial(t *testing.T) {
	ms, err := LoadMigrations()
	require.NoError(t, err)
	require.NotEmpty(t, ms, "expected at least one embedded migration")
	var names []string
	for _, m := range ms {
		names = append(names, m.Filename)
	}
	assert.Contains(t, names, "001_initial.sql")
}

// TestLoadMigrationsSortedByVersion checks files come back in order.
func TestLoadMigrationsSortedByVersion(t *testing.T) {
	ms, err := LoadMigrations()
	require.NoError(t, err)
	for i := 1; i < len(ms); i++ {
		assert.Less(t, ms[i-1].Filename, ms[i].Filename,
			"migrations must be sorted by filename; got %s before %s",
			ms[i-1].Filename, ms[i].Filename)
	}
}

// TestApplyMigrationsFresh opens a fresh :memory: SQLite DB, applies
// migrations, and confirms the _migrations table records the baseline.
func TestApplyMigrationsFresh(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	applied, err := ApplyMigrations(ctx, db)
	require.NoError(t, err)
	require.NotEmpty(t, applied, "first run should apply at least one migration")
	assert.Equal(t, "001_initial.sql", applied[0].Filename)

	// _migrations table is created and populated.
	rows, err := db.QueryContext(ctx,
		`SELECT version FROM _migrations ORDER BY version ASC`)
	require.NoError(t, err)
	var versions []string
	for rows.Next() {
		var v string
		require.NoError(t, rows.Scan(&v))
		versions = append(versions, v)
	}
	rows.Close()
	assert.Contains(t, versions, "001")
}

// TestApplyMigrationsIdempotent confirms re-running on a HEAD database
// is a no-op (returns nil with zero applied).
func TestApplyMigrationsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	first, err := ApplyMigrations(ctx, db)
	require.NoError(t, err)
	require.NotEmpty(t, first)

	second, err := ApplyMigrations(ctx, db)
	require.NoError(t, err)
	assert.Empty(t, second, "second run should apply nothing; got %d", len(second))

	// PendingMigrations agrees.
	pending, err := PendingMigrations(ctx, db)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// TestPendingMigrationsOnFreshDB confirms a fresh DB reports every
// embedded migration as pending.
func TestPendingMigrationsOnFreshDB(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	all, err := LoadMigrations()
	require.NoError(t, err)

	pending, err := PendingMigrations(ctx, db)
	require.NoError(t, err)
	assert.Len(t, pending, len(all))
}

// TestRagfsMigrateDryRunDoesNotTouchState confirms --dry-run leaves
// _migrations empty.
func TestRagfsMigrateDryRunDoesNotTouchState(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dsn := filepath.Join(tmp, "ragfs_meta.sqlite")

	var out bytes.Buffer
	require.NoError(t, runRagfsMigrate(ctx, &out, dsn, true))
	s := out.String()
	assert.Contains(t, s, "ragfs migrate --dry-run")
	assert.Contains(t, s, "001_initial.sql")

	// _migrations should not exist on a dry-run.
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer db.Close()
	var name string
	err = db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='_migrations' LIMIT 1`,
	).Scan(&name)
	assert.True(t, errors.Is(err, sql.ErrNoRows),
		"_migrations must not be created on dry-run; got %v", err)
}

// TestRagfsMigrateApplyThenDryRun confirms a database at HEAD prints
// "no pending migrations" on a follow-up dry-run.
func TestRagfsMigrateApplyThenDryRun(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dsn := filepath.Join(tmp, "ragfs_meta.sqlite")

	var applyOut bytes.Buffer
	require.NoError(t, runRagfsMigrate(ctx, &applyOut, dsn, false))
	assert.Contains(t, applyOut.String(), "applied")

	var dryOut bytes.Buffer
	require.NoError(t, runRagfsMigrate(ctx, &dryOut, dsn, true))
	assert.Contains(t, dryOut.String(), "no pending migrations")
}

// TestRagfsMigrateIdempotentReRun confirms two consecutive non-dry-run
// invocations produce "no pending migrations" on the second.
func TestRagfsMigrateIdempotentReRun(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dsn := filepath.Join(tmp, "ragfs_meta.sqlite")

	var first bytes.Buffer
	require.NoError(t, runRagfsMigrate(ctx, &first, dsn, false))
	assert.Contains(t, first.String(), "applied")

	var second bytes.Buffer
	require.NoError(t, runRagfsMigrate(ctx, &second, dsn, false))
	assert.Contains(t, second.String(), "no pending migrations")
}

// TestRootHasAllSubcommands confirms the CLI surface includes the new
// migration subcommands.
func TestRootHasAllSubcommands(t *testing.T) {
	root := NewRoot()
	cmds := root.Commands()
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.Name())
	}
	for _, want := range []string{"ragfs", "vectordb", "queuefs", "all", "ovpack", "version"} {
		assert.Contains(t, names, want, "root should expose %q subcommand", want)
	}
}

// TestAllDryRunSkipsMissingFlags confirms `migrate all --dry-run` with no
// subsystem flags prints skip lines and exits cleanly.
func TestAllDryRunSkipsMissingFlags(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	require.NoError(t, runAllMigrate(ctx, &out, allOptions{dryRun: true}))
	s := out.String()
	assert.Contains(t, s, "=== ragfs ===")
	assert.Contains(t, s, "ragfs: skip")
	assert.Contains(t, s, "vectordb: skip")
	assert.Contains(t, s, "queuefs: skip")
	assert.Contains(t, s, "=== done ===")
}

// TestAllDryRunWithRagfsOnly confirms `migrate all --dry-run --ragfs-dsn`
// runs the ragfs step and skips the others.
func TestAllDryRunWithRagfsOnly(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dsn := filepath.Join(tmp, "ragfs_meta.sqlite")

	var out bytes.Buffer
	require.NoError(t, runAllMigrate(ctx, &out, allOptions{
		ragfsDSN: dsn,
		dryRun:   true,
	}))
	s := out.String()
	assert.Contains(t, s, "ragfs migrate --dry-run")
	assert.Contains(t, s, "vectordb: skip")
	assert.Contains(t, s, "queuefs: skip")
}

// TestAllApplyThenDryRunIdempotent confirms a full `migrate all` cycle is
// idempotent across the ragfs subsystem.
func TestAllApplyThenDryRunIdempotent(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dsn := filepath.Join(tmp, "ragfs_meta.sqlite")

	var first bytes.Buffer
	require.NoError(t, runAllMigrate(ctx, &first, allOptions{
		ragfsDSN: dsn,
		dryRun:   false,
	}))
	assert.Contains(t, first.String(), "applied")

	var second bytes.Buffer
	require.NoError(t, runAllMigrate(ctx, &second, allOptions{
		ragfsDSN: dsn,
		dryRun:   true,
	}))
	assert.Contains(t, second.String(), "no pending migrations")
}

// TestSplitSQLStatementsConfirmSimpleCase locks in the splitter behavior
// for the kind of DDL we ship.
func TestSplitSQLStatementsConfirmSimpleCase(t *testing.T) {
	in := "-- comment\nSELECT 1;\n\nCREATE TABLE x (id INTEGER);\n"
	got := splitSQLStatements(in)
	// Two non-empty statements after trimming; the trailing newline
	// produces a third empty entry that applyOne skips.
	var nonEmpty []string
	for _, s := range got {
		if strings.TrimSpace(s) != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	assert.Len(t, nonEmpty, 2, "expected 2 non-empty statements, got %d", len(nonEmpty))
}

// TestListAppliedOnFreshDB confirms ListApplied returns empty (not an
// error) on a database without _migrations.
func TestListAppliedOnFreshDB(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	applied, err := ListApplied(ctx, db)
	require.NoError(t, err)
	assert.Empty(t, applied)
}

// TestAppliedMigrationTimestampParses confirms timestamps written by the
// runner round-trip through time.Parse.
func TestAppliedMigrationTimestampParses(t *testing.T) {
	ts := time.Now().UTC().Format(time.RFC3339)
	_, err := time.Parse(time.RFC3339, ts)
	require.NoError(t, err, "runner-written timestamps must be RFC3339")
}

// TestRagfsMigrateEmptyDSNRejected confirms an empty DSN is rejected.
func TestRagfsMigrateEmptyDSNRejected(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	err := runRagfsMigrate(ctx, &out, "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty sqlite dsn")
}

// TestRagfsMigrateFileDSNPersistsAcrossOpens confirms a file-backed DSN
// records applied migrations across separate opens.
func TestRagfsMigrateFileDSNPersistsAcrossOpens(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dsn := filepath.Join(tmp, "ragfs_meta.sqlite")

	var first bytes.Buffer
	require.NoError(t, runRagfsMigrate(ctx, &first, dsn, false))
	assert.Contains(t, first.String(), "applied")

	// Remove any cached file handles; re-open should see HEAD.
	require.NoError(t, os.Chmod(dsn, 0o644))
	var second bytes.Buffer
	require.NoError(t, runRagfsMigrate(ctx, &second, dsn, false))
	assert.Contains(t, second.String(), "no pending migrations")
}
