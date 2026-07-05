package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/saker-ai/ctxhub/internal/models/vlm"
)

// Metric is the eval metric interface. Implementations must be safe
// for concurrent use. Score returns a value in [0, 1] where 1 is best.
//
// The method signature mirrors the conceptual shape of ragas metrics:
// given the case (query + ground_truth), the generated answer, and the
// retrieved contexts, return a numeric score. The Python ragas library
// uses a `Metric.score` single-row API; we collapse to a single method
// for ergonomics.
type Metric interface {
	// Name is the metric identifier used as the score map key. Must
	// be stable across runs (it is persisted in SQLite).
	Name() string
	// Score evaluates the metric. A nil error is required for a
	// meaningful score; on error the runner records 0 plus the error
	// string in EvalResult.Errors.
	Score(ctx context.Context, c EvalCase, answer string, contexts []string) (float64, error)
}

// Grader is the LLM interface every metric uses for judgment calls.
// internal/models/vlm.VLM satisfies it (Chat + Embed), so production
// code passes a *vlm.OpenAIClient / *vlm.AnthropicClient and tests
// pass a *vlm.Stub with a scripted ChatFn / EmbedFn.
//
// Defining Grader here (rather than importing vlm.VLM directly) keeps
// the metric structs serializable and lets us swap in a non-VLM grader
// (e.g. a local model) without touching every metric.
type Grader interface {
	Chat(ctx context.Context, req vlm.ChatRequest) (*vlm.ChatResponse, error)
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// grade is the shared helper: call the grader with system+user prompts
// at temperature 0 and return the assistant content. Empty model name
// is allowed (the stub ignores it; real clients fall back to their
// configured default).
func grade(ctx context.Context, g Grader, model, system, user string) (string, error) {
	if g == nil {
		return "", fmt.Errorf("eval: nil grader")
	}
	msgs := []vlm.Message{
		{Role: vlm.RoleSystem, Content: system},
		{Role: vlm.RoleUser, Content: user},
	}
	req := vlm.ChatRequest{Model: model, Messages: msgs, Temperature: 0}
	resp, err := g.Chat(ctx, req)
	if err != nil {
		return "", fmt.Errorf("eval: grader chat: %w", err)
	}
	return resp.Content, nil
}

// extractJSONObject locates the first {...} block in s and unmarshals
// it into out. LLMs occasionally wrap JSON in markdown fences or
// prose; this helper is permissive about leading/trailing text but
// strict about the JSON itself.
func extractJSONObject(s string, out any) error {
	start := strings.Index(s, "{")
	if start < 0 {
		return fmt.Errorf("eval: no JSON object in response: %q", truncate(s, 200))
	}
	end := strings.LastIndex(s, "}")
	if end <= start {
		return fmt.Errorf("eval: unterminated JSON object")
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), out); err != nil {
		return fmt.Errorf("eval: parse JSON: %w", err)
	}
	return nil
}

// truncate returns the first n chars of s with an ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// cosine returns the cosine similarity between two float32 vectors.
// Returns 0 for empty or mismatched-length inputs. Used by
// AnswerRelevancy and ContextualRelevance.
func cosine(a, b []float32) float64 {
	n := len(a)
	if n == 0 || n != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		af := float64(a[i])
		bf := float64(b[i])
		dot += af * bf
		na += af * af
		nb += bf * bf
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---------------------------------------------------------------------------
// Faithfulness: are the answer's claims supported by the contexts?
//
// Ported from ragas.metrics._faithfulness.Faithfulness. Two-step LLM
// flow:
//  1. Extract claims from the answer.
//  2. For each claim, verify whether it can be inferred from the
//     retrieved contexts.
//
// Score = supported_claims / total_claims. When the answer has no
// claims (e.g. "I don't know") the score is 0 — matching ragas.
// ---------------------------------------------------------------------------

// Faithfulness measures whether the answer is grounded in the
// retrieved contexts (no hallucinations).
type Faithfulness struct {
	Grader Grader
	Model  string
}

// Name implements Metric.
func (m *Faithfulness) Name() string { return "faithfulness" }

// Score implements Metric.
func (m *Faithfulness) Score(ctx context.Context, c EvalCase, answer string, contexts []string) (float64, error) {
	if answer == "" {
		return 0, nil
	}
	if len(contexts) == 0 {
		return 0, nil
	}
	claims, err := m.extractClaims(ctx, answer)
	if err != nil {
		return 0, err
	}
	if len(claims) == 0 {
		return 0, nil
	}
	verdicts, err := m.verifyClaims(ctx, claims, contexts)
	if err != nil {
		return 0, err
	}
	if len(verdicts) == 0 {
		return 0, nil
	}
	supported := 0
	for _, v := range verdicts {
		if v {
			supported++
		}
	}
	return float64(supported) / float64(len(verdicts)), nil
}

func (m *Faithfulness) extractClaims(ctx context.Context, answer string) ([]string, error) {
	const sys = "You are a careful evaluator. Extract every distinct factual claim from the answer. " +
		"Return ONLY a JSON object of shape {\"claims\": [\"...\", ...]}."
	user := "Answer:\n" + answer + "\n\nExtract the claims."
	out, err := grade(ctx, m.Grader, m.Model, sys, user)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Claims []string `json:"claims"`
	}
	if err := extractJSONObject(out, &resp); err != nil {
		return nil, err
	}
	return resp.Claims, nil
}

