// Package vectorize implements the offline batch vectorization pipeline
// exposed by the ctxhub-vectorize binary. It reads JSONL records
// (one {id, text, metadata} per line), batches them through an
// embedder.Embedder, and upserts the resulting vectors into a
// vectordb.CollectionAdapter.
//
// The pipeline is intentionally simple — no retry, no backoff, no
// cross-collection routing. Callers that need richer orchestration
// should use the ingest.Orchestrator instead. The binary exists for
// the offline-batch use case where a separate process replays a
// JSONL dump into the vector store without running the full agent
// stack.
package vectorize

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/saker-ai/ctxhub/internal/models/embedder"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)
// Record is one line of the input JSONL. ID must be non-empty; Text
// is the string sent to the embedder; Metadata is forwarded verbatim
// to vectordb.Vector.Metadata. A record with empty Text is still
// upserted (with a zero embedding) so callers can pre-register IDs.
type Record struct {
	ID       string         `json:"id"`
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Options controls a single Run invocation.
type Options struct {
	// Input is the JSONL file path. "-" means stdin.
	Input string
	// Collection is the vectordb collection name to upsert into.
	Collection string
	// BatchSize is the max records per embed+upsert round. Must be >0;
	// Run panics if not (caller should validate).
	BatchSize int
	// EmbedderModel is the model name passed to Embedder.Embed. For
	// providers that ignore the model parameter, this is informational.
	EmbedderModel string
	// Dimension is the vector dimensionality. Used for EnsureCollection
	// before the first upsert. When 0, the embedder's Dimensions()
	// result for EmbedderModel is used; when the embedder reports 0
	// too, Run returns an error.
	Dimension int
	// Distance is the vectordb distance metric: "cosine" (default),
	// "l2", or "ip".
	Distance string
}

// Stats reports what Run did. All fields are non-negative.
type Stats struct {
	RecordsRead    int
	RecordsUpserted int
	Batches        int
	BytesRead      int64
}

// Run reads records from opts.Input, batches them through emb, and
// upserts into coll. It returns when the input is exhausted or ctx
// is canceled. On error, the stats reflect partial progress up to the
// failure point.
//
// The function is safe to call from a test with a stub embedder and
// an in-memory vectordb collection — no network is touched by those
// backends. Real embedders/vectordbs will hit the network; callers
// should pass an http.Client with a sensible timeout.
func Run(ctx context.Context, emb embedder.Embedder, coll vectordb.CollectionAdapter, opts Options) (*Stats, error) {
	if opts.BatchSize <= 0 {
		return nil, fmt.Errorf("vectorize: batch size must be > 0, got %d", opts.BatchSize)
	}
	if opts.Collection == "" {
		return nil, fmt.Errorf("vectorize: collection name is required")
	}
	if emb == nil {
		return nil, fmt.Errorf("vectorize: embedder is nil")
	}
	if coll == nil {
		return nil, fmt.Errorf("vectorize: collection adapter is nil")
	}
	reader, err := openInput(opts.Input)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	// Ensure the collection exists before the first upsert. Memory
	// backends tolerate lazy creation; production backends (qdrant,
	// vikingdb) require an explicit EnsureCollection.
	dim := opts.Dimension
	if dim == 0 {
		dim = emb.Dimensions(opts.EmbedderModel)
	}
	if dim <= 0 {
		return nil, fmt.Errorf("vectorize: dimension unknown — set Options.Dimension or use an embedder that reports Dimensions()")
	}
	distance := opts.Distance
	if distance == "" {
		distance = "cosine"
	}
	if err := coll.EnsureCollection(context.Background(), vectordb.CollectionSchema{
		Name:     opts.Collection,
		Dim:      dim,
		Distance: distance,
	}); err != nil {
		return nil, fmt.Errorf("vectorize: ensure collection: %w", err)
	}
	stats := &Stats{}
	scanner := bufio.NewScanner(reader)
	// Allow long lines — embeddings metadata can be large.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	batch := make([]Record, 0, opts.BatchSize)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return stats, fmt.Errorf("vectorize: parse line %d: %w", stats.RecordsRead+1, err)
		}
		if rec.ID == "" {
			return stats, fmt.Errorf("vectorize: record at line %d has empty id", stats.RecordsRead+1)
		}
		stats.RecordsRead++
		batch = append(batch, rec)
		if len(batch) >= opts.BatchSize {
			n, err := flushBatch(ctx, emb, coll, opts, batch)
			stats.RecordsUpserted += n
			stats.Batches++
			if err != nil {
				return stats, err
			}
			batch = batch[:0]
		}
	}
	if err := scanner.Err(); err != nil {
		return stats, fmt.Errorf("vectorize: read: %w", err)
	}
	// Flush remaining.
	if len(batch) > 0 {
		n, err := flushBatch(ctx, emb, coll, opts, batch)
		stats.RecordsUpserted += n
		stats.Batches++
		if err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// openInput opens opts.Input as a file, or stdin when Input is "-".
// Returns the reader and the bytes read (best-effort; 0 for stdin
// until the read completes).
func openInput(path string) (io.ReadCloser, error) {
	if path == "" || path == "-" {
		return os.Stdin, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("vectorize: open %s: %w", path, err)
	}
	return f, nil
}

// flushBatch embeds the texts and upserts the resulting vectors.
// Returns the number of records upserted (len(recs) on success).
func flushBatch(ctx context.Context, emb embedder.Embedder, coll vectordb.CollectionAdapter, opts Options, recs []Record) (int, error) {
	texts := make([]string, len(recs))
	for i, r := range recs {
		texts[i] = r.Text
	}
	vecs, err := emb.Embed(ctx, texts, opts.EmbedderModel)
	if err != nil {
		return 0, fmt.Errorf("vectorize: embed batch: %w", err)
	}
	if len(vecs) != len(recs) {
		return 0, fmt.Errorf("vectorize: embed returned %d vectors for %d texts", len(vecs), len(recs))
	}
	rows := make([]vectordb.Vector, len(recs))
	for i, r := range recs {
		rows[i] = vectordb.Vector{
			ID:        r.ID,
			Embedding: vecs[i],
			Metadata:  r.Metadata,
		}
	}
	if err := coll.Upsert(ctx, opts.Collection, rows); err != nil {
		return 0, fmt.Errorf("vectorize: upsert batch: %w", err)
	}
	return len(recs), nil
}
