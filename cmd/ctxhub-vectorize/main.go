// Command openviking-vectorize is the offline batch vectorization
// service. It reads JSONL records ({id, text, metadata} per line),
// batches them through the configured embedder, and upserts the
// resulting vectors into the configured vectordb collection.
//
// Usage:
//
//	openviking-vectorize --config ov.conf --input docs.jsonl --collection my-docs --batch-size 64
//
// Input format (one JSON object per line):
//
//	{"id": "doc-1", "text": "the text to embed", "metadata": {"kind": "doc"}}
//
// Flags:
//
//	--config       path to ov.conf (default search order applies)
//	--input        JSONL path; "-" means stdin
//	--collection   vectordb collection name (required)
//	--batch-size   records per embed+upsert round (default 64)
//	--dimension    override embedding dimension (default: from embedder)
//	--distance     vectordb distance metric: cosine|l2|ip (default cosine)
//	--model        override embedder model from config
//	--version      print version and exit
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/models/embedder"
	"github.com/saker-ai/ctxhub/internal/version"
	"github.com/saker-ai/ctxhub/internal/vectorize"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "openviking-vectorize:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "", "path to ov.conf (default search order applies)")
	input := flag.String("input", "-", "JSONL path; '-' means stdin")
	collection := flag.String("collection", "", "vectordb collection name (required)")
	batchSize := flag.Int("batch-size", 64, "records per embed+upsert round")
	dimension := flag.Int("dimension", 0, "override embedding dimension (default: from embedder)")
	distance := flag.String("distance", "cosine", "vectordb distance metric: cosine|l2|ip")
	model := flag.String("model", "", "override embedder model from config")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return nil
	}

	if *collection == "" {
		return errors.New("--collection is required")
	}
	if *batchSize <= 0 {
		return fmt.Errorf("--batch-size must be > 0, got %d", *batchSize)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if *model != "" {
		cfg.Embedder.Model = *model
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("openviking-vectorize starting",
		"input", *input,
		"collection", *collection,
		"batch_size", *batchSize,
		"embedder_provider", cfg.Embedder.Provider,
		"embedder_model", cfg.Embedder.Model,
		"vectordb_backend", cfg.VectorDB.Backend,
	)

	emb, err := newEmbedder(cfg.Embedder)
	if err != nil {
		return fmt.Errorf("build embedder: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	coll, err := vectordb.NewAdapter(ctx, &cfg.VectorDB)
	if err != nil {
		return fmt.Errorf("build vectordb adapter: %w", err)
	}

	stats, err := vectorize.Run(ctx, emb, coll, vectorize.Options{
		Input:         *input,
		Collection:    *collection,
		BatchSize:     *batchSize,
		EmbedderModel: cfg.Embedder.Model,
		Dimension:     *dimension,
		Distance:      *distance,
	})
	if err != nil {
		return fmt.Errorf("pipeline: %w (stats: %+v)", err, stats)
	}
	logger.Info("openviking-vectorize done",
		"records_read", stats.RecordsRead,
		"records_upserted", stats.RecordsUpserted,
		"batches", stats.Batches,
	)
	return nil
}

// newEmbedder builds an embedder.Embedder from config.EmbedderConfig.
// Mirrors the doctor/server switch — kept local to this binary so the
// cmd doesn't pull in the doctor package's transitive deps.
func newEmbedder(cfg config.EmbedderConfig) (embedder.Embedder, error) {
	switch cfg.Provider {
	case "openai", "litellm":
		return embedder.NewOpenAI(cfg.APIBase, cfg.APIKey, nil), nil
	case "volcengine":
		return embedder.NewVolcengine(cfg, nil), nil
	case "dashscope":
		return embedder.NewDashscope(cfg, nil), nil
	case "local":
		return embedder.NewLocal(cfg, nil), nil
	default:
		return nil, fmt.Errorf("embedder: provider %q not supported by openviking-vectorize", cfg.Provider)
	}
}
