package vectordb

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseQdrantURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		host    string
		port    int
		tls     bool
		wantErr bool
	}{
		{"localhost", "localhost", 6334, false, false},
		{"localhost:6334", "localhost", 6334, false, false},
		{"grpc://qdrant.example:6334", "qdrant.example", 6334, false, false},
		{"grpcs://qdrant.example:443", "qdrant.example", 443, true, false},
		{"https://qdrant.example", "qdrant.example", 6334, true, false},
		{"http://qdrant.example:8080", "qdrant.example", 8080, false, false},
		{"", "", 0, false, true},
		{"grpc://:1234", "", 1234, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			host, port, tls, err := parseQdrantURL(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.host, host)
			assert.Equal(t, tc.port, port)
			assert.Equal(t, tc.tls, tls)
		})
	}
}

// TestQdrantAdapterIntegration runs the full Qdrant adapter against a live
// server. Skipped by default; set OV_QDRANT_URL to enable (e.g.
// OV_QDRANT_URL=localhost:6334).
func TestQdrantAdapterIntegration(t *testing.T) {
	urlStr := os.Getenv("OV_QDRANT_URL")
	if urlStr == "" {
		t.Skip("set OV_QDRANT_URL to run Qdrant integration tests")
	}
	ctx := context.Background()
	a, err := NewQdrantAdapter(ctx, urlStr, "", "ov_")
	require.NoError(t, err)
	defer a.Close()

	const coll = "ov_test_acct__file"
	require.NoError(t, a.EnsureCollection(ctx, CollectionSchema{Name: coll, Dim: 3, Distance: "cosine"}))
	t.Cleanup(func() {
		_ = a.DropCollection(ctx, coll)
	})

	rows := []Vector{
		{ID: "q1", Embedding: []float32{1, 0, 0}, Metadata: map[string]any{"account": "test_acct", "kind": "file", "uri": "viking://test_acct/file/q1"}},
		{ID: "q2", Embedding: []float32{0, 1, 0}, Metadata: map[string]any{"account": "test_acct", "kind": "file", "uri": "viking://test_acct/file/q2"}},
		{ID: "q3", Embedding: []float32{0, 0, 1}, Metadata: map[string]any{"account": "test_acct", "kind": "dir", "uri": "viking://test_acct/dir/q3"}},
	}
	require.NoError(t, a.Upsert(ctx, coll, rows))

	n, err := a.Count(ctx, coll)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)

	res, err := a.Search(ctx, SearchParams{Collection: coll, Query: []float32{1, 0, 0}, TopK: 2})
	require.NoError(t, err)
	require.NotEmpty(t, res.Hits)
	assert.Equal(t, "q1", res.Hits[0].ID)

	// Filter by kind=dir.
	res, err = a.Search(ctx, SearchParams{
		Collection: coll,
		Query:      []float32{1, 0, 0},
		TopK:       10,
		Filter:     Filter{Kind: "dir"},
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "q3", res.Hits[0].ID)

	// Get single row by ID.
	got, err := a.Get(ctx, coll, "q2")
	require.NoError(t, err)
	assert.Equal(t, "q2", got.ID)
	assert.Equal(t, "test_acct", got.Metadata["account"])

	// Delete and verify.
	require.NoError(t, a.Delete(ctx, coll, []string{"q2"}))
	n, err = a.Count(ctx, coll)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	// Idempotent drop.
	require.NoError(t, a.DropCollection(ctx, coll))
}

// TestQdrantNewAdapterRejectsEmptyURL guards against silent misconfiguration.
func TestQdrantNewAdapterRejectsEmptyURL(t *testing.T) {
	_, err := NewQdrantAdapter(context.Background(), "", "", "ov_")
	require.Error(t, err)
}

// Compile-time assertion that QdrantAdapter satisfies CollectionAdapter.
var _ CollectionAdapter = (*QdrantAdapter)(nil)
