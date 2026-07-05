// Package embedder — MiniMax embeddings client.
//
// Implements the MiniMax /v1/embeddings HTTP API over net/http. MiniMax
// is NOT OpenAI-compatible: the request shape is {model, type, texts}
// where "type" is "query" | "db" (asymmetric retrieval), and the
// response shape is {vectors: [[float,...]], base_resp: {status_code,
// status_msg}}.
//
// Auth is `Authorization: Bearer <MINIMAX_API_KEY>`. Some deployments
// also require a "GroupId" query parameter, passed via ExtraHeaders
// using the key "GroupId" (case-insensitive).
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

// DefaultMinimaxBaseURL is the canonical MiniMax embeddings endpoint.
const DefaultMinimaxBaseURL = "https://api.minimax.chat/v1/embeddings"

// MinimaxClient is an Embedder backed by MiniMax /v1/embeddings.
type MinimaxClient struct {
	// APIBase is the full endpoint URL (path included).
	APIBase string
	APIKey  string
	Model   string
	HTTP    Doer
	// Dim, when positive, is the configured output dim. MiniMax embo-01
	// returns 1536-dim vectors natively; the API does not support
	// server-side truncation, so Dim is informational only.
	Dim int
	// GroupID, when non-empty, is sent as the "GroupId" query parameter.
	GroupID string
	// ExtraHeaders carries optional headers (e.g. tenant id). The
	// "Authorization" and "Content-Type" keys are reserved.
	ExtraHeaders map[string]string
}

// NewMinimax constructs a MiniMax-backed embedder. httpCLI may be nil.
func NewMinimax(apiBase, apiKey, model string, dim int, httpCLI *http.Client) *MinimaxClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if apiBase == "" {
		apiBase = DefaultMinimaxBaseURL
	}
	return &MinimaxClient{
		APIBase: apiBase,
		APIKey:  apiKey,
		Model:   model,
		HTTP:    httpCLI,
		Dim:     dim,
	}
}

// minimaxRequest is the /v1/embeddings request shape.
type minimaxRequest struct {
	Model string   `json:"model"`
	Type  string   `json:"type"`
	Texts []string `json:"texts"`
}

// minimaxResponse is the response shape. MiniMax returns business errors
// via base_resp.status_code != 0.
type minimaxResponse struct {
	Vectors [][]float32 `json:"vectors"`
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// Embed calls POST {APIBase} with type="db" (the document-side default).
func (c *MinimaxClient) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	m := model
	if m == "" {
		m = c.Model
	}
	if m == "" {
		return nil, fmt.Errorf("embedder.minimax: model is required")
	}
	body, err := json.Marshal(minimaxRequest{
		Model: m,
		Type:  "db",
		Texts: texts,
	})
	if err != nil {
		return nil, wrapEmbed(fmt.Errorf("encode request: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase, bytes.NewReader(body))
	if err != nil {
		return nil, wrapEmbed(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, v := range c.ExtraHeaders {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "content-type" || lk == "groupid" || lk == "group_id" {
			continue
		}
		req.Header.Set(k, v)
	}
	if c.GroupID != "" {
		q := req.URL.Query()
		q.Set("GroupId", c.GroupID)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, wrapEmbed(fmt.Errorf("http: %w", err))
	}
	defer drainAndCloseMinimax(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, wrapEmbed(fmt.Errorf("embedder.minimax: http %d: %s", resp.StatusCode, string(b)))
	}
	var raw minimaxResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, wrapEmbed(fmt.Errorf("decode response: %w", err))
	}
	if raw.BaseResp.StatusCode != 0 {
		return nil, wrapEmbed(fmt.Errorf("embedder.minimax: %d %s",
			raw.BaseResp.StatusCode, raw.BaseResp.StatusMsg))
	}
	if len(raw.Vectors) == 0 {
		return nil, wrapEmbed(fmt.Errorf("embedder.minimax: empty vectors"))
	}
	return raw.Vectors, nil
}

// Dimensions returns the configured dim, defaulting to 1536 (embo-01).
func (c *MinimaxClient) Dimensions(model string) int {
	if c.Dim > 0 {
		return c.Dim
	}
	return 1536
}

// drainAndCloseMinimax reads and discards r, then closes it.
func drainAndCloseMinimax(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 4096))
	_ = r.Close()
}

// Compile-time assertion that MinimaxClient satisfies Embedder.
var _ Embedder = (*MinimaxClient)(nil)
