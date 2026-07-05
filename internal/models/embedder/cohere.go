// Package embedder — Cohere dense embeddings client.
//
// Implements the Cohere Embed API v2 (POST /v2/embed) over net/http.
// Cohere is NOT OpenAI-compatible: it uses a "texts" array, requires
// "input_type" ("search_query" | "search_document"), and returns vectors
// under "embeddings.float". embed-v4.0 supports server-side dimension
// reduction via "output_dimension"; v3 models fall back to client-side
// truncation when Dim is smaller than the native dim.
//
// Auth is `Authorization: Bearer <COHERE_API_KEY>`.
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

// DefaultCohereBaseURL is the canonical Cohere REST root.
const DefaultCohereBaseURL = "https://api.cohere.com"

// cohereModelDim lists the native output dimension for known Cohere models.
// Unknown models default to 1024 (the v3 family default).
var cohereModelDim = map[string]int{
	"embed-v4.0":                  1536,
	"embed-multilingual-v3.0":     1024,
	"embed-english-v3.0":          1024,
	"embed-multilingual-light-v3.0": 384,
	"embed-english-light-v3.0":    384,
}

// cohereAllowedDim lists the server-side output_dimension values a model
// accepts. Only embed-v4.0 supports server-side truncation today.
var cohereAllowedDim = map[string]map[int]struct{}{
	"embed-v4.0": {256: {}, 512: {}, 1024: {}, 1536: {}},
}

// CohereClient is an Embedder backed by Cohere /v2/embed.
type CohereClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
	// Dim, when positive, requests a specific output dimension. For
	// embed-v4.0 the value is sent as "output_dimension"; for v3 models
	// the response is truncated client-side and L2-renormalized.
	Dim int
	// BatchSize caps the number of texts per HTTP request. Cohere accepts
	// up to 96 inputs per call.
	BatchSize int
}

// NewCohere constructs a Cohere-backed embedder. httpCLI may be nil.
//
// model is the Cohere model id (e.g. "embed-v4.0"). dim, when > 0,
// requests server-side or client-side dimension reduction. When dim is
// 0, the model's native dimension is used.
func NewCohere(baseURL, apiKey, model string, dim int, httpCLI *http.Client) *CohereClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = DefaultCohereBaseURL
	}
	return &CohereClient{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		Model:     model,
		HTTP:      httpCLI,
		Dim:       dim,
		BatchSize: 96,
	}
}

// cohereEmbedRequest is the /v2/embed request shape.
type cohereEmbedRequest struct {
	Model          string   `json:"model"`
	Texts          []string `json:"texts"`
	InputType      string   `json:"input_type"`
	EmbeddingTypes []string `json:"embedding_types"`
	OutputDimension int     `json:"output_dimension,omitempty"`
}

// cohereEmbedResponse is the /v2/embed response shape.
type cohereEmbedResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
}

// Embed calls POST {BaseURL}/v2/embed in batches of BatchSize. The
// returned slice preserves input order across batches. Per Cohere's
// asymmetric design, all texts in a single call share the same
// input_type; we use "search_document" (the document-side default).
func (c *CohereClient) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	m := model
	if m == "" {
		m = c.Model
	}
	if m == "" {
		return nil, fmt.Errorf("embedder.cohere: model is required")
	}
	batch := c.BatchSize
	if batch <= 0 {
		batch = 96
	}
	native := cohereNativeDim(m)
	useServerDim := c.useServerDim(m)
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += batch {
		j := i + batch
		if j > len(texts) {
			j = len(texts)
		}
		chunk := texts[i:j]
		req := cohereEmbedRequest{
			Model:          m,
			Texts:          chunk,
			InputType:      "search_document",
			EmbeddingTypes: []string{"float"},
		}
		if useServerDim && c.Dim > 0 {
			req.OutputDimension = c.Dim
		}
		body, err := json.Marshal(req)
		if err != nil {
			return nil, wrapEmbed(fmt.Errorf("encode request: %w", err))
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.BaseURL+"/v2/embed", bytes.NewReader(body))
		if err != nil {
			return nil, wrapEmbed(err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		resp, err := c.HTTP.Do(httpReq)
		if err != nil {
			return nil, wrapEmbed(fmt.Errorf("http: %w", err))
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, wrapEmbed(fmt.Errorf("embedder.cohere: http %d: %s", resp.StatusCode, string(b)))
		}
		var raw cohereEmbedResponse
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			_ = resp.Body.Close()
			return nil, wrapEmbed(fmt.Errorf("decode response: %w", err))
		}
		_ = resp.Body.Close()
		for _, v := range raw.Embeddings.Float {
			if !useServerDim && c.Dim > 0 && c.Dim < native && c.Dim < len(v) {
				v = truncateAndNormalize(v, c.Dim)
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// Dimensions returns the effective dimension for the given model: the
// caller-specified Dim when set, otherwise the model's native dim.
func (c *CohereClient) Dimensions(model string) int {
	if c.Dim > 0 {
		return c.Dim
	}
	return cohereNativeDim(model)
}

// cohereNativeDim returns the native output dim for a Cohere model.
func cohereNativeDim(model string) int {
	if d, ok := cohereModelDim[strings.ToLower(model)]; ok {
		return d
	}
	return 1024
}

// useServerDim reports whether the model accepts server-side output_dimension
// and the caller has set a non-native Dim.
func (c *CohereClient) useServerDim(model string) bool {
	if c.Dim <= 0 {
		return false
	}
	allowed, ok := cohereAllowedDim[strings.ToLower(model)]
	if !ok {
		return false
	}
	_, ok = allowed[c.Dim]
	return ok && c.Dim != cohereNativeDim(model)
}

// truncateAndNormalize truncates v to dim and L2-renormalizes the result.
// Used as the client-side fallback for v3 Cohere models that do not
// support server-side output_dimension.
func truncateAndNormalize(v []float32, dim int) []float32 {
	if dim <= 0 || dim >= len(v) {
		return v
	}
	out := append([]float32(nil), v[:dim]...)
	var sumSq float32
	for _, x := range out {
		sumSq += x * x
	}
	if sumSq > 0 {
		inv := 1.0 / float32(sqrtF32(sumSq))
		for i := range out {
			out[i] *= inv
		}
	}
	return out
}

// Compile-time assertion that CohereClient satisfies Embedder.
var _ Embedder = (*CohereClient)(nil)
