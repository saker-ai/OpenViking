package migrate

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// allCmd implements `migrate all` (the default when no subcommand is
// given). It runs ragfs, vectordb, and queuefs migrations in order and
// exits non-zero on the first failure.
func allCmd() *cobra.Command {
	var (
		configPath string
		ragfsDSN   string
		account    string
		kind       string
		dim        int
		distance   string
		redisAddr  string
		redisPW    string
		redisDB    int
		flushStale bool
		dryRun     bool
	)
	cmd := &cobra.Command{
		Use:   "all",
		Short: "Run ragfs, vectordb, and queuefs migrations in order",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAllMigrate(cmd.Context(), cmd.OutOrStdout(), allOptions{
				configPath: configPath,
				ragfsDSN:   ragfsDSN,
				account:    account,
				kind:       kind,
				dim:        dim,
				distance:   distance,
				redisAddr:  redisAddr,
				redisPW:    redisPW,
				redisDB:    redisDB,
				flushStale: flushStale,
				dryRun:     dryRun,
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to ov.conf (empty = standard search)")
	cmd.Flags().StringVar(&ragfsDSN, "ragfs-dsn", "", "ragfs metadata SQLite DSN")
	cmd.Flags().StringVar(&account, "account", "", "tenant account slug (required for vectordb)")
	cmd.Flags().StringVar(&kind, "kind", "file", "resource kind")
	cmd.Flags().IntVar(&dim, "dim", 0, "embedding dimension; 0 reads from config.Embedder.Dim")
	cmd.Flags().StringVar(&distance, "distance", "cosine", "distance metric: cosine|l2|ip")
	cmd.Flags().StringVar(&redisAddr, "redis-addr", "", "redis host:port (required for queuefs)")
	cmd.Flags().StringVar(&redisPW, "redis-password", "", "redis password (optional)")
	cmd.Flags().IntVar(&redisDB, "redis-db", 0, "redis logical db index")
	cmd.Flags().BoolVar(&flushStale, "flush-stale", false, "delete stale queuefs keys")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print plan without applying changes")
	return cmd
}

// allOptions is the parameter bundle for runAllMigrate. It is a struct so
// the test harness can construct it inline without invoking cobra.
type allOptions struct {
	configPath string
	ragfsDSN   string
	account    string
	kind       string
	dim        int
	distance   string
	redisAddr  string
	redisPW    string
	redisDB    int
	flushStale bool
	dryRun     bool
}

// runAllMigrate runs the three migration steps in order. Each step prints
// its own header so operators can see which subsystem is currently active.
func runAllMigrate(ctx context.Context, out io.Writer, opts allOptions) error {
	steps := []struct {
		name string
		fn   func(context.Context, io.Writer) error
	}{
		{"ragfs", func(c context.Context, w io.Writer) error {
			if opts.ragfsDSN == "" {
				fmt.Fprintln(w, "ragfs: skip (no --ragfs-dsn)")
				return nil
			}
			return runRagfsMigrate(c, w, opts.ragfsDSN, opts.dryRun)
		}},
		{"vectordb", func(c context.Context, w io.Writer) error {
			if opts.account == "" {
				fmt.Fprintln(w, "vectordb: skip (no --account)")
				return nil
			}
			return runVectordbMigrate(c, w, opts.configPath, opts.account,
				opts.kind, opts.dim, opts.distance, opts.dryRun)
		}},
		{"queuefs", func(c context.Context, w io.Writer) error {
			if opts.redisAddr == "" {
				fmt.Fprintln(w, "queuefs: skip (no --redis-addr)")
				return nil
			}
			return runQueuefsMigrate(c, w, opts.redisAddr, opts.redisPW,
				opts.redisDB, opts.flushStale, opts.dryRun)
		}},
	}

	for _, s := range steps {
		fmt.Fprintf(out, "=== %s ===\n", s.name)
		if err := s.fn(ctx, out); err != nil {
			return fmt.Errorf("migrate all: %s: %w", s.name, err)
		}
	}
	fmt.Fprintln(out, "=== done ===")
	return nil
}
