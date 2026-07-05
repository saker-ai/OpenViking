package migrate

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// vectordbCmd implements `migrate vectordb`. It constructs the configured
// CollectionAdapter and ensures the target collection exists with the
// current schema (Dim/Distance). For the local backend it is a no-op
// because the hnsw file format is self-versioning.
func vectordbCmd() *cobra.Command {
	var (
		configPath string
		account    string
		kind       string
		dim        int
		distance   string
		dryRun     bool
	)
	cmd := &cobra.Command{
		Use:   "vectordb",
		Short: "Apply vectordb schema migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVectordbMigrate(cmd.Context(), cmd.OutOrStdout(), configPath,
				account, kind, dim, distance, dryRun)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to ov.conf (empty = standard search)")
	cmd.Flags().StringVar(&account, "account", "", "tenant account slug (e.g. acme)")
	cmd.Flags().StringVar(&kind, "kind", "file", "resource kind (e.g. file)")
	cmd.Flags().IntVar(&dim, "dim", 0, "embedding dimension; 0 reads from config.Embedder.Dim")
	cmd.Flags().StringVar(&distance, "distance", "cosine", "distance metric: cosine|l2|ip")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the migration plan without applying it")
	_ = cmd.MarkFlagRequired("account")
	return cmd
}

// runVectordbMigrate is the testable core of `migrate vectordb`.
func runVectordbMigrate(ctx context.Context, out io.Writer, configPath, account, kind string,
	dim int, distance string, dryRun bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("migrate vectordb: %w", err)
	}
	if dim == 0 {
		dim = cfg.Embedder.Dim
	}
	if dim <= 0 {
		return fmt.Errorf("migrate vectordb: --dim or config.embedder.dim must be > 0")
	}
	if distance == "" {
		distance = "cosine"
	}
	if account == "" {
		return fmt.Errorf("migrate vectordb: --account is required")
	}
	if kind == "" {
		kind = "file"
	}

	collection, err := vectordb.CollectionName(cfg.VectorDB.CollectionPrefix, account, kind)
	if err != nil {
		return fmt.Errorf("migrate vectordb: %w", err)
	}
	schema := vectordb.CollectionSchema{
		Name:     collection,
		Dim:      dim,
		Distance: distance,
	}

	if dryRun {
		printVectordbDryRun(out, cfg.VectorDB.Backend, schema)
		return nil
	}

	adapter, err := vectordb.NewAdapter(ctx, &cfg.VectorDB)
	if err != nil {
		return fmt.Errorf("migrate vectordb: %w", err)
	}
	defer adapter.Close()

	// Local backend is a no-op: the hnsw SavedGraph format is self-versioning.
	// Memory backend has no persistence to migrate. For all others we call
	// Migrate when the adapter implements vectordb.Migrator, falling back to
	// EnsureCollection.
	if cfg.VectorDB.Backend == "local" || cfg.VectorDB.Backend == "memory" {
		fmt.Fprintf(out, "vectordb migrate: backend=%s no-op (self-versioning)\n", cfg.VectorDB.Backend)
		return nil
	}

	if m, ok := adapter.(vectordb.Migrator); ok {
		if err := m.Migrate(ctx, schema); err != nil {
			return fmt.Errorf("migrate vectordb: %w", err)
		}
		fmt.Fprintf(out, "vectordb migrate: backend=%s collection=%s migrated (Migrate)\n",
			cfg.VectorDB.Backend, collection)
		return nil
	}

	if err := adapter.EnsureCollection(ctx, schema); err != nil {
		return fmt.Errorf("migrate vectordb: %w", err)
	}
	fmt.Fprintf(out, "vectordb migrate: backend=%s collection=%s ensured\n",
		cfg.VectorDB.Backend, collection)
	return nil
}

// printVectordbDryRun writes the planned migration set without touching
// state. Output is stable so tests can assert against it.
func printVectordbDryRun(out io.Writer, backend string, schema vectordb.CollectionSchema) {
	fmt.Fprintf(out, "vectordb migrate --dry-run --backend %s\n", backend)
	switch backend {
	case "local", "memory":
		fmt.Fprintf(out, "  plan: no-op (backend %s is self-versioning)\n", backend)
	default:
		fmt.Fprintf(out, "  plan: ensure collection %s (dim=%d distance=%s)\n",
			schema.Name, schema.Dim, schema.Distance)
	}
}