func (m *Faithfulness) verifyClaims(ctx context.Context, claims, contexts []string) ([]bool, error) {
	const sys = "You are a careful evaluator. For each claim decide whether it can be directly inferred " +
		"from the provided contexts. Return ONLY a JSON object: " +
		"{\"verdicts\": [{\"claim\": \"...\", \"verdict\": 1 or 0}]}. Verdict 1 = supported, 0 = unsupported."
	var sb strings.Builder
	sb.WriteString("Contexts:\n")
	for i, c := range contexts {
		fmt.Fprintf(&sb, "[%d] %s\n\n", i+1, c)
	}
	sb.WriteString("Claims:\n")
	for i, c := range claims {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, c)
	}
	out, err := grade(ctx, m.Grader, m.Model, sys, sb.String())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Verdicts []struct {
			Claim   string `json:"claim"`
			Verdict int    `json:"verdict"`
		} `json:"verdicts"`
	}
	if err := extractJSONObject(out, &resp); err != nil {
		return nil, err
	}
	res := make([]bool, len(resp.Verdicts))
	for i, v := range resp.Verdicts {
		res[i] = v.Verdict == 1
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// AnswerRelevancy: does the answer actually address the question?
//
// Ported from ragas.metrics._answer_relevance.AnswerRelevancy. Two-step
// flow:
//  1. Ask the LLM to generate N candidate questions that the answer
//     could be responding to.
//  2. Embed each generated question and the original question, compute
//     mean cosine similarity.
//
// Score = mean(cosine(original, generated_i)). When the answer is
// empty or the LLM returns no questions, the score is 0.
// ---------------------------------------------------------------------------

// AnswerRelevancy measures how directly the answer addresses the
// question. Higher = more relevant.
type AnswerRelevancy struct {
	Grader       Grader
	Model        string
	EmbedderModel string
	// NumQuestions is the number of candidate questions to generate
	// (Python default: 3). Must be >= 1.
	NumQuestions int
}

// Name implements Metric.
func (m *AnswerRelevancy) Name() string { return "answer_relevancy" }

// Score implements Metric.
func (m *AnswerRelevancy) Score(ctx context.Context, c EvalCase, answer string, contexts []string) (float64, error) {
	if answer == "" || c.Query == "" {
		return 0, nil
	}
	n := m.NumQuestions
	if n < 1 {
		n = 3
	}
	questions, err := m.generateQuestions(ctx, c.Query, answer, n)
	if err != nil {
		return 0, err
	}
	if len(questions) == 0 {
		return 0, nil
	}
	embeds, err := m.Grader.Embed(ctx, append([]string{c.Query}, questions...))
	if err != nil {
		return 0, fmt.Errorf("answer_relevancy: embed: %w", err)
	}
	if len(embeds) != len(questions)+1 {
		return 0, fmt.Errorf("answer_relevancy: embed returned %d vectors, want %d", len(embeds), len(questions)+1)
	}
	orig := embeds[0]
	var sum float64
	for _, q := range embeds[1:] {
		sum += cosine(orig, q)
	}
	return sum / float64(len(questions)), nil
}

func (m *AnswerRelevancy) generateQuestions(ctx context.Context, query, answer string, n int) ([]string, error) {
	const sys = "You are a careful evaluator. Given an answer, generate questions that the answer " +
		"could be responding to. Return ONLY a JSON object: {\"questions\": [\"...\", ...]}."
	user := fmt.Sprintf("Answer:\n%s\n\nGenerate %d candidate questions.", answer, n)
	out, err := grade(ctx, m.Grader, m.Model, sys, user)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Questions []string `json:"questions"`
	}
	if err := extractJSONObject(out, &resp); err != nil {
		return nil, err
	}
	return resp.Questions, nil
}

