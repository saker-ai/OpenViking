// Package embedder — LiteLLM proxy embeddings client.
//
// LiteLLM is a proxy that routes embedding requests to dozens of
// providers (OpenRouter, Ollama, vLLM, etc.). The model id uses
// litellm's "provider/model" format (e.g. "openai/text-embedding-3-small",
// "ollama/nomic-embed-text"). LiteLLM exposes an OpenAI-compatible
// /embeddings endpoint, so this client posts {model, input} via net/http.
//
// Auth is `Authorization: Bearer <LITELLM_API_KEY>`. The caller-specified
// Dim is used for client-side truncation only — we do NOT pass a
// "dimensions" parameter to LiteLLM because many backends (e.g.
// Qwen3-Embedding-4B) reject it. This matches the Python reference.
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

// DefaultLiteLLMBaseURL is the conventional LiteLLM proxy root. Most
// deployments override this with their own proxy URL.
const DefaultLiteLLMBaseURL = "http://localhost:4000/v1"

// LiteLLMClient is an Embedder backed by a LiteLLM proxy.
type LiteLLMClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
	// Dim is the caller-pinned dimension. Vectors longer than Dim are
	// truncated client-side; shorter vectors are returned as-is.
	Dim int
	// BatchSize caps the number of texts per HTTP request.
	BatchSize int
}

// NewLiteLLM constructs a LiteLLM-backed embedder. httpCLI may be nil.
//
// dim is required by the Python reference (LiteLLM does not publish a
// per-model default dim table); callers MUST pass dim > 0.
func NewLiteLLM(baseURL, apiKey, model string, dim int, httpCLI *http.Client) *LiteLLMClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = DefaultLiteLLMBaseURL
	}
	return &LiteLLMClient{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		Model:     model,
		HTTP:      httpCLI,
		Dim:       dim,
		BatchSize: 64,
	}
}

// litellmRequest is the /embeddings request shape (OpenAI-compatible).
type litellmRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// litellmResponse is the OpenAI-shaped response (data[].embedding).
type litellmResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed calls POST {BaseURL}/embeddings in batches of BatchSize.
func (c *LiteLLMClient) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	m := model
	if m == "" {
		m = c.Model
	}
	if m == "" {
		return nil, fmt.Errorf("embedder.litellm: model is required")
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
		body, err := json.Marshal(litellmRequest{Model: m, Input: chunk})
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
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, wrapEmbed(fmt.Errorf("embedder.litellm: http %d: %s", resp.StatusCode, string(b)))
		}
		var raw litellmResponse
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			_ = resp.Body.Close()
			return nil, wrapEmbed(fmt.Errorf("decode response: %w", err))
		}
		_ = resp.Body.Close()
		for _, d := range raw.Data {
			v := d.Embedding
			if c.Dim > 0 && c.Dim < len(v) {
				v = append([]float32(nil), v[:c.Dim]...)
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// Dimensions returns the caller-pinned dim. LiteLLM does not publish a
// per-model dim table; the caller must set Dim at construction time.
func (c *LiteLLMClient) Dimensions(model string) int {
	return c.Dim
}

// Compile-time assertion that LiteLLMClient satisfies Embedder.
var _ Embedder = (*LiteLLMClient)(nil)
