package vectorize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saker-ai/ctxhub/internal/models/embedder"
	"github.com/saker-ai/ctxhub/internal/vectordb"
)

// stubEmbedder returns deterministic vectors derived from the text hash.
type stubEmbedder struct{}

func (stubEmbedder) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if len(t) == 0 {
			out[i] = []float32{0, 0, 0}
			continue
		}
		out[i] = []float32{
			float32(len(t) % 256),
			float32(byte(t[0])),
			float32(byte(t[len(t)-1])),
		}
	}
	return out, nil
}

func (stubEmbedder) Dimensions(model string) int { return 3 }

// errorEmbedder always fails.
type errorEmbedder struct{}

func (errorEmbedder) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	return nil, fmt.Errorf("embedder offline")
}

func (errorEmbedder) Dimensions(model string) int { return 3 }

// shortEmbedder returns fewer vectors than texts.
type shortEmbedder struct{}

func (shortEmbedder) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	return [][]float32{{1, 2, 3}}, nil
}

func (shortEmbedder) Dimensions(model string) int { return 3 }

// writeJSONL writes records to a temp JSONL file and returns the path.
func writeJSONL(t *testing.T, recs []Record) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "in.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	for _, r := range recs {
		data, _ := json.Marshal(r)
		if _, err := f.Write(append(data, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return path
}

// TestRun_HappyPath verifies the full pipeline: read JSONL, embed,
// upsert.
func TestRun_HappyPath(t *testing.T) {
	recs := []Record{
		{ID: "a", Text: "hello", Metadata: map[string]any{"kind": "doc"}},
		{ID: "b", Text: "world", Metadata: map[string]any{"kind": "doc"}},
		{ID: "c", Text: "foo"},
	}
	path := writeJSONL(t, recs)
	coll := vectordb.NewMemoryAdapter()
	stats, err := Run(context.Background(), stubEmbedder{}, coll, Options{
		Input:         path,
		Collection:    "test",
		BatchSize:     2,
		EmbedderModel: "stub",
	})
	requireNoError(t, err)
	if stats.RecordsRead != 3 {
		t.Errorf("RecordsRead = %d, want 3", stats.RecordsRead)
	}
	if stats.RecordsUpserted != 3 {
		t.Errorf("RecordsUpserted = %d, want 3", stats.RecordsUpserted)
	}
	if stats.Batches != 2 {
		t.Errorf("Batches = %d, want 2 (batch of 2 + batch of 1)", stats.Batches)
	}
	// Verify the vectors landed in the collection.
	res, err := coll.Search(context.Background(), vectordb.SearchParams{
		Collection: "test",
		Query:      []float32{5, 'h', 'o'},
		TopK:       10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Hits) != 3 {
		t.Errorf("search returned %d hits, want 3", len(res.Hits))
	}
}

// TestRun_StdinInput verifies Input="-" reads from stdin.
func TestRun_StdinInput(t *testing.T) {
	// Build stdin content.
	var buf bytes.Buffer
	for _, r := range []Record{
		{ID: "x", Text: "alpha"},
		{ID: "y", Text: "beta"},
	} {
		data, _ := json.Marshal(r)
		buf.Write(append(data, '\n'))
	}
	// Swap os.Stdin for the test.
	orig := os.Stdin
	defer func() { os.Stdin = orig }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r
	go func() {
		_, _ = w.Write(buf.Bytes())
		_ = w.Close()
	}()
	coll := vectordb.NewMemoryAdapter()
	stats, err := Run(context.Background(), stubEmbedder{}, coll, Options{
		Input:      "-",
		Collection: "test",
		BatchSize:  10,
	})
	requireNoError(t, err)
	if stats.RecordsRead != 2 {
		t.Errorf("RecordsRead = %d, want 2", stats.RecordsRead)
	}
}

// TestRun_EmptyInput verifies an empty file produces zero stats and
// no error.
func TestRun_EmptyInput(t *testing.T) {
	path := writeJSONL(t, nil)
	coll := vectordb.NewMemoryAdapter()
	stats, err := Run(context.Background(), stubEmbedder{}, coll, Options{
		Input:      path,
		Collection: "test",
		BatchSize:  10,
	})
	requireNoError(t, err)
	if stats.RecordsRead != 0 || stats.RecordsUpserted != 0 || stats.Batches != 0 {
		t.Errorf("expected zero stats, got %+v", stats)
	}
}

// TestRun_BlankLinesSkipped verifies blank lines are skipped without
// error.
func TestRun_BlankLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in.jsonl")
	content := `{"id":"a","text":"hi"}

   {"id":"b","text":"yo"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	coll := vectordb.NewMemoryAdapter()
	stats, err := Run(context.Background(), stubEmbedder{}, coll, Options{
		Input:      path,
		Collection: "test",
		BatchSize:  10,
	})
	requireNoError(t, err)
	if stats.RecordsRead != 2 {
		t.Errorf("RecordsRead = %d, want 2 (blank line skipped)", stats.RecordsRead)
	}
}

// TestRun_EmptyIDRejected verifies a record with empty ID returns an
// error.
func TestRun_EmptyIDRejected(t *testing.T) {
	path := writeJSONL(t, []Record{{ID: "", Text: "no id"}})
	coll := vectordb.NewMemoryAdapter()
	_, err := Run(context.Background(), stubEmbedder{}, coll, Options{
		Input:      path,
		Collection: "test",
		BatchSize:  10,
	})
	if err == nil {
		t.Fatalf("expected error for empty ID")
	}
	if !strings.Contains(err.Error(), "empty id") {
		t.Errorf("error = %v, want 'empty id'", err)
	}
}

// TestRun_MalformedJSONRejected verifies a malformed JSON line returns
// an error pointing at the line number.
func TestRun_MalformedJSONRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in.jsonl")
	content := `{"id":"a","text":"hi"}
{not valid json}
{"id":"b","text":"yo"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	coll := vectordb.NewMemoryAdapter()
	_, err := Run(context.Background(), stubEmbedder{}, coll, Options{
		Input:      path,
		Collection: "test",
		BatchSize:  10,
	})
	if err == nil {
		t.Fatalf("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error = %v, want 'line 2'", err)
	}
}

// TestRun_EmbedErrorPropagates verifies embedder errors are returned.
func TestRun_EmbedErrorPropagates(t *testing.T) {
	path := writeJSONL(t, []Record{{ID: "a", Text: "hi"}})
	coll := vectordb.NewMemoryAdapter()
	_, err := Run(context.Background(), errorEmbedder{}, coll, Options{
		Input:      path,
		Collection: "test",
		BatchSize:  10,
	})
	if err == nil {
		t.Fatalf("expected error from embedder")
	}
	if !strings.Contains(err.Error(), "embedder offline") {
		t.Errorf("error = %v, want 'embedder offline'", err)
	}
}

// TestRun_CountMismatch verifies a mismatched vector count is caught.
func TestRun_CountMismatch(t *testing.T) {
	path := writeJSONL(t, []Record{
		{ID: "a", Text: "hi"},
		{ID: "b", Text: "yo"},
	})
	coll := vectordb.NewMemoryAdapter()
	_, err := Run(context.Background(), shortEmbedder{}, coll, Options{
		Input:      path,
		Collection: "test",
		BatchSize:  10,
	})
	if err == nil {
		t.Fatalf("expected error for count mismatch")
	}
	if !strings.Contains(err.Error(), "1 vectors for 2 texts") {
		t.Errorf("error = %v, want '1 vectors for 2 texts'", err)
	}
}

// TestRun_CtxCanceled verifies ctx cancellation stops the pipeline.
func TestRun_CtxCanceled(t *testing.T) {
	path := writeJSONL(t, []Record{{ID: "a", Text: "hi"}})
	coll := vectordb.NewMemoryAdapter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, stubEmbedder{}, coll, Options{
		Input:      path,
		Collection: "test",
		BatchSize:  10,
	})
	if err == nil {
		t.Fatalf("expected ctx canceled error")
	}
}

// TestRun_InvalidBatchSize verifies batch size 0 is rejected.
func TestRun_InvalidBatchSize(t *testing.T) {
	_, err := Run(context.Background(), stubEmbedder{}, vectordb.NewMemoryAdapter(), Options{
		Input:      "-",
		Collection: "test",
		BatchSize:  0,
	})
	if err == nil {
		t.Fatalf("expected error for batch size 0")
	}
	if !strings.Contains(err.Error(), "batch size") {
		t.Errorf("error = %v", err)
	}
}

// TestRun_EmptyCollection verifies empty collection name is rejected.
func TestRun_EmptyCollection(t *testing.T) {
	_, err := Run(context.Background(), stubEmbedder{}, vectordb.NewMemoryAdapter(), Options{
		Input:      "-",
		Collection: "",
		BatchSize:  10,
	})
	if err == nil {
		t.Fatalf("expected error for empty collection")
	}
	if !strings.Contains(err.Error(), "collection name") {
		t.Errorf("error = %v", err)
	}
}

// TestRun_NilEmbedder verifies nil embedder is rejected.
func TestRun_NilEmbedder(t *testing.T) {
	_, err := Run(context.Background(), nil, vectordb.NewMemoryAdapter(), Options{
		Input:      "-",
		Collection: "test",
		BatchSize:  10,
	})
	if err == nil {
		t.Fatalf("expected error for nil embedder")
	}
	if !strings.Contains(err.Error(), "embedder is nil") {
		t.Errorf("error = %v", err)
	}
}

// TestRun_NilCollection verifies nil collection adapter is rejected.
func TestRun_NilCollection(t *testing.T) {
	_, err := Run(context.Background(), stubEmbedder{}, nil, Options{
		Input:      "-",
		Collection: "test",
		BatchSize:  10,
	})
	if err == nil {
		t.Fatalf("expected error for nil collection")
	}
	if !strings.Contains(err.Error(), "collection adapter is nil") {
		t.Errorf("error = %v", err)
	}
}

// TestRun_FileNotFound verifies a missing input file returns an error.
func TestRun_FileNotFound(t *testing.T) {
	_, err := Run(context.Background(), stubEmbedder{}, vectordb.NewMemoryAdapter(), Options{
		Input:      "/nonexistent/path.jsonl",
		Collection: "test",
		BatchSize:  10,
	})
	if err == nil {
		t.Fatalf("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "open") {
		t.Errorf("error = %v", err)
	}
}

// TestRun_EmptyText verifies a record with empty Text is still
// upserted (with a zero embedding from the embedder).
func TestRun_EmptyText(t *testing.T) {
	path := writeJSONL(t, []Record{{ID: "a", Text: ""}})
	coll := vectordb.NewMemoryAdapter()
	stats, err := Run(context.Background(), stubEmbedder{}, coll, Options{
		Input:      path,
		Collection: "test",
		BatchSize:  10,
	})
	requireNoError(t, err)
	if stats.RecordsUpserted != 1 {
		t.Errorf("RecordsUpserted = %d, want 1", stats.RecordsUpserted)
	}
}

// TestRun_BatchSizeOne verifies batch size 1 produces N batches for N
// records.
func TestRun_BatchSizeOne(t *testing.T) {
	recs := []Record{
		{ID: "a", Text: "x"},
		{ID: "b", Text: "y"},
		{ID: "c", Text: "z"},
	}
	path := writeJSONL(t, recs)
	coll := vectordb.NewMemoryAdapter()
	stats, err := Run(context.Background(), stubEmbedder{}, coll, Options{
		Input:      path,
		Collection: "test",
		BatchSize:  1,
	})
	requireNoError(t, err)
	if stats.Batches != 3 {
		t.Errorf("Batches = %d, want 3", stats.Batches)
	}
	if stats.RecordsUpserted != 3 {
		t.Errorf("RecordsUpserted = %d, want 3", stats.RecordsUpserted)
	}
}

// requireNoError fails the test if err is non-nil.
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ensure embedder.Embedder is satisfied by stubs (compile-time check).
var _ embedder.Embedder = stubEmbedder{}
var _ embedder.Embedder = errorEmbedder{}
var _ embedder.Embedder = shortEmbedder{}
