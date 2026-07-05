package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/hibiken/asynq"

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/models/rerank"
	"github.com/saker-ai/ctxhub/internal/models/vlm"
	vectordb "github.com/saker-ai/ctxhub/internal/vectordb"
)

// probeConnectivity pings each configured backend. Each probe returns one
// ProbeResult with status ok/fail/skip and latency_ms measured around the
// remote call. Probes respect ctx timeout.
func probeConnectivity(ctx context.Context, cfg *config.Config, timeout time.Duration) []ProbeResult {
	if cfg == nil {
		return []ProbeResult{{Name: "connectivity", Status: StatusSkip, Detail: "config not loaded"}}
	}
	results := []ProbeResult{}
	results = append(results, probeVectordb(ctx, cfg, timeout))
	results = append(results, probeEmbedder(ctx, cfg, timeout))
	results = append(results, probeReranker(ctx, cfg, timeout))
	results = append(results, probeVLM(ctx, cfg, timeout))
	results = append(results, probeQueueFS(ctx, cfg, timeout))
	results = append(results, probeRAGFS(ctx, cfg, timeout))
	return results
}

func probeVectordb(ctx context.Context, cfg *config.Config, timeout time.Duration) ProbeResult {
	const name = "connectivity.vectordb"
	// Memory and local backends have no remote to ping; treat as skip with
	// a clear reason so the report still surfaces them.
	switch cfg.VectorDB.Backend {
	case "memory":
		return ProbeResult{Name: name, Status: StatusSkip, Detail: "memory backend (no remote)"}
	case "local":
		return ProbeResult{Name: name, Status: StatusSkip, Detail: "local backend (no remote)"}
	}
	lat, err := runWithTimeout(ctx, timeout, func(c context.Context) error {
		adapter, err := vectordb.NewAdapter(c, &cfg.VectorDB)
		if err != nil {
			return err
		}
		defer adapter.Close()
		_, lerr := adapter.ListCollections(c)
		return lerr
	})
	if err != nil {
		return ProbeResult{Name: name, Status: StatusFail, LatencyMS: lat, Error: err.Error()}
	}
	return ProbeResult{Name: name, Status: StatusOK, LatencyMS: lat, Detail: "list_collections ok"}
}

func probeEmbedder(ctx context.Context, cfg *config.Config, timeout time.Duration) ProbeResult {
	const name = "connectivity.embedder"
	if cfg.Embedder.Provider == "" {
		return ProbeResult{Name: name, Status: StatusSkip, Detail: "no embedder provider configured"}
	}
	cli, err := newEmbedderClient(cfg.Embedder, &http.Client{Timeout: timeout})
	if err != nil {
		return ProbeResult{Name: name, Status: StatusFail, Error: err.Error()}
	}
	lat, err := runWithTimeout(ctx, timeout, func(c context.Context) error {
		vecs, err := cli.Embed(c, []string{"ping"}, cfg.Embedder.Model)
		if err != nil {
			return err
		}
		if len(vecs) != 1 {
			return fmt.Errorf("embedder returned %d vectors, want 1", len(vecs))
		}
		return nil
	})
	if err != nil {
		// Unsupported providers (e.g. local hash stub) are reported as
		// skip rather than fail so a missing ping method does not crater
		// the run.
		if errors.Is(err, domain.ErrUnsupported) {
			return ProbeResult{Name: name, Status: StatusSkip, LatencyMS: lat, Detail: "no ping method: " + err.Error()}
		}
		return ProbeResult{Name: name, Status: StatusFail, LatencyMS: lat, Error: err.Error()}
	}
	return ProbeResult{Name: name, Status: StatusOK, LatencyMS: lat, Detail: "embed ok"}
}

func probeReranker(ctx context.Context, cfg *config.Config, timeout time.Duration) ProbeResult {
	const name = "connectivity.rerank"
	if cfg.Rerank.Provider == "" {
		return ProbeResult{Name: name, Status: StatusSkip, Detail: "no rerank provider configured"}
	}
	cli, err := newRerankerClient(cfg.Rerank, &http.Client{Timeout: timeout})
	if err != nil {
		return ProbeResult{Name: name, Status: StatusFail, Error: err.Error()}
	}
	lat, err := runWithTimeout(ctx, timeout, func(c context.Context) error {
		docs := []rerank.Document{
			{ID: "a", Content: "alpha"},
			{ID: "b", Content: "beta"},
		}
		_, err := cli.Rerank(c, "alpha", docs, 2)
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrUnsupported) {
			return ProbeResult{Name: name, Status: StatusSkip, LatencyMS: lat, Detail: "no ping method: " + err.Error()}
		}
		return ProbeResult{Name: name, Status: StatusFail, LatencyMS: lat, Error: err.Error()}
	}
	return ProbeResult{Name: name, Status: StatusOK, LatencyMS: lat, Detail: "rerank ok"}
}

