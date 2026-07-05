package retrieve

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/models/vlm"
)

func TestVLMIntentAnalyzerNilVLM(t *testing.T) {
	t.Parallel()
	a := NewVLMIntentAnalyzer(nil, "")
	intent, err := a.Analyze(context.Background(), "what is rag")
	require.NoError(t, err)
	require.NotNil(t, intent)
	assert.Equal(t, "what is rag", intent.OriginalQuery)
	assert.Equal(t, "what is rag", intent.RewrittenQuery)
	assert.Equal(t, Level(""), intent.LevelHint)
}

func TestVLMIntentAnalyzerEmptyQuery(t *testing.T) {
	t.Parallel()
	a := NewVLMIntentAnalyzer(NewStubVLM(), "")
	_, err := a.Analyze(context.Background(), "  ")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrValidation))
}

func TestVLMIntentAnalyzerSuccess(t *testing.T) {
	t.Parallel()
	stub := vlm.NewStub()
	stub.ChatFn = func(_ context.Context, _ vlm.ChatRequest) (*vlm.ChatResponse, error) {
		return &vlm.ChatResponse{Content: `{
			"rewritten_query": "what does rag mean",
			"subqueries": ["definition of rag", "retrieval augmented generation"],
			"level_hint": "L0",
			"reasoning": "conceptual question"
		}`}, nil
	}
	a := NewVLMIntentAnalyzer(stub, "gpt-4o")
	intent, err := a.Analyze(context.Background(), "what is rag")
	require.NoError(t, err)
	require.NotNil(t, intent)
	assert.Equal(t, "what is rag", intent.OriginalQuery)
	assert.Equal(t, "what does rag mean", intent.RewrittenQuery)
	require.Len(t, intent.Subqueries, 2)
	assert.Equal(t, Level0Abstract, intent.LevelHint)
	assert.Equal(t, "conceptual question", intent.Reasoning)
}

func TestVLMIntentAnalyzerMalformedJSON(t *testing.T) {
	t.Parallel()
	stub := vlm.NewStub()
	stub.ChatFn = func(_ context.Context, _ vlm.ChatRequest) (*vlm.ChatResponse, error) {
		return &vlm.ChatResponse{Content: `not json at all`}, nil
	}
	a := NewVLMIntentAnalyzer(stub, "m")
	intent, err := a.Analyze(context.Background(), "q")
	require.NoError(t, err) // soft fallback
	require.NotNil(t, intent)
	assert.Equal(t, "q", intent.RewrittenQuery) // fallback to original
	assert.Contains(t, intent.Reasoning, "malformed")
}

func TestVLMIntentAnalyzerJSONWithProse(t *testing.T) {
	t.Parallel()
	stub := vlm.NewStub()
	stub.ChatFn = func(_ context.Context, _ vlm.ChatRequest) (*vlm.ChatResponse, error) {
		// Model wraps JSON in code fences — the parser must extract it.
		return &vlm.ChatResponse{Content: "```json\n" +
			`{"rewritten_query": "clean", "level_hint": "L2"}` + "\n```"}, nil
	}
	a := NewVLMIntentAnalyzer(stub, "m")
	intent, err := a.Analyze(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, "clean", intent.RewrittenQuery)
	assert.Equal(t, Level2Chunk, intent.LevelHint)
}

func TestVLMIntentAnalyzerEmptyResponse(t *testing.T) {
	t.Parallel()
	stub := vlm.NewStub()
	stub.ChatFn = func(_ context.Context, _ vlm.ChatRequest) (*vlm.ChatResponse, error) {
		return &vlm.ChatResponse{Content: ""}, nil
	}
	a := NewVLMIntentAnalyzer(stub, "m")
	intent, err := a.Analyze(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, "q", intent.RewrittenQuery)
}

func TestVLMIntentAnalyzerVLMError(t *testing.T) {
	t.Parallel()
	stub := vlm.NewStub()
	stub.ChatFn = func(_ context.Context, _ vlm.ChatRequest) (*vlm.ChatResponse, error) {
		return nil, domain.ErrVLMFailed
	}
	a := NewVLMIntentAnalyzer(stub, "m")
	_, err := a.Analyze(context.Background(), "q")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrVLMFailed))
}

func TestVLMIntentAnalyzerSendsJSONResponseFormat(t *testing.T) {
	t.Parallel()
	stub := vlm.NewStub()
	var captured vlm.ChatRequest
	stub.ChatFn = func(_ context.Context, req vlm.ChatRequest) (*vlm.ChatResponse, error) {
		captured = req
		return &vlm.ChatResponse{Content: `{}`}, nil
	}
	a := NewVLMIntentAnalyzer(stub, "gpt-4o")
	_, _ = a.Analyze(context.Background(), "q")
	require.NotNil(t, captured.ResponseFormat)
	assert.Equal(t, "json_object", captured.ResponseFormat.Type)
	assert.Equal(t, "gpt-4o", captured.Model)
	assert.Len(t, captured.Messages, 1)
	assert.Equal(t, vlm.RoleUser, captured.Messages[0].Role)
	assert.Contains(t, captured.Messages[0].Content, "q")
}

// NewStubVLM returns a vlm.Stub with no scripted responses. Used by
// tests that need a non-nil VLM but want to assert error paths.
func NewStubVLM() vlm.VLM { return vlm.NewStub() }
