// Package embedder — OpenAI embeddings client.
//
// Implements the OpenAI /v1/embeddings wire shape over net/http. The same
// client works for any OpenAI-compatible gateway (Azure, LiteLLM, local
// proxy) by overriding BaseURL and the auth header. Volcengine Ark uses
// its own client (volcengine.go) due to a different default base URL and
// model id conventions, but the wire shape is identical.
//
// Tests inject an httptest.Server; no network calls.
package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultOpenAIBaseURL is the canonical OpenAI REST root.
const DefaultOpenAIBaseURL = "https://api.openai.com/v1"

// OpenAIClient is an Embedder backed by OpenAI's /v1/embeddings endpoint.
type OpenAIClient struct {
	BaseURL string
	APIKey  string
	HTTP    Doer
	// DimByModel overrides the dimension table for providers that publish
	// multiple dimensions (e.g. OpenAI text-embedding-3-small = 1536 by
	// default, but may be truncated). Leave nil to use knownDefaultDim.
	DimByModel map[string]int
	// BatchSize is the maximum number of texts per HTTP request. OpenAI
	// accepts up to 2048 inputs per call; we cap at 256 by default to
	// keep response bodies small.
	BatchSize int
}

// NewOpenAI constructs an OpenAI-backed embedder. httpCLI may be nil.
func NewOpenAI(baseURL, apiKey string, httpCLI *http.Client) *OpenAIClient {
	if baseURL == "" {
		baseURL = DefaultOpenAIBaseURL
	}
	return &OpenAIClient{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		HTTP:      httpOrDefault(httpCLI),
		BatchSize: 64,
	}
}

// openAIEmbeddingResponse is the wire shape returned by /v1/embeddings.
type openAIEmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed calls POST {BaseURL}/embeddings, batching input when len(texts) >
// BatchSize. The returned slice preserves input order across batches.
func (c *OpenAIClient) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if model == "" {
		return nil, fmt.Errorf("embedder.openai: model is required")
	}
	batch := c.BatchSize
	if batch <= 0 {
		batch = 64
	}
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += batch {
		j := i + batch
		if j > len(texts) {
			j = len(texts)
		}
		chunk := texts[i:j]
		body, err := json.Marshal(map[string]any{
			"model": model,
			"input": chunk,
		})
		if err != nil {
			return nil, wrapEmbed(fmt.Errorf("encode request: %w", err))
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.BaseURL+"/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, wrapEmbed(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, wrapEmbed(fmt.Errorf("http: %w", err))
		}
		if err := drainOnError(resp); err != nil {
			return nil, err
		}
		var raw openAIEmbeddingResponse
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return nil, wrapEmbed(fmt.Errorf("decode response: %w", err))
		}
		for _, d := range raw.Data {
			out = append(out, d.Embedding)
		}
		_ = resp.Body.Close()
	}
	return out, nil
}

// Dimensions returns the dimensionality of the given model, using the
// DimByModel override table if present, otherwise knownDefaultDim.
func (c *OpenAIClient) Dimensions(model string) int {
	if c.DimByModel != nil {
		if d, ok := c.DimByModel[model]; ok && d > 0 {
			return d
		}
	}
	return knownDefaultDim(model)
}

// knownDefaultDim returns the documented embedding dimension for common
// OpenAI / Volcengine / Azure models. Unknown models fall back to 1536
// (the OpenAI default for text-embedding-3-small / text-embedding-ada-002).
func knownDefaultDim(model string) int {
	switch {
	case strings.Contains(model, "text-embedding-3-large"):
		return 3072
	case strings.Contains(model, "text-embedding-3-small"):
		return 1536
	case strings.Contains(model, "text-embedding-ada-002"):
		return 1536
	case strings.Contains(model, "doubao-embedding"):
		return 2048
	case strings.Contains(model, "bge-large"):
		return 1024
	case strings.Contains(model, "bge-small"):
		return 512
	}
	return 1536
}

// drainOnError reads and discards a non-2xx response body, returning a
// domain-wrapped error. The caller is still responsible for closing the
// body.
func drainOnError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return wrapEmbed(fmt.Errorf("embedder: http %d: %s", resp.StatusCode, string(body)))
}

// defaultTimeout is the timeout applied when the caller passes a nil
// *http.Client to a constructor. Kept as a var to allow overriding in
// tests (e.g. to assert a shorter timeout).
var defaultTimeout = 30 * time.Second
