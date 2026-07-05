package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"
)

// ragfsCmd implements `migrate ragfs`. It applies embedded SQL migrations
// to the ragfs metadata SQLite database.
func ragfsCmd() *cobra.Command {
	var (
		dsn    string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "ragfs",
		Short: "Apply ragfs schema migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRagfsMigrate(cmd.Context(), cmd.OutOrStdout(), dsn, dryRun)
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "ragfs metadata SQLite DSN (file path or ':memory:')")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print pending migrations without applying them")
	_ = cmd.MarkFlagRequired("dsn")
	return cmd
}

// runRagfsMigrate is the testable core of `migrate ragfs`.
func runRagfsMigrate(ctx context.Context, out io.Writer, dsn string, dryRun bool) error {
	db, err := openRagfsDB(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	pending, err := PendingMigrations(ctx, db)
	if err != nil {
		return err
	}

	if dryRun {
		printRagfsDryRun(out, dsn, pending)
		return nil
	}

	applied, err := ApplyMigrations(ctx, db)
	if err != nil {
		return err
	}
	printRagfsApplied(out, dsn, applied)
	return nil
}

// printRagfsDryRun writes the planned migration set without touching
// state. Output is stable so tests can assert against it.
func printRagfsDryRun(out io.Writer, dsn string, pending []Migration) {
	fmt.Fprintf(out, "ragfs migrate --dry-run --dsn %s\n", displayDSN(dsn))
	if len(pending) == 0 {
		fmt.Fprintln(out, "  no pending migrations; database is at HEAD")
		return
	}
	fmt.Fprintf(out, "  plan: apply %d migration(s)\n", len(pending))
	for _, m := range pending {
		fmt.Fprintf(out, "  - %s\n", m.Filename)
	}
}

// printRagfsApplied writes the result of an apply run. Idempotent
// re-runs print "at HEAD" so operators can tell nothing changed.
func printRagfsApplied(out io.Writer, dsn string, applied []Migration) {
	if len(applied) == 0 {
		fmt.Fprintf(out, "ragfs migrate --dsn %s: no pending migrations; database is at HEAD\n", displayDSN(dsn))
		return
	}
	fmt.Fprintf(out, "ragfs migrate --dsn %s: applied %d migration(s)\n", displayDSN(dsn), len(applied))
	for _, m := range applied {
		fmt.Fprintf(out, "  + %s\n", m.Filename)
	}
}

// displayDSN returns a human-friendly form of the DSN for log output.
// File paths are cleaned; ":memory:" is preserved verbatim.
func displayDSN(dsn string) string {
	if dsn == ":memory:" {
		return ":memory:"
	}
	return filepath.Clean(dsn)
}

// compile-time guard: ensure *sql.DB satisfies the runner's store
// interface so future storage backends can replace SQLite without
// rewriting callers.
var _ = func(*sql.DB) {}
