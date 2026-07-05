// Package rerank — Alibaba Cloud DashScope (Tongyi) rerank client.
//
// DashScope's rerank endpoint is NOT exposed via the OpenAI-compatible
// surface. The native endpoint is
// POST https://dashscope.aliyuncs.com/api/v1/services/rerank/text-rerank/text-rerank
// with a Cohere-like request shape (model + input.query + input.documents +
// parameters.top_n + parameters.return_documents) and a response shaped as
// {"output": {"results": [{"index": int, "relevance_score": float}]}}.
//
// Auth is `Authorization: Bearer <DASHSCOPE_API_KEY>` — the same key used
// for the compatible-mode embedding/llm endpoints.
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

// DefaultDashscopeBaseURL is the DashScope native API root (rerank lives
// under /services/rerank/... rather than the OpenAI-compatible /v1).
const DefaultDashscopeBaseURL = "https://dashscope.aliyuncs.com/api/v1"

// DashscopeClient is a Reranker backed by DashScope's native rerank API.
type DashscopeClient struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    Doer
}

// NewDashscope constructs a DashScope-backed reranker. httpCLI may be nil.
func NewDashscope(baseURL, apiKey, model string, httpCLI *http.Client) *DashscopeClient {
	if httpCLI == nil {
		httpCLI = &http.Client{Timeout: 30 * time.Second}
	}
	if baseURL == "" {
		baseURL = DefaultDashscopeBaseURL
	}
	return &DashscopeClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    httpCLI,
	}
}

type dashscopeReq struct {
	Model      string           `json:"model"`
	Input      dashscopeInput   `json:"input"`
	Parameters dashscopeParams  `json:"parameters,omitempty"`
}

type dashscopeInput struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type dashscopeParams struct {
	TopN            int  `json:"top_n,omitempty"`
	ReturnDocuments bool `json:"return_documents,omitempty"`
}

type dashscopeResp struct {
	Output struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	} `json:"output"`
	RequestID string `json:"request_id"`
}

// Rerank calls POST {BaseURL}/services/rerank/text-rerank/text-rerank.
func (c *DashscopeClient) Rerank(ctx context.Context, query string, docs []Document, topN int) ([]Document, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if query == "" {
		return nil, fmt.Errorf("rerank.dashscope: query is required")
	}
	if c.Model == "" {
		return nil, fmt.Errorf("rerank.dashscope: model is required")
	}
	if topN <= 0 || topN > len(docs) {
		topN = len(docs)
	}
	docStrings := make([]string, len(docs))
	for i, d := range docs {
		docStrings[i] = d.Content
	}
	body, err := json.Marshal(dashscopeReq{
		Model: c.Model,
		Input: dashscopeInput{
			Query:     query,
			Documents: docStrings,
		},
		Parameters: dashscopeParams{
			TopN:            topN,
			ReturnDocuments: false,
		},
	})
	if err != nil {
		return nil, wrapRerank(fmt.Errorf("encode request: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/services/rerank/text-rerank/text-rerank", bytes.NewReader(body))
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
		return nil, wrapRerank(fmt.Errorf("rerank.dashscope: http %d: %s", resp.StatusCode, string(body)))
	}
	var raw dashscopeResp
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, wrapRerank(fmt.Errorf("decode response: %w", err))
	}
	out := make([]Document, 0, len(raw.Output.Results))
	for _, r := range raw.Output.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			continue
		}
		d := docs[r.Index]
		d.Score = r.RelevanceScore
		out = append(out, d)
	}
	return out, nil
}
