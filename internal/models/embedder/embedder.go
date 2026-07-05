// Package embedder defines the embedding adapter layer used by ingest and
// retrieve pipelines.
//
// Every implementation (OpenAI, Volcengine, local, stub) returns float32
// vectors of a provider-specific dimension. Concrete clients are constructed
// via the New* functions in their respective files; tests inject a
// recording HTTP transport or use LocalEmbedder's hash stub to avoid
// network calls.
//
// Tests MUST NOT make network calls.
package embedder

import (
	"context"
	"net/http"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// Embedder is the provider-agnostic embedding interface implemented by every
// adapter. Implementations must be safe for concurrent use.
//
// Embed returns one vector per input text, in matching order. Empty input
// slices return an empty slice and no error.
//
// Dimensions returns the vector dimensionality for the given model. For
// providers that ship a fixed-dimension model (most do), the model parameter
// is informational; for providers that expose multiple dims (OpenAI
// text-embedding-3-* supports 256/512/1536/3072), it is load-bearing.
type Embedder interface {
	Embed(ctx context.Context, texts []string, model string) ([][]float32, error)
	Dimensions(model string) int
}

// Doer is the minimal HTTP round-tripper interface used by every embedder
// client. *http.Client satisfies it; tests inject an httptest.Server
// recorder.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// wrapEmbed annotates an error with the embed business code.
func wrapEmbed(err error) error {
	if err == nil {
		return nil
	}
	return domain.Wrap(domain.CodeEmbedFailed, 502, err)
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