func probeVLM(ctx context.Context, cfg *config.Config, timeout time.Duration) ProbeResult {
	const name = "connectivity.vlm"
	if cfg.VLM.Provider == "" {
		return ProbeResult{Name: name, Status: StatusSkip, Detail: "no vlm provider configured"}
	}
	cli, err := newVLMClient(cfg.VLM, &http.Client{Timeout: timeout})
	if err != nil {
		return ProbeResult{Name: name, Status: StatusFail, Error: err.Error()}
	}
	lat, err := runWithTimeout(ctx, timeout, func(c context.Context) error {
		req := vlm.ChatRequest{
			Model: cfg.VLM.Model,
			Messages: []vlm.Message{
				{Role: vlm.RoleUser, Content: "ping"},
			},
			MaxTokens: 1,
		}
		_, err := cli.Chat(c, req)
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrUnsupported) {
			return ProbeResult{Name: name, Status: StatusSkip, LatencyMS: lat, Detail: "no ping method: " + err.Error()}
		}
		return ProbeResult{Name: name, Status: StatusFail, LatencyMS: lat, Error: err.Error()}
	}
	return ProbeResult{Name: name, Status: StatusOK, LatencyMS: lat, Detail: "chat ok"}
}

func probeQueueFS(ctx context.Context, cfg *config.Config, timeout time.Duration) ProbeResult {
	const name = "connectivity.queuefs"
	if cfg.Queue.Backend != "redis" {
		return ProbeResult{Name: name, Status: StatusSkip, Detail: "queue backend " + cfg.Queue.Backend + " (no remote)"}
	}
	if cfg.Queue.Redis.Addr == "" {
		return ProbeResult{Name: name, Status: StatusSkip, Detail: "redis addr not configured"}
	}
	lat, err := runWithTimeout(ctx, timeout, func(c context.Context) error {
		// asynq.Client.Ping opens a Redis connection and issues PING.
		client := asynq.NewClient(asynq.RedisClientOpt{
			Addr:     cfg.Queue.Redis.Addr,
			Password: cfg.Queue.Redis.Password,
			DB:       cfg.Queue.Redis.DB,
		})
		defer client.Close()
		return client.Ping()
	})
	if err != nil {
		return ProbeResult{Name: name, Status: StatusFail, LatencyMS: lat, Error: err.Error()}
	}
	return ProbeResult{Name: name, Status: StatusOK, LatencyMS: lat, Detail: "redis PING ok"}
}

func probeRAGFS(ctx context.Context, cfg *config.Config, timeout time.Duration) ProbeResult {
	const name = "connectivity.ragfs"
	root := firstLocalMountPath(cfg.RAGFS)
	if root == "" {
		return ProbeResult{Name: name, Status: StatusSkip, Detail: "no local ragfs mount configured"}
	}
	lat, err := runWithTimeout(ctx, timeout, func(c context.Context) error {
		if _, err := os.Stat(root); err != nil {
			return fmt.Errorf("stat %s: %w", root, err)
		}
		// Write probe: create and remove a temp file to verify writability.
		probePath := filepath.Join(root, ".ov-doctor-probe")
		f, err := os.Create(probePath)
		if err != nil {
			return fmt.Errorf("write %s: %w", probePath, err)
		}
		_ = f.Close()
		_ = os.Remove(probePath)
		return nil
	})
	if err != nil {
		return ProbeResult{Name: name, Status: StatusFail, LatencyMS: lat, Error: err.Error()}
	}
	return ProbeResult{Name: name, Status: StatusOK, LatencyMS: lat, Detail: "stat+write ok", Extra: map[string]any{"root": root}}
}

// firstLocalMountPath returns the path of the first local backend mount,
// or "" when none is configured.
func firstLocalMountPath(ragfs config.RAGFSConfig) string {
	for _, m := range ragfs.Mounts {
		if m.Backend == "local" && m.Path != "" {
			return m.Path
		}
	}
	return ""
}
