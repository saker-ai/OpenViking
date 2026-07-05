// Package rerank — Volcengine Ark (Doubao) rerank client.
//
// Volcengine Ark exposes a rerank endpoint at POST /api/v3/rerank that is
// NOT OpenAI-compatible (the request/response shape mirrors Cohere). We
// therefore implement it directly rather than reusing the Cohere client:
// the auth header differs (Ark uses "Bearer <ARK_API_KEY>" against the
// Ark base URL; Cohere uses a "Bearer" token against the Cohere base URL
// plus a "X-Source" header). The response field is `relevance_score`,
// the same as Cohere.
//
// arkruntime SDK v1.2.39 does not expose a rerank API (only chat,
// embeddings, images, batch, etc.). This client therefore remains
// hand-written until the SDK adds rerank coverage. Tracked as a known
// gap in docs/design/go-rewrite-known-gaps.md (§1.1).
//
// Tests inject an httptest.Server; no network calls.
package rerank

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

// DefaultVolcengineBaseURL is the Ark rerank root.
const DefaultVolcengineBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

// VolcengineClient is a Reranker backed by Ark /api/v3/rerank.
type VolcengineClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
}

// NewVolcengine constructs an Ark-backed reranker. httpCLI may be nil.
func NewVolcengine(baseURL, apiKey, model string, httpCLI *http.Client) *VolcengineClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = DefaultVolcengineBaseURL
	}
	return &VolcengineClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    httpCLI,
	}
}

type volcReq struct {
	Model     string    `json:"model"`
	Query     string    `json:"query"`
	Documents []volcDoc `json:"documents"`
	TopN      int       `json:"top_n,omitempty"`
}

type volcDoc struct {
	Content string `json:"content"`
}

type volcResp struct {
	Results []struct {
		Index int     `json:"index"`
		Score float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank calls POST {BaseURL}/rerank.
func (c *VolcengineClient) Rerank(ctx context.Context, query string, docs []Document, topN int) ([]Document, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if query == "" {
		return nil, fmt.Errorf("rerank.volcengine: query is required")
	}
	if c.Model == "" {
		return nil, fmt.Errorf("rerank.volcengine: model is required")
	}
	if topN <= 0 || topN > len(docs) {
		topN = len(docs)
	}
	vd := make([]volcDoc, len(docs))
	for i, d := range docs {
		vd[i] = volcDoc{Content: d.Content}
	}
	body, err := json.Marshal(volcReq{
		Model:     c.Model,
		Query:     query,
		Documents: vd,
		TopN:      topN,
	})
	if err != nil {
		return nil, wrapRerank(fmt.Errorf("encode request: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, wrapRerank(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, wrapRerank(fmt.Errorf("http: %w", err))
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, wrapRerank(fmt.Errorf("rerank.volcengine: http %d: %s", resp.StatusCode, string(body)))
	}
	var raw volcResp
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, wrapRerank(fmt.Errorf("decode response: %w", err))
	}
	out := make([]Document, 0, len(raw.Results))
	for _, r := range raw.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			continue
		}
		d := docs[r.Index]
		d.Score = r.Score
		out = append(out, d)
	}
	return out, nil
}