// ---------------------------------------------------------------------------
// ContextualPrecision: are the relevant contexts ranked at the top?
//
// Ported from ragas.metrics._context_precision.ContextPrecision. The
// LLM is asked, for each context in retrieval order, whether it is
// useful for answering the question. The score is the mean precision@k
// across the relevant positions:
//
//	score = (1 / total_relevant) * sum_{k where context_k is relevant} precision@k
//	      = (1 / total_relevant) * sum_{k relevant} (relevant_so_far / k)
//
// When no context is relevant the score is 0 — matching ragas.
// ---------------------------------------------------------------------------

// ContextualPrecision measures whether useful contexts appear at the
// top of the retrieval list.
type ContextualPrecision struct {
	Grader Grader
	Model  string
}

// Name implements Metric.
func (m *ContextualPrecision) Name() string { return "contextual_precision" }

// Score implements Metric.
func (m *ContextualPrecision) Score(ctx context.Context, c EvalCase, answer string, contexts []string) (float64, error) {
	if c.Query == "" || len(contexts) == 0 {
		return 0, nil
	}
	verdicts, err := m.verifyContexts(ctx, c.Query, contexts)
	if err != nil {
		return 0, err
	}
	if len(verdicts) == 0 {
		return 0, nil
	}
	relevantTotal := 0
	for _, v := range verdicts {
		if v {
			relevantTotal++
		}
	}
	if relevantTotal == 0 {
		return 0, nil
	}
	var sum float64
	relevantSoFar := 0
	for k, v := range verdicts {
		if v {
			relevantSoFar++
			sum += float64(relevantSoFar) / float64(k+1)
		}
	}
	return sum / float64(relevantTotal), nil
}

func (m *ContextualPrecision) verifyContexts(ctx context.Context, query string, contexts []string) ([]bool, error) {
	const sys = "You are a careful evaluator. For each context chunk decide whether it is useful for " +
		"answering the question. Return ONLY a JSON object: " +
		"{\"verdicts\": [{\"context_index\": 1-based int, \"verdict\": 1 or 0}]}."
	var sb strings.Builder
	fmt.Fprintf(&sb, "Question:\n%s\n\nContexts:\n", query)
	for i, c := range contexts {
		fmt.Fprintf(&sb, "[%d] %s\n\n", i+1, c)
	}
	out, err := grade(ctx, m.Grader, m.Model, sys, sb.String())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Verdicts []struct {
			ContextIndex int `json:"context_index"`
			Verdict      int `json:"verdict"`
		} `json:"verdicts"`
	}
	if err := extractJSONObject(out, &resp); err != nil {
		return nil, err
	}
	// Index by context_index (1-based). Missing entries default to 0.
	res := make([]bool, len(contexts))
	for _, v := range resp.Verdicts {
		idx := v.ContextIndex - 1
		if idx >= 0 && idx < len(res) {
			res[idx] = v.Verdict == 1
		}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// ContextualRecall: do the contexts contain the information needed to
// answer the question (per the ground truth)?
//
// Ported from ragas.metrics._context_recall.ContextRecall. The LLM
// is asked, for each claim in the ground truth, whether it can be
// attributed to the retrieved contexts.
//
// Score = attributed_claims / total_claims. Requires GroundTruth; when
// it is empty the metric returns an error so callers know they need to
// supply a reference answer.
// ---------------------------------------------------------------------------

// ContextualRecall measures whether the contexts cover the ground
// truth's claims.
type ContextualRecall struct {
	Grader Grader
	Model  string
}

// Name implements Metric.
func (m *ContextualRecall) Name() string { return "contextual_recall" }

// Score implements Metric.
func (m *ContextualRecall) Score(ctx context.Context, c EvalCase, answer string, contexts []string) (float64, error) {
	if c.GroundTruth == "" {
		return 0, fmt.Errorf("contextual_recall: ground_truth is required")
	}
	if len(contexts) == 0 {
		return 0, nil
	}
	claims, err := m.extractClaims(ctx, c.GroundTruth)
	if err != nil {
		return 0, err
	}
	if len(claims) == 0 {
		return 0, nil
	}
	verdicts, err := m.attributeClaims(ctx, claims, contexts)
	if err != nil {
		return 0, err
	}
	if len(verdicts) == 0 {
		return 0, nil
	}
	attributed := 0
	for _, v := range verdicts {
		if v {
			attributed++
		}
	}
	return float64(attributed) / float64(len(verdicts)), nil
}

func (m *ContextualRecall) extractClaims(ctx context.Context, groundTruth string) ([]string, error) {
	const sys = "You are a careful evaluator. Extract every distinct factual claim from the reference answer. " +
		"Return ONLY a JSON object: {\"claims\": [\"...\", ...]}."
	user := "Reference answer:\n" + groundTruth + "\n\nExtract the claims."
	out, err := grade(ctx, m.Grader, m.Model, sys, user)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Claims []string `json:"claims"`
	}
	if err := extractJSONObject(out, &resp); err != nil {
		return nil, err
	}
	return resp.Claims, nil
}

