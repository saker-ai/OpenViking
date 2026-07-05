// Package rerank — Cohere rerank client.
//
// Implements the Cohere /v1/rerank HTTP API over net/http. Cohere is the
// canonical rerank provider; the same client works for any
// Cohere-compatible gateway by overriding BaseURL.
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

// DefaultCohereBaseURL is the canonical Cohere REST root.
const DefaultCohereBaseURL = "https://api.cohere.com/v1"

// CohereClient is a Reranker backed by Cohere /v1/rerank.
type CohereClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
}

// NewCohere constructs a Cohere-backed reranker. httpCLI may be nil.
func NewCohere(baseURL, apiKey, model string, httpCLI *http.Client) *CohereClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = DefaultCohereBaseURL
	}
	return &CohereClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    httpCLI,
	}
}

type cohereReq struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	Model     string   `json:"model,omitempty"`
	TopN      int      `json:"top_n,omitempty"`
}

type cohereResp struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank calls POST {BaseURL}/rerank.
func (c *CohereClient) Rerank(ctx context.Context, query string, docs []Document, topN int) ([]Document, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if query == "" {
		return nil, fmt.Errorf("rerank.cohere: query is required")
	}
	if topN <= 0 || topN > len(docs) {
		topN = len(docs)
	}
	body, err := json.Marshal(cohereReq{
		Query:     query,
		Documents: docsToTexts(docs),
		Model:     c.Model,
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
		return nil, wrapRerank(fmt.Errorf("rerank.cohere: http %d: %s", resp.StatusCode, string(body)))
	}
	var raw cohereResp
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, wrapRerank(fmt.Errorf("decode response: %w", err))
	}
	out := make([]Document, 0, len(raw.Results))
	for _, r := range raw.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			continue
		}
		d := docs[r.Index]
		d.Score = r.RelevanceScore
		out = append(out, d)
	}
	return out, nil
}

func docsToTexts(docs []Document) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Content
	}
	return out
}

func drainAndClose(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 4096))
	_ = r.Close()
}
