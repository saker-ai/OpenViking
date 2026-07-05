// Package rerank defines the rerank adapter layer used by the retrieve
// pipeline to reorder candidate documents by query relevance.
//
// Every implementation (Cohere, Volcengine, local, stub) accepts the same
// query + []Document and returns the top-N matches. Concrete clients are
// constructed via the New* functions in their respective files; tests
// inject a recording HTTP transport or use the cosine-similarity LocalReranker.
//
// Tests MUST NOT make network calls.
package rerank

import (
	"context"
	"net/http"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// Document is one candidate to be reranked. Content is the text the model
// scores against the query. Metadata is opaque to the reranker but is
// preserved on the returned Document so callers can correlate hits with
// upstream records (e.g. a retrieve.Document URI).
type Document struct {
	ID       string         `json:"id"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Score is populated by Rerank; the input score is ignored.
	Score float64 `json:"score,omitempty"`
}

// Reranker is the provider-agnostic interface implemented by every adapter.
// Implementations must be safe for concurrent use.
//
// Rerank returns at most topN documents ordered by descending relevance
// score. If topN <= 0, the implementation returns all input documents in
// reranked order.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []Document, topN int) ([]Document, error)
}

// Doer is the minimal HTTP round-tripper interface used by every rerank
// client. *http.Client satisfies it; tests inject an httptest.Server
// recorder.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// wrapRerank annotates an error with the rerank business code.
func wrapRerank(err error) error {
	if err == nil {
		return nil
	}
	return domain.Wrap(domain.CodeRerankFailed, 502, err)
}

// ErrUnsupported is re-exported so adapters in this package can return a
// stable sentinel without importing domain in their constructors.
var ErrUnsupported = domain.ErrUnsupported

// httpOrDefault returns c when non-nil, otherwise a fresh *http.Client.
func httpOrDefault(c *http.Client) Doer {
	if c == nil {
		return &http.Client{}
	}
	return c
}
