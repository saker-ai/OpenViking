package eval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/models/vlm"
)

// stubGrader returns canned chat responses keyed by substring match
// against the user prompt. Embeddings come back as deterministic
// unit-axis vectors so cosine similarity is predictable.
type stubGrader struct {
	// chatBy contains maps from substring -> response. The first map
	// whose key is a substring of the user prompt wins, in insertion
	// order. This lets a test script extract-then-verify calls in
	// sequence.
	chatBy []chatEntry
	// embedBy returns fixed vectors for each input text.
	embedBy map[string][]float32
	// embedSeq returns vectors in order, one per call, regardless of
	// input text. Used when a test wants to control the embed order
	// without naming each text.
	embedSeq [][]float32
	// embedCalls records every embed input for assertions.
	embedCalls [][]string
}

type chatEntry struct {
	contains string
	resp     string
	err      error
}

func (s *stubGrader) Chat(ctx context.Context, req vlm.ChatRequest) (*vlm.ChatResponse, error) {
	// Concatenate user messages for substring matching.
	var userText string
	for _, m := range req.Messages {
		if m.Role == vlm.RoleUser {
			userText += m.Content + "\n"
		}
	}
	for _, e := range s.chatBy {
		if e.contains == "" || strings.Contains(userText, e.contains) {
			if e.err != nil {
				return nil, e.err
			}
			return &vlm.ChatResponse{Content: e.resp}, nil
		}
	}
	return &vlm.ChatResponse{Content: "{}"}, nil
}

func (s *stubGrader) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	s.embedCalls = append(s.embedCalls, append([]string(nil), texts...))
	if s.embedSeq != nil {
		out := make([][]float32, len(texts))
		for i := range texts {
			if i < len(s.embedSeq) {
				out[i] = s.embedSeq[i]
			} else {
				out[i] = s.embedSeq[len(s.embedSeq)-1]
			}
		}
		return out, nil
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := s.embedBy[t]; ok {
			out[i] = v
		} else {
			out[i] = []float32{0, 0, 0}
		}
	}
	return out, nil
}

// cannedChat returns a stubGrader chat entry that matches a substring
// in the user prompt and returns a fixed response.
func cannedChat(contains, resp string) chatEntry {
	return chatEntry{contains: contains, resp: resp}
}

// ---------------------------------------------------------------------------
// Faithfulness
// ---------------------------------------------------------------------------

func TestFaithfulness_HighScoreWhenAllClaimsSupported(t *testing.T) {
	t.Parallel()
	g := &stubGrader{
		chatBy: []chatEntry{
			cannedChat("Extract the claims", `{"claims": ["the sky is blue", "water is wet"]}`),
			cannedChat("Contexts", `{"verdicts":[{"claim":"the sky is blue","verdict":1},{"claim":"water is wet","verdict":1}]}`),
		},
	}
	m := &Faithfulness{Grader: g, Model: "stub"}
	score, err := m.Score(context.Background(), EvalCase{Query: "q"}, "answer", []string{"ctx"})
	require.NoError(t, err)
	assert.InDelta(t, 1.0, score, 1e-9)
}

func TestFaithfulness_LowScoreWhenNoClaimsSupported(t *testing.T) {
	t.Parallel()
	g := &stubGrader{
		chatBy: []chatEntry{
			cannedChat("Extract the claims", `{"claims": ["claim1", "claim2", "claim3"]}`),
			cannedChat("Contexts", `{"verdicts":[{"claim":"claim1","verdict":0},{"claim":"claim2","verdict":0},{"claim":"claim3","verdict":0}]}`),
		},
	}
	m := &Faithfulness{Grader: g, Model: "stub"}
	score, err := m.Score(context.Background(), EvalCase{Query: "q"}, "answer", []string{"ctx"})
	require.NoError(t, err)
	assert.InDelta(t, 0.0, score, 1e-9)
}

func TestFaithfulness_EmptyAnswerReturnsZero(t *testing.T) {
	t.Parallel()
	g := &stubGrader{}
	m := &Faithfulness{Grader: g, Model: "stub"}
	score, err := m.Score(context.Background(), EvalCase{Query: "q"}, "", []string{"ctx"})
	require.NoError(t, err)
	assert.InDelta(t, 0.0, score, 1e-9)
}

// ---------------------------------------------------------------------------
// AnswerRelevancy
// ---------------------------------------------------------------------------

