// Package rerank — local in-process reranker.
//
// LocalReranker scores documents against a query using cosine similarity
// over hash-based embeddings (the same hash fallback as
// internal/models/embedder/local.go). This is NOT a cross-encoder: it is
// a deterministic baseline that lets the retrieve pipeline run end-to-end
// in tests and offline dev without an external API.
//
// The score is computed as the cosine of the query vector and each
// document vector. Documents are returned in descending score order.
//
// A real cross-encoder can be plugged in by setting CrossEncoder to a
// non-nil implementation (e.g. a local ONNX model served via sidecar);
// that path is optional and out of scope for P7.
package rerank

import (
	"context"
	"fmt"
	"sort"

	"github.com/saker-ai/ctxhub/internal/models/embedder"
)

// LocalReranker is a Reranker that scores documents via cosine similarity
// over hash embeddings. It is the default reranker for tests and offline
// dev when no provider is configured.
type LocalReranker struct {
	// Dim is the dimension of the hash embedding. Default 768.
	Dim int
	// CrossEncoder, when non-nil, overrides the cosine fallback with a
	// real cross-encoder. Optional.
	CrossEncoder Reranker
}

// NewLocal constructs a LocalReranker with the default dim.
func NewLocal() *LocalReranker { return &LocalReranker{Dim: 768} }

// Rerank scores docs against query via cosine similarity and returns the
// topN hits in descending order. If topN <= 0, all docs are returned.
func (r *LocalReranker) Rerank(ctx context.Context, query string, docs []Document, topN int) ([]Document, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if r.CrossEncoder != nil {
		return r.CrossEncoder.Rerank(ctx, query, docs, topN)
	}
	if query == "" {
		return nil, fmt.Errorf("rerank.local: query is required")
	}
	dim := r.Dim
	if dim <= 0 {
		dim = 768
	}
	qv := embedHash(query, dim)
	scored := make([]Document, len(docs))
	for i, d := range docs {
		dv := embedHash(d.Content, dim)
		scored[i] = d
		scored[i].Score = cosine(qv, dv)
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})
	if topN > 0 && topN < len(scored) {
		scored = scored[:topN]
	}
	return scored, nil
}

// embedHash is a thin wrapper around embedder.LocalClient to produce a
// deterministic hash embedding. We construct a fresh LocalClient on each
// call to avoid sharing state — the hash fallback is stateless.
func embedHash(text string, dim int) []float32 {
	c := embedder.LocalConfigFromBaseURL("", dim)
	vec, _ := c.Embed(context.Background(), []string{text}, "")
	if len(vec) == 0 {
		return make([]float32, dim)
	}
	return vec[0]
}

// cosine returns the cosine similarity of two float32 vectors. Returns 0
// when either vector is zero-length or dimensions mismatch.
func cosine(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, na, nb float32
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float64(dot) / float64(sqrtF32(na*nb))
}

// sqrtF32 is a float32-only sqrt (Newton's method).
func sqrtF32(x float32) float32 {
	if x <= 0 {
		return 0
	}
	g := x
	for i := 0; i < 6; i++ {
		g = 0.5 * (g + x/g)
	}
	return g
}

// Compile-time assertion that LocalReranker satisfies Reranker.
var _ Reranker = (*LocalReranker)(nil)
