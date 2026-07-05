// Package rerank — OpenAI-compatible rerank client.
//
// Supports third-party rerank services that expose a Cohere-style rerank
// API with Bearer token auth. Compatible with Alibaba Cloud DashScope
// (qwen3-rerank) and other OpenAI-compatible rerank gateways. The
// request shape is {model, query, documents: [string], top_n} and the
// response shape is {results: [{index, relevance_score}]}.
//
// Auth is `Authorization: Bearer <API_KEY>`.
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

// DefaultOpenAIRerankBaseURL is the conventional OpenAI-compatible rerank
// endpoint. Most deployments override this with the full gateway URL
// (e.g. DashScope's qwen3-rerank endpoint).
const DefaultOpenAIRerankBaseURL = "https://api.openai.com/v1/rerank"

// OpenAIClient is a Reranker backed by an OpenAI-compatible rerank endpoint.
type OpenAIClient struct {
	// APIBase is the full endpoint URL (path included).
	APIBase string
	APIKey  string
	Model   string
	HTTP    Doer
	// ExtraHeaders carries optional headers (e.g. tenant id).
	ExtraHeaders map[string]string
	// Timeout, when > 0, overrides the default HTTP client timeout.
	Timeout time.Duration
}

// NewOpenAI constructs an OpenAI-compatible reranker. httpCLI may be nil.
// apiBase is the FULL endpoint URL (e.g.
// https://dashscope.aliyuncs.com/api/v1/services/rerank/text-rerank/text-rerank
// for DashScope qwen3-rerank). When empty, defaults to the canonical
// OpenAI-compatible /v1/rerank.
func NewOpenAI(apiBase, apiKey, model string, httpCLI *http.Client) *OpenAIClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if apiBase == "" {
		apiBase = DefaultOpenAIRerankBaseURL
	}
	return &OpenAIClient{
		APIBase: apiBase,
		APIKey:  apiKey,
		Model:   model,
		HTTP:    httpCLI,
	}
}

// openaiReq is the rerank request shape (documents as raw strings).
type openaiReq struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

// openaiResp is the response shape.
type openaiResp struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank calls POST {APIBase}.
func (c *OpenAIClient) Rerank(ctx context.Context, query string, docs []Document, topN int) ([]Document, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if query == "" {
		return nil, fmt.Errorf("rerank.openai: query is required")
	}
	if c.Model == "" {
		return nil, fmt.Errorf("rerank.openai: model is required")
	}
	if topN <= 0 || topN > len(docs) {
		topN = len(docs)
	}
	docStrings := make([]string, len(docs))
	for i, d := range docs {
		docStrings[i] = d.Content
	}
	body, err := json.Marshal(openaiReq{
		Model:     c.Model,
		Query:     query,
		Documents: docStrings,
		TopN:      topN,
	})
	if err != nil {
		return nil, wrapRerank(fmt.Errorf("encode request: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIBase, bytes.NewReader(body))
	if err != nil {
		return nil, wrapRerank(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, v := range c.ExtraHeaders {
		if strings.EqualFold(k, "authorization") || strings.EqualFold(k, "content-type") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, wrapRerank(fmt.Errorf("http: %w", err))
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, wrapRerank(fmt.Errorf("rerank.openai: http %d: %s", resp.StatusCode, string(b)))
	}
	var raw openaiResp
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

// Compile-time assertion that OpenAIClient satisfies Reranker.
var _ Reranker = (*OpenAIClient)(nil)
