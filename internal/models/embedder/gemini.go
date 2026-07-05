// Package embedder — Google Gemini embeddings client.
//
// Implements the Gemini REST endpoint
// POST /v1beta/models/{model}:embedContent
// (and batchEmbedContent for >1 inputs) over net/http. Gemini is NOT
// OpenAI-compatible. Auth is `x-goog-api-key: <GOOGLE_API_KEY>`.
//
// The Gemini embedding family (gemini-embedding-2-preview,
// gemini-embedding-001, text-embedding-004) supports Matryoshka dimension
// reduction via output_dimensionality. Default dims: 3072 for
// gemini-embedding-*, 768 for text-embedding-*.
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

// DefaultGeminiBaseURL is the canonical Gemini REST root.
const DefaultGeminiBaseURL = "https://generativelanguage.googleapis.com"

// geminiModelDim lists the default output dim per Gemini model family.
var geminiModelDim = map[string]int{
	"gemini-embedding-2-preview": 3072,
	"gemini-embedding-001":       3072,
	"text-embedding-004":         768,
}

// geminiBatchSize is the per-request cap for batchEmbedContent.
const geminiBatchSize = 100

// GeminiClient is an Embedder backed by Gemini REST.
type GeminiClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
	// Dim, when positive, requests server-side output_dimensionality.
	Dim int
	// TaskType, when non-empty, is sent as task_type (e.g.
	// "RETRIEVAL_DOCUMENT"). Gemini ignores case but expects upper-case
	// tokens; callers should pass upper-case values.
	TaskType string
}

// NewGemini constructs a Gemini-backed embedder. httpCLI may be nil.
func NewGemini(baseURL, apiKey, model string, dim int, httpCLI *http.Client) *GeminiClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = DefaultGeminiBaseURL
	}
	return &GeminiClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    httpCLI,
		Dim:     dim,
	}
}

// geminiEmbedRequest is the embedContent request shape.
type geminiEmbedRequest struct {
	Content geminiContent `json:"content"`
	// TaskType / OutputDimensionality / Title use omitempty to omit when
	// not configured.
	TaskType           string `json:"taskType,omitempty"`
	OutputDimensionality int   `json:"outputDimensionality,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

// geminiEmbedResponse is the embedContent response shape.
type geminiEmbedResponse struct {
	Embedding struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
}

// geminiBatchRequest is the batchEmbedContent request shape.
type geminiBatchRequest struct {
	Requests []geminiBatchItem `json:"requests"`
}

type geminiBatchItem struct {
	Model    string         `json:"model"`
	Content  geminiContent  `json:"content"`
	TaskType string         `json:"taskType,omitempty"`
	OutputDimensionality int `json:"outputDimensionality,omitempty"`
}

// geminiBatchResponse is the batchEmbedContent response shape.
type geminiBatchResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

// Embed calls POST /v1beta/models/{model}:(batch)EmbedContent. Single
// inputs use embedContent; len(texts) > 1 uses batchEmbedContent (with
// internal chunking at geminiBatchSize).
func (c *GeminiClient) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	m := model
	if m == "" {
		m = c.Model
	}
	if m == "" {
		return nil, fmt.Errorf("embedder.gemini: model is required")
	}
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += geminiBatchSize {
		j := i + geminiBatchSize
		if j > len(texts) {
			j = len(texts)
		}
		chunk := texts[i:j]
		if len(chunk) == 1 {
			v, err := c.embedOne(ctx, m, chunk[0])
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		vs, err := c.embedBatch(ctx, m, chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}

// embedOne calls the single-input embedContent endpoint.
func (c *GeminiClient) embedOne(ctx context.Context, model, text string) ([]float32, error) {
	req := geminiEmbedRequest{
		Content: geminiContent{Parts: []geminiPart{{Text: text}}},
	}
	if c.TaskType != "" {
		req.TaskType = strings.ToUpper(c.TaskType)
	}
	if c.Dim > 0 {
		req.OutputDimensionality = c.Dim
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, wrapEmbed(fmt.Errorf("encode request: %w", err))
	}
	url := c.BaseURL + "/v1beta/models/" + model + ":embedContent"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, wrapEmbed(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("x-goog-api-key", c.APIKey)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, wrapEmbed(fmt.Errorf("http: %w", err))
	}
	defer drainAndCloseGemini(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, wrapEmbed(fmt.Errorf("embedder.gemini: http %d: %s", resp.StatusCode, string(b)))
	}
	var raw geminiEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, wrapEmbed(fmt.Errorf("decode response: %w", err))
	}
	return raw.Embedding.Values, nil
}

// embedBatch calls the batchEmbedContent endpoint.
func (c *GeminiClient) embedBatch(ctx context.Context, model string, texts []string) ([][]float32, error) {
	items := make([]geminiBatchItem, len(texts))
	for i, t := range texts {
		items[i] = geminiBatchItem{
			Model:   model,
			Content: geminiContent{Parts: []geminiPart{{Text: t}}},
		}
		if c.TaskType != "" {
			items[i].TaskType = strings.ToUpper(c.TaskType)
		}
		if c.Dim > 0 {
			items[i].OutputDimensionality = c.Dim
		}
	}
	body, err := json.Marshal(geminiBatchRequest{Requests: items})
	if err != nil {
		return nil, wrapEmbed(fmt.Errorf("encode request: %w", err))
	}
	url := c.BaseURL + "/v1beta/models/" + model + ":batchEmbedContent"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, wrapEmbed(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("x-goog-api-key", c.APIKey)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, wrapEmbed(fmt.Errorf("http: %w", err))
	}
	defer drainAndCloseGemini(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, wrapEmbed(fmt.Errorf("embedder.gemini: http %d: %s", resp.StatusCode, string(b)))
	}
	var raw geminiBatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, wrapEmbed(fmt.Errorf("decode response: %w", err))
	}
	out := make([][]float32, len(raw.Embeddings))
	for i, e := range raw.Embeddings {
		out[i] = e.Values
	}
	return out, nil
}

// Dimensions returns the effective dim for the given model: caller-specified
// Dim when set, otherwise the model's native dim.
func (c *GeminiClient) Dimensions(model string) int {
	if c.Dim > 0 {
		return c.Dim
	}
	return geminiNativeDim(model)
}

// geminiNativeDim returns the native output dim for a Gemini model.
func geminiNativeDim(model string) int {
	if d, ok := geminiModelDim[model]; ok {
		return d
	}
	if strings.HasPrefix(model, "text-embedding-") {
		return 768
	}
	return 3072
}

// drainAndCloseGemini reads and discards r, then closes it. Inline copy
// to avoid depending on a shared helper that may move between files.
func drainAndCloseGemini(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 4096))
	_ = r.Close()
}

// Compile-time assertion that GeminiClient satisfies Embedder.
var _ Embedder = (*GeminiClient)(nil)
