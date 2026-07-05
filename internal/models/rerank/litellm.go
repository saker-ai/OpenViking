// Package rerank — LiteLLM proxy rerank client.
//
// LiteLLM exposes a rerank endpoint (POST /v1/rerank) that proxies
// rerank requests to dozens of providers. The request shape mirrors
// Cohere: {model, query, documents: [{text}], top_n}. The response
// shape is {results: [{index, relevance_score}]}.
//
// Auth is `Authorization: Bearer <LITELLM_API_KEY>`.
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

// DefaultLiteLLMBaseURL is the conventional LiteLLM proxy root.
const DefaultLiteLLMBaseURL = "http://localhost:4000/v1"

// LiteLLMClient is a Reranker backed by a LiteLLM proxy.
type LiteLLMClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
}

// NewLiteLLM constructs a LiteLLM-backed reranker. httpCLI may be nil.
func NewLiteLLM(baseURL, apiKey, model string, httpCLI *http.Client) *LiteLLMClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = DefaultLiteLLMBaseURL
	}
	return &LiteLLMClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    httpCLI,
	}
}

// litellmDoc is the per-document item sent to LiteLLM. The reference
// sends {"text": ...}; raw strings are NOT accepted.
type litellmDoc struct {
	Text string `json:"text"`
}

// litellmReq is the /v1/rerank request shape.
type litellmReq struct {
	Model     string       `json:"model"`
	Query     string       `json:"query"`
	Documents []litellmDoc `json:"documents"`
	TopN      int          `json:"top_n,omitempty"`
}

// litellmResp is the response shape.
type litellmResp struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank calls POST {BaseURL}/rerank.
func (c *LiteLLMClient) Rerank(ctx context.Context, query string, docs []Document, topN int) ([]Document, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if query == "" {
		return nil, fmt.Errorf("rerank.litellm: query is required")
	}
	if c.Model == "" {
		return nil, fmt.Errorf("rerank.litellm: model is required")
	}
	if topN <= 0 || topN > len(docs) {
		topN = len(docs)
	}
	dd := make([]litellmDoc, len(docs))
	for i, d := range docs {
		dd[i] = litellmDoc{Text: d.Content}
	}
	body, err := json.Marshal(litellmReq{
		Model:     c.Model,
		Query:     query,
		Documents: dd,
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
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, wrapRerank(fmt.Errorf("rerank.litellm: http %d: %s", resp.StatusCode, string(b)))
	}
	var raw litellmResp
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

// Compile-time assertion that LiteLLMClient satisfies Reranker.
var _ Reranker = (*LiteLLMClient)(nil)
