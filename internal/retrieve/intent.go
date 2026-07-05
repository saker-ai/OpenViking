// Package retrieve — VLM-backed intent analyzer.
//
// IntentAnalyzer rewrites the user query, generates subqueries, and emits
// a level hint that can short-circuit the L0/L1/L2 funnel. The
// VLM-backed implementation prompts the model with a small JSON contract;
// a stub implementation is provided for tests.
//
// All network calls go through the injected vlm.VLM; tests inject a
// vlm.Stub and never hit the network.
package retrieve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/models/vlm"
)

// intentPromptTemplate instructs the VLM to emit a strict JSON object. The
// shape is the contract between the analyzer and the model; parsing is
// fault-tolerant (missing fields default to zero values).
const intentPromptTemplate = `You are an intent analyzer for a hierarchical retrieval system.

Given a user query, produce a JSON object with these fields:
- "rewritten_query": a clearer, search-friendly rewrite of the query.
- "subqueries": an array of 0-3 alternative phrasings to broaden recall.
- "level_hint": one of "L0", "L1", "L2", or "" (empty) when unsure.
  * L0 = abstract / conceptual questions
  * L1 = overview / summary questions
  * L2 = specific detail / chunk-level questions
- "reasoning": a short explanation (one sentence).

Respond with JSON only. No prose, no code fences.

Query: %s`

// VLMIntentAnalyzer is an IntentAnalyzer backed by a vlm.VLM. The VLM is
// expected to return a JSON object matching intentPromptTemplate.
type VLMIntentAnalyzer struct {
	VLM   vlm.VLM
	Model string
}

// NewVLMIntentAnalyzer constructs a VLM-backed analyzer. vlm may be nil;
// in that case Analyze returns the original query with no rewrite and a
// nil level hint, so the pipeline still runs (useful for offline dev).
func NewVLMIntentAnalyzer(v vlm.VLM, model string) *VLMIntentAnalyzer {
	return &VLMIntentAnalyzer{VLM: v, Model: model}
}

// Analyze implements IntentAnalyzer.
func (a *VLMIntentAnalyzer) Analyze(ctx context.Context, query string) (*Intent, error) {
	if a.VLM == nil {
		return &Intent{OriginalQuery: query, RewrittenQuery: query}, nil
	}
	if strings.TrimSpace(query) == "" {
		return nil, domain.Wrap(domain.CodeValidationFailed, 422,
			fmt.Errorf("retrieve.intent: query is required"))
	}
	prompt := fmt.Sprintf(intentPromptTemplate, query)
	resp, err := a.VLM.Chat(ctx, vlm.ChatRequest{
		Model: a.Model,
		Messages: []vlm.Message{
			{Role: vlm.RoleUser, Content: prompt},
		},
		ResponseFormat: &vlm.ResponseFormat{Type: "json_object"},
		Temperature:    0.0,
		MaxTokens:      512,
	})
	if err != nil {
		return nil, domain.Wrap(domain.CodeVLMFailed, 502,
			fmt.Errorf("retrieve.intent: vlm chat: %w", err))
	}
	return parseIntent(query, resp.Content)
}

// parseIntent extracts an Intent from the model's JSON response. Missing
// fields default to zero values; malformed JSON falls back to the
// original query so the pipeline does not break on a misbehaving model.
func parseIntent(original, body string) (*Intent, error) {
	intent := &Intent{OriginalQuery: original, RewrittenQuery: original}
	body = strings.TrimSpace(body)
	if body == "" {
		return intent, nil
	}
	// Tolerate accidental code fences / leading prose by extracting the
	// first {...} block.
	if i := strings.Index(body, "{"); i >= 0 {
		body = body[i:]
	}
	if j := strings.LastIndex(body, "}"); j >= 0 {
		body = body[:j+1]
	}
	var raw struct {
		RewrittenQuery string   `json:"rewritten_query"`
		Subqueries     []string `json:"subqueries"`
		LevelHint      string   `json:"level_hint"`
		Reasoning      string   `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		// Malformed JSON: keep the original query and surface a soft error.
		intent.Reasoning = "intent: malformed JSON, falling back to original"
		return intent, nil
	}
	if raw.RewrittenQuery != "" {
		intent.RewrittenQuery = raw.RewrittenQuery
	}
	intent.Subqueries = raw.Subqueries
	intent.LevelHint = Level(strings.ToUpper(raw.LevelHint))
	intent.Reasoning = raw.Reasoning
	return intent, nil
}

// Compile-time assertion.
var _ IntentAnalyzer = (*VLMIntentAnalyzer)(nil)