func TestAnswerRelevancy_HighScoreWhenQuestionsAlign(t *testing.T) {
	t.Parallel()
	// The metric embeds [original, q1, q2, q3] and averages cosine
	// similarity of (original, q_i). When all q_i == original vector,
	// every cosine = 1 -> score = 1.
	g := &stubGrader{
		chatBy: []chatEntry{
			cannedChat("Generate", `{"questions": ["q1", "q2", "q3"]}`),
		},
		embedSeq: [][]float32{
			{1, 0, 0}, // original
			{1, 0, 0}, // q1
			{1, 0, 0}, // q2
			{1, 0, 0}, // q3
		},
	}
	m := &AnswerRelevancy{Grader: g, Model: "stub", NumQuestions: 3}
	score, err := m.Score(context.Background(), EvalCase{Query: "what is x?"}, "answer is x", nil)
	require.NoError(t, err)
	assert.InDelta(t, 1.0, score, 1e-9)
}

func TestAnswerRelevancy_LowScoreWhenQuestionsDiverge(t *testing.T) {
	t.Parallel()
	// q_i vectors orthogonal to the original -> cosine = 0 -> score 0.
	g := &stubGrader{
		chatBy: []chatEntry{
			cannedChat("Generate", `{"questions": ["q1", "q2", "q3"]}`),
		},
		embedSeq: [][]float32{
			{1, 0, 0}, // original
			{0, 1, 0}, // q1 (orthogonal)
			{0, 0, 1}, // q2 (orthogonal)
			{0, 1, 0}, // q3 (orthogonal)
		},
	}
	m := &AnswerRelevancy{Grader: g, Model: "stub", NumQuestions: 3}
	score, err := m.Score(context.Background(), EvalCase{Query: "what is x?"}, "answer is x", nil)
	require.NoError(t, err)
	assert.InDelta(t, 0.0, score, 1e-9)
}

// ---------------------------------------------------------------------------
// ContextualPrecision
// ---------------------------------------------------------------------------

func TestContextualPrecision_HighScoreWhenRelevantFirst(t *testing.T) {
	t.Parallel()
	// Two contexts, both relevant. precision@1 = 1/1 = 1; precision@2
	// = 2/2 = 1. Mean = (1 + 1) / 2 = 1.
	g := &stubGrader{
		chatBy: []chatEntry{
			cannedChat("Question", `{"verdicts":[{"context_index":1,"verdict":1},{"context_index":2,"verdict":1}]}`),
		},
	}
	m := &ContextualPrecision{Grader: g, Model: "stub"}
	score, err := m.Score(context.Background(), EvalCase{Query: "q"}, "ans", []string{"c1", "c2"})
	require.NoError(t, err)
	assert.InDelta(t, 1.0, score, 1e-9)
}

func TestContextualPrecision_LowScoreWhenRelevantLast(t *testing.T) {
	t.Parallel()
	// Three contexts; only the last is relevant. precision@3 = 1/3.
	// Mean = (1/3) / 1 = 0.333.
	g := &stubGrader{
		chatBy: []chatEntry{
			cannedChat("Question", `{"verdicts":[{"context_index":1,"verdict":0},{"context_index":2,"verdict":0},{"context_index":3,"verdict":1}]}`),
		},
	}
	m := &ContextualPrecision{Grader: g, Model: "stub"}
	score, err := m.Score(context.Background(), EvalCase{Query: "q"}, "ans", []string{"c1", "c2", "c3"})
	require.NoError(t, err)
	assert.InDelta(t, 1.0/3.0, score, 1e-9)
}

// ---------------------------------------------------------------------------
// ContextualRecall
// ---------------------------------------------------------------------------

func TestContextualRecall_HighScoreWhenAllClaimsAttributed(t *testing.T) {
	t.Parallel()
	g := &stubGrader{
		chatBy: []chatEntry{
			cannedChat("Extract the claims", `{"claims": ["a", "b"]}`),
			cannedChat("Contexts", `{"verdicts":[{"claim":"a","verdict":1},{"claim":"b","verdict":1}]}`),
		},
	}
	m := &ContextualRecall{Grader: g, Model: "stub"}
	score, err := m.Score(context.Background(), EvalCase{Query: "q", GroundTruth: "gt"}, "ans", []string{"ctx"})
	require.NoError(t, err)
	assert.InDelta(t, 1.0, score, 1e-9)
}

func TestContextualRecall_LowScoreWhenNoClaimsAttributed(t *testing.T) {
	t.Parallel()
	g := &stubGrader{
		chatBy: []chatEntry{
			cannedChat("Extract the claims", `{"claims": ["a", "b", "c"]}`),
			cannedChat("Contexts", `{"verdicts":[{"claim":"a","verdict":0},{"claim":"b","verdict":0},{"claim":"c","verdict":0}]}`),
		},
	}
	m := &ContextualRecall{Grader: g, Model: "stub"}
	score, err := m.Score(context.Background(), EvalCase{Query: "q", GroundTruth: "gt"}, "ans", []string{"ctx"})
	require.NoError(t, err)
	assert.InDelta(t, 0.0, score, 1e-9)
}

