package vectordb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// NewAdapter builds the CollectionAdapter selected by cfg.VectorDB.Backend.
//
// Supported backends:
//   - "memory":     in-memory brute-force cosine (dev/tests; data lost on Close)
//   - "local":      on-disk HNSW via github.com/coder/hnsw (no cgo)
//   - "qdrant":     remote Qdrant cluster via gRPC (github.com/qdrant/go-client)
//   - "opengauss":  OpenGauss/pgvector via github.com/jackc/pgx/v5
//   - "volcengine": Volcengine Ark vector store via HTTP (Bearer auth)
//   - "http":       generic REST backend, per-op configurable URLs
//   - "vikingdb":   Volcengine VikingDB via hand-rolled V4-signed HTTP
//     (see vikingdb.go). P1 review item: replace with
//     github.com/volcengine/volcengine-go-sdk/service/vikingdb once
//     the SDK is wired in go.mod.
//
// Backends not yet implemented return an error wrapping domain.ErrInternal.
func NewAdapter(ctx context.Context, cfg *config.VectorDBConfig) (CollectionAdapter, error) {
	if cfg == nil {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vectordb: nil config"))
	}
	switch cfg.Backend {
	case "memory":
		return NewMemoryAdapter(), nil
	case "local":
		path := cfg.Local.Path
		if path == "" {
			path = "./data/vectordb"
		}
		if err := os.MkdirAll(filepath.Clean(path), 0o755); err != nil {
			return nil, domain.Wrap(domain.CodeVectorDBError, 500,
				fmt.Errorf("vectordb: mkdir %s: %w", path, err))
		}
		return NewLocalAdapter(path, cfg.CollectionPrefix)
	case "qdrant":
		return NewQdrantAdapter(ctx, cfg.Qdrant.URL, cfg.Qdrant.APIKey, cfg.CollectionPrefix)
	case "opengauss":
		return NewOpenGaussAdapter(ctx, cfg.OpenGauss)
	case "volcengine":
		return NewVolcengineAdapter(ctx, cfg.Volcengine, cfg.CollectionPrefix, nil)
	case "http":
		return NewHTTPAdapter(ctx, cfg.HTTP, nil)
	case "vikingdb":
		return NewVikingDBAdapter(ctx, cfg.VikingDB, cfg.CollectionPrefix)
	default:
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("vectordb: unknown backend %q", cfg.Backend))
	}
}
