// Package embedder — Voyage AI embeddings client.
//
// Voyage exposes an OpenAI-compatible embeddings endpoint at
// https://api.voyageai.com/v1/embeddings. The wire shape is OpenAI's
// {model, input}; Voyage-specific dimension control is sent via
// "output_dimension" in the request body (NOT the OpenAI "dimensions"
// field — Voyage uses its own key).
//
// Auth is `Authorization: Bearer <VOYAGE_API_KEY>`.
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

// DefaultVoyageBaseURL is the Voyage OpenAI-compatible endpoint root.
const DefaultVoyageBaseURL = "https://api.voyageai.com/v1"

// voyageModelDim lists the native output dim per Voyage model. Unknown
// models default to 1024 (the Voyage family default).
var voyageModelDim = map[string]int{
	"voyage-3":          1024,
	"voyage-3-large":    1024,
	"voyage-3.5":        1024,
	"voyage-3.5-lite":   1024,
	"voyage-4":          1024,
	"voyage-4-lite":     1024,
	"voyage-4-large":    1024,
	"voyage-code-3":     1024,
	"voyage-context-3":  1024,
	"voyage-finance-2":  1024,
	"voyage-law-2":      1024,
}

// VoyageClient is an Embedder backed by Voyage /v1/embeddings.
type VoyageClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
	// Dim, when positive, requests server-side output_dimension. Voyage
	// models accept {256, 512, 1024, 2048} for the v3/v4 family.
	Dim int
	// BatchSize caps the number of texts per HTTP request.
	BatchSize int
}

// NewVoyage constructs a Voyage-backed embedder. httpCLI may be nil.
func NewVoyage(baseURL, apiKey, model string, dim int, httpCLI *http.Client) *VoyageClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = DefaultVoyageBaseURL
	}
	return &VoyageClient{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		Model:     model,
		HTTP:      httpCLI,
		Dim:       dim,
		BatchSize: 64,
	}
}

// voyageRequest is the /v1/embeddings request shape.
type voyageRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	OutputDimension int     `json:"output_dimension,omitempty"`
}

// voyageResponse is the OpenAI-shaped response (data[].embedding).
type voyageResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed calls POST {BaseURL}/embeddings in batches of BatchSize.
func (c *VoyageClient) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	m := model
	if m == "" {
		m = c.Model
	}
	if m == "" {
		return nil, fmt.Errorf("embedder.voyage: model is required")
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
		req := voyageRequest{Model: m, Input: chunk}
		if c.Dim > 0 {
			req.OutputDimension = c.Dim
		}
		body, err := json.Marshal(req)
		if err != nil {
			return nil, wrapEmbed(fmt.Errorf("encode request: %w", err))
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.BaseURL+"/embeddings", bytes.NewReader(body))
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
			return nil, wrapEmbed(fmt.Errorf("embedder.voyage: http %d: %s", resp.StatusCode, string(b)))
		}
		var raw voyageResponse
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			_ = resp.Body.Close()
			return nil, wrapEmbed(fmt.Errorf("decode response: %w", err))
		}
		_ = resp.Body.Close()
		for _, d := range raw.Data {
			out = append(out, d.Embedding)
		}
	}
	return out, nil
}

// Dimensions returns the effective dim for the given model: caller-specified
// Dim when set, otherwise the model's native dim.
func (c *VoyageClient) Dimensions(model string) int {
	if c.Dim > 0 {
		return c.Dim
	}
	if d, ok := voyageModelDim[strings.ToLower(model)]; ok {
		return d
	}
	return 1024
}

// Compile-time assertion that VoyageClient satisfies Embedder.
var _ Embedder = (*VoyageClient)(nil)