func TestContextualRecall_RequiresGroundTruth(t *testing.T) {
	t.Parallel()
	m := &ContextualRecall{Grader: &stubGrader{}, Model: "stub"}
	_, err := m.Score(context.Background(), EvalCase{Query: "q"}, "ans", []string{"ctx"})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// ContextualRelevance
// ---------------------------------------------------------------------------

func TestContextualRelevance_HighScoreWhenAllSentencesRelevant(t *testing.T) {
	t.Parallel()
	g := &stubGrader{
		chatBy: []chatEntry{
			cannedChat("Extract the relevant sentences",
				`{"sentences": ["s1", "s2"], "relevant": ["s1", "s2"]}`),
		},
	}
	m := &ContextualRelevance{Grader: g, Model: "stub"}
	score, err := m.Score(context.Background(), EvalCase{Query: "q"}, "ans", []string{"ctx"})
	require.NoError(t, err)
	assert.InDelta(t, 1.0, score, 1e-9)
}

func TestContextualRelevance_LowScoreWhenNoSentencesRelevant(t *testing.T) {
	t.Parallel()
	g := &stubGrader{
		chatBy: []chatEntry{
			cannedChat("Extract the relevant sentences",
				`{"sentences": ["s1", "s2", "s3"], "relevant": []}`),
		},
	}
	m := &ContextualRelevance{Grader: g, Model: "stub"}
	score, err := m.Score(context.Background(), EvalCase{Query: "q"}, "ans", []string{"ctx"})
	require.NoError(t, err)
	assert.InDelta(t, 0.0, score, 1e-9)
}

// ---------------------------------------------------------------------------
// DefaultMetrics sanity check
// ---------------------------------------------------------------------------

func TestDefaultMetricsReturnsFive(t *testing.T) {
	t.Parallel()
	ms := DefaultMetrics(&stubGrader{}, "stub")
	require.Len(t, ms, 5)
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Name()
	}
	assert.Contains(t, names, "faithfulness")
	assert.Contains(t, names, "answer_relevancy")
	assert.Contains(t, names, "contextual_precision")
	assert.Contains(t, names, "contextual_recall")
	assert.Contains(t, names, "contextual_relevance")
}

// ---------------------------------------------------------------------------
// Misc helpers
// ---------------------------------------------------------------------------

func TestCosine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0},
		{"orthogonal", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0},
		{"opposite", []float32{1, 0, 0}, []float32{-1, 0, 0}, -1.0},
		{"empty", nil, nil, 0.0},
		{"mismatched", []float32{1, 0}, []float32{1}, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, cosine(tc.a, tc.b), 1e-9)
		})
	}
}

func TestExtractJSONObject(t *testing.T) {
	t.Parallel()
	t.Run("pure JSON", func(t *testing.T) {
		var v struct{ A int `json:"a"` }
		require.NoError(t, extractJSONObject(`{"a": 1}`, &v))
		assert.Equal(t, 1, v.A)
	})
	t.Run("JSON in prose", func(t *testing.T) {
		var v struct{ A int `json:"a"` }
		require.NoError(t, extractJSONObject(`here is the answer: {"a": 2} done`, &v))
		assert.Equal(t, 2, v.A)
	})
	t.Run("no JSON", func(t *testing.T) {
		var v map[string]any
		err := extractJSONObject("no json here", &v)
		require.Error(t, err)
	})
}

// stubGrader must satisfy the Grader interface at compile time.
var _ Grader = (*stubGrader)(nil)

// errGrader is a Grader that always fails Chat and Embed.
type errGrader struct{}

func (errGrader) Chat(context.Context, vlm.ChatRequest) (*vlm.ChatResponse, error) {
	return nil, errors.New("grader unavailable")
}
func (errGrader) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embed unavailable")
}

func TestFaithfulness_GraderErrorPropagates(t *testing.T) {
	t.Parallel()
	m := &Faithfulness{Grader: errGrader{}, Model: "stub"}
	_, err := m.Score(context.Background(), EvalCase{Query: "q"}, "answer", []string{"ctx"})
	require.Error(t, err)
}

// Compile-time assertion that Faithfulness etc. satisfy Metric.
var _ Metric = (*Faithfulness)(nil)
var _ Metric = (*AnswerRelevancy)(nil)
var _ Metric = (*ContextualPrecision)(nil)
var _ Metric = (*ContextualRecall)(nil)
var _ Metric = (*ContextualRelevance)(nil)
