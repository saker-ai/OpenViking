// Package embedder — Jina AI embeddings client.
//
// Jina exposes an OpenAI-compatible embeddings endpoint at
// https://api.jina.ai/v1/embeddings. The wire shape is OpenAI's
// {model, input, dimensions}, plus Jina-specific extensions carried in
// "extra_body": "task" (e.g. "retrieval.query" | "retrieval.passage"
// for non-symmetric models) and "late_chunking" (bool).
//
// We post directly via net/http rather than reusing OpenAIClient because
// the extra_body fields are Jina-specific and the dimensions parameter
// must be sent as a top-level "dimensions" key (Jina supports Matryoshka
// reduction).
//
// Auth is `Authorization: Bearer <JINA_API_KEY>`.
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

// DefaultJinaBaseURL is the Jina OpenAI-compatible endpoint root.
const DefaultJinaBaseURL = "https://api.jina.ai/v1"

// jinaModelDim lists the maximum output dim per Jina model. Unknown
// models default to 1024 (the v5 family default).
var jinaModelDim = map[string]int{
	"jina-embeddings-v5-text-small": 1024,
	"jina-embeddings-v5-text-nano":  768,
	"jina-code-embeddings-1.5b":     1024,
	"jina-code-embeddings-0.5b":     768,
}

// JinaClient is an Embedder backed by Jina /v1/embeddings.
type JinaClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
	// Dim, when positive, requests Matryoshka dimension reduction.
	Dim int
	// Task, when non-empty, is sent as the Jina "task" field (e.g.
	// "retrieval.query" or "retrieval.passage"). Required for non-symmetric
	// Jina models; ignored by symmetric ones.
	Task string
	// LateChunking, when non-nil, is sent as the "late_chunking" field.
	LateChunking *bool
	// BatchSize caps the number of texts per HTTP request.
	BatchSize int
}

// NewJina constructs a Jina-backed embedder. httpCLI may be nil.
func NewJina(baseURL, apiKey, model string, dim int, httpCLI *http.Client) *JinaClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = DefaultJinaBaseURL
	}
	return &JinaClient{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		Model:     model,
		HTTP:      httpCLI,
		Dim:       dim,
		BatchSize: 64,
	}
}

// jinaRequest is the /v1/embeddings request shape.
type jinaRequest struct {
	Model       string   `json:"model"`
	Input       []string `json:"input"`
	Dimensions  int      `json:"dimensions,omitempty"`
	Task        string   `json:"task,omitempty"`
	LateChunking *bool   `json:"late_chunking,omitempty"`
}

// jinaResponse is the OpenAI-shaped response (data[].embedding).
type jinaResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed calls POST {BaseURL}/embeddings in batches of BatchSize.
func (c *JinaClient) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	m := model
	if m == "" {
		m = c.Model
	}
	if m == "" {
		return nil, fmt.Errorf("embedder.jina: model is required")
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
		req := jinaRequest{Model: m, Input: chunk}
		if c.Dim > 0 {
			req.Dimensions = c.Dim
		}
		if c.Task != "" {
			req.Task = c.Task
		}
		if c.LateChunking != nil {
			req.LateChunking = c.LateChunking
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
			return nil, wrapEmbed(fmt.Errorf("embedder.jina: http %d: %s", resp.StatusCode, string(b)))
		}
		var raw jinaResponse
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
// Dim when set, otherwise the model's max dim.
func (c *JinaClient) Dimensions(model string) int {
	if c.Dim > 0 {
		return c.Dim
	}
	if d, ok := jinaModelDim[model]; ok {
		return d
	}
	return 1024
}

// Compile-time assertion that JinaClient satisfies Embedder.
var _ Embedder = (*JinaClient)(nil)
