// Package embedder — local in-process embedder.
//
// Python OpenViking uses llama-cpp-python in-process for local embeddings.
// The Go rewrite follows design 7.8.5: a local embedder is exposed via an
// HTTP sidecar (Ollama, llama.cpp server, or any OpenAI-compatible /v1/embeddings
// endpoint). LocalClient below is a thin HTTP client pointed at
// cfg.APIBase (env: OV_EMBEDDER_API_BASE / LOCAL_EMBED_BASE_URL).
//
// When no sidecar URL is configured, LocalClient falls back to a
// deterministic hash-based embedder that produces float32 vectors of the
// requested dimension. The hash embedder is NOT a meaningful embedding —
// it exists so unit tests and offline dev can construct collections without
// a running model. Callers that need real similarity MUST configure a
// sidecar URL.
//
// Tests MUST NOT make network calls; the hash fallback is the default in
// all tests.
package embedder

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net/http"

	"github.com/saker-ai/ctxhub/internal/config"
)

// LocalClient is an Embedder that prefers an HTTP sidecar and falls back
// to a deterministic hash embedder when no sidecar is configured.
type LocalClient struct {
	// Sidecar, when non-nil, is an OpenAI-compatible HTTP embedder pointed
	// at a local server (Ollama, llama.cpp, etc.).
	Sidecar *OpenAIClient
	// Dim is the dimension used by the hash fallback.
	Dim int
	// ModelHint is the model id passed to the sidecar (e.g. "nomic-embed-text").
	ModelHint string
}

// NewLocal constructs a local embedder from cfg.
//
// If cfg.APIBase is non-empty, the returned client routes Embed calls to
// an OpenAI-compatible sidecar at that URL. Otherwise Embed uses the hash
// fallback with cfg.Dim (defaulting to 768 if zero — a common local dim).
func NewLocal(cfg config.EmbedderConfig, httpCLI *http.Client) *LocalClient {
	dim := cfg.Dim
	if dim <= 0 {
		dim = 768
	}
	c := &LocalClient{Dim: dim, ModelHint: cfg.Model}
	if cfg.APIBase != "" {
		c.Sidecar = NewOpenAI(cfg.APIBase, cfg.APIKey, httpCLI)
	}
	return c
}

// Embed returns one vector per input text. When a sidecar is configured,
// it is used; otherwise the hash fallback produces deterministic vectors
// of dimension c.Dim.
func (c *LocalClient) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if c.Sidecar != nil {
		m := model
		if m == "" {
			m = c.ModelHint
		}
		return c.Sidecar.Embed(ctx, texts, m)
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = hashEmbed(t, c.Dim)
	}
	return out, nil
}

// Dimensions returns the configured dim (hash fallback) or defers to the
// sidecar's dimension table.
func (c *LocalClient) Dimensions(model string) int {
	if c.Sidecar != nil {
		return c.Sidecar.Dimensions(model)
	}
	return c.Dim
}

// hashEmbed produces a deterministic float32 vector of the requested
// dimension from text. The vector is L2-normalized so cosine similarity
// is well-defined. The hash is NOT a meaningful semantic embedding; it
// exists solely to give tests and offline dev a stable, non-random
// representation.
//
// Algorithm:
//  1. seed fnv64 of the text
//  2. for each dim slot, sha256(seed || text || slot) and take the first
//     4 bytes as a uint32, mapped to [-1, 1] via (x / 2^31) - 1
//  3. L2-normalize the resulting vector
func hashEmbed(text string, dim int) []float32 {
	if dim <= 0 {
		dim = 768
	}
	v := make([]float32, dim)
	seed := fnv.New64a()
	_, _ = seed.Write([]byte(text))
	seedVal := seed.Sum64()

	var sumSq float32
	for i := 0; i < dim; i++ {
		h := sha256.New()
		var buf [16]byte
		binary.LittleEndian.PutUint64(buf[:8], seedVal)
		binary.LittleEndian.PutUint64(buf[8:], uint64(i))
		_, _ = h.Write(buf[:])
		_, _ = h.Write([]byte(text))
		sum := h.Sum(nil)
		x := binary.LittleEndian.Uint32(sum[:4])
		// Map uint32 [0, 2^32) to [-1, 1].
		f := float32(int32(x)) / float32(2147483648.0)
		v[i] = f
		sumSq += f * f
	}
	if sumSq > 0 {
		inv := 1.0 / float32(sqrtF32(sumSq))
		for i := range v {
			v[i] *= inv
		}
	}
	return v
}

// sqrtF32 is a float32-only sqrt to avoid importing math (which would pull
// in assembly and slow tests on some platforms). Newton's method, ~6 iters.
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

// Compile-time assertion that LocalClient satisfies Embedder.
var _ Embedder = (*LocalClient)(nil)

// installHint is referenced by stub embedders that ship without a sidecar
// URL; the message is surfaced when callers ask why embeddings are not
// semantically meaningful.
const installHint = "// Install: set OV_EMBEDDER_API_BASE to an OpenAI-compatible local embedding " +
	"server (e.g. https://ollama.local:11434/v1) for real embeddings; the " +
	"hash fallback is for offline tests only."

// ErrNoSidecar is returned by MustHaveSidecar when a caller explicitly
// requires a sidecar but none is configured. The error message embeds
// installHint so callers can surface install guidance.
var ErrNoSidecar = fmt.Errorf("%s: %s",
	"local embedder has no sidecar configured", installHint)

// MustHaveSidecar returns the sidecar client, or an error if none is
// configured. Use it from code paths that require real embeddings (e.g.
// ingest pipelines writing to vectordb); tests and offline dev use Embed
// directly, which falls back to the hash stub.
func (c *LocalClient) MustHaveSidecar() (*OpenAIClient, error) {
	if c.Sidecar == nil {
		return nil, ErrNoSidecar
	}
	return c.Sidecar, nil
}

// LocalConfigFromBaseURL constructs a LocalClient from a single base URL
// (e.g. environment variable LOCAL_EMBED_BASE_URL). Convenience wrapper
// around NewLocal for callers that do not have a full config.EmbedderConfig.
func LocalConfigFromBaseURL(baseURL string, dim int) *LocalClient {
	cfg := config.EmbedderConfig{APIBase: baseURL, Dim: dim}
	return NewLocal(cfg, &http.Client{})
}