func (m *ContextualRecall) attributeClaims(ctx context.Context, claims, contexts []string) ([]bool, error) {
	const sys = "You are a careful evaluator. For each claim decide whether it can be attributed to " +
		"(i.e. directly supported by) the provided contexts. Return ONLY a JSON object: " +
		"{\"verdicts\": [{\"claim\": \"...\", \"verdict\": 1 or 0}]}."
	var sb strings.Builder
	sb.WriteString("Contexts:\n")
	for i, c := range contexts {
		fmt.Fprintf(&sb, "[%d] %s\n\n", i+1, c)
	}
	sb.WriteString("Claims:\n")
	for i, c := range claims {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, c)
	}
	out, err := grade(ctx, m.Grader, m.Model, sys, sb.String())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Verdicts []struct {
			Claim   string `json:"claim"`
			Verdict int    `json:"verdict"`
		} `json:"verdicts"`
	}
	if err := extractJSONObject(out, &resp); err != nil {
		return nil, err
	}
	res := make([]bool, len(resp.Verdicts))
	for i, v := range resp.Verdicts {
		res[i] = v.Verdict == 1
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// ContextualRelevance: are the retrieved contexts actually relevant
// to the question?
//
// Ported from the legacy ragas "context relevance" metric (the
// sentence-extraction variant). For each context the LLM is asked to
// extract the sentences that are relevant to the question; the score
// for that context is (relevant_sentences / total_sentences). The
// final score is the mean across contexts.
//
// We use the LLM rather than embeddings so the metric stays a pure
// "judgment" call — matching the Python implementation. Embedding-based
// relevance is available via AnswerRelevancy + ContextualPrecision.
// ---------------------------------------------------------------------------

// ContextualRelevance measures per-context relevance via sentence
// extraction.
type ContextualRelevance struct {
	Grader Grader
	Model  string
}

// Name implements Metric.
func (m *ContextualRelevance) Name() string { return "contextual_relevance" }

// Score implements Metric.
func (m *ContextualRelevance) Score(ctx context.Context, c EvalCase, answer string, contexts []string) (float64, error) {
	if c.Query == "" || len(contexts) == 0 {
		return 0, nil
	}
	var sum float64
	var n int
	for _, ctxt := range contexts {
		score, err := m.scoreContext(ctx, ctxt, c.Query)
		if err != nil {
			return 0, err
		}
		sum += score
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return sum / float64(n), nil
}

func (m *ContextualRelevance) scoreContext(ctx context.Context, chunk, query string) (float64, error) {
	const sys = "You are a careful evaluator. Extract the sentences from the context that are relevant " +
		"to answering the question. Return ONLY a JSON object: " +
		"{\"sentences\": [\"...\", ...], \"relevant\": [\"...\", ...]}."
	user := fmt.Sprintf("Question:\n%s\n\nContext:\n%s\n\nExtract the relevant sentences.", query, chunk)
	out, err := grade(ctx, m.Grader, m.Model, sys, user)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Sentences []string `json:"sentences"`
		Relevant  []string `json:"relevant"`
	}
	if err := extractJSONObject(out, &resp); err != nil {
		return 0, err
	}
	total := len(resp.Sentences)
	if total == 0 {
		// Treat the whole chunk as one sentence so a fully-relevant
		// chunk still scores 1 when the LLM skips sentence splitting.
		total = 1
	}
	relevant := len(resp.Relevant)
	if relevant > total {
		relevant = total
	}
	return float64(relevant) / float64(total), nil
}

// DefaultMetrics returns the 5 built-in metrics wired to the given
// grader. Convenience for callers that want "all of them" without
// constructing each one.
func DefaultMetrics(g Grader, model string) []Metric {
	return []Metric{
		&Faithfulness{Grader: g, Model: model},
		&AnswerRelevancy{Grader: g, Model: model, NumQuestions: 3},
		&ContextualPrecision{Grader: g, Model: model},
		&ContextualRecall{Grader: g, Model: model},
		&ContextualRelevance{Grader: g, Model: model},
	}
}
