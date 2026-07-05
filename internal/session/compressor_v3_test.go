package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/saker-ai/ctxhub/internal/domain"
)

// fakeLLM is a test double for LLMClient. It records call counts and
// returns canned responses, optionally failing to exercise the v3 -> v2
// fallback path.
type fakeLLM struct {
	summarizeCalls int32
	extractCalls   int32
	summaries      map[int]string // keyed by call index
	summaryErr     error
	extractErr     error
	extractResult  []domain.ExtractedMemory
}

func (f *fakeLLM) Summarize(ctx context.Context, turns []domain.Turn) (string, error) {
	idx := atomic.AddInt32(&f.summarizeCalls, 1)
	if f.summaryErr != nil {
		return "", f.summaryErr
	}
	if s, ok := f.summaries[int(idx)]; ok {
		return s, nil
	}
	return "summary-of-chunk", nil
}

func (f *fakeLLM) Extract(ctx context.Context, turns []domain.Turn) ([]domain.ExtractedMemory, error) {
	atomic.AddInt32(&f.extractCalls, 1)
	if f.extractErr != nil {
		return nil, f.extractErr
	}
	return f.extractResult, nil
}

func TestCompressorV3SummarizesAndPrependsSummary(t *testing.T) {
	llm := &fakeLLM{}
	c := NewCompressorV3(CompressorConfig{
		MaxTurns:         5,
		MaxContextTokens: 32000,
		KeepLastNTurns:   3,
		SummaryChunkSize: 2,
		LLM:              llm,
	})
	sess := buildSession(10, 1) // 10 turns, MaxTurns=5 -> compress; keep=3 -> 7 dropped
	out, err := c.Compress(context.Background(), sess)
	require.NoError(t, err)
	// head(anchor) + summary turn + 3 tail = 5 turns
	require.Len(t, out.Turns, 5)
	// First preserved turn is the anchor.
	assert.Equal(t, sess.Turns[0].ID, out.Turns[0].ID)
	// Second turn is the synthetic summary turn.
	assert.Equal(t, domain.TurnRoleAssistant, out.Turns[1].Role)
	assert.Contains(t, out.Turns[1].Content, "[v3 summary]")
	// Last 3 turns preserved.
	assert.Equal(t, sess.Turns[9].ID, out.Turns[4].ID)
	// LLM was called for each chunk (7 dropped / chunkSize 2 -> 4 calls).
	assert.Equal(t, int32(4), atomic.LoadInt32(&llm.summarizeCalls))
	// Summary combines the per-chunk summaries.
	assert.Contains(t, out.Summary, "summary-of-chunk")
}

func TestCompressorV3FallsBackToV2OnLLMError(t *testing.T) {
	llm := &fakeLLM{summaryErr: errors.New("boom")}
	c := NewCompressorV3(CompressorConfig{
		MaxTurns:         5,
		MaxContextTokens: 32000,
		KeepLastNTurns:   3,
		SummaryChunkSize: 2,
		LLM:              llm,
	})
	sess := buildSession(10, 1)
	out, err := c.Compress(context.Background(), sess)
	require.NoError(t, err)
	// v2 fallback: head + 3 tail = 4 turns (no synthetic summary turn).
	require.Len(t, out.Turns, 4)
	assert.Contains(t, out.Summary, "[v2 truncation]")
}

func TestCompressorV3BelowThreshold(t *testing.T) {
	c := NewCompressorV3(CompressorConfig{
		MaxTurns:         20,
		MaxContextTokens: 32000,
		KeepLastNTurns:   3,
		LLM:              &fakeLLM{},
	})
	sess := buildSession(3, 1)
	out, err := c.Compress(context.Background(), sess)
	require.NoError(t, err)
	assert.Len(t, out.Turns, 3)
	assert.Empty(t, out.Summary)
}

func TestCompressorV3NilLLMNoPanics(t *testing.T) {
	c := NewCompressorV3(CompressorConfig{
		MaxTurns:         5,
		MaxContextTokens: 32000,
		KeepLastNTurns:   3,
		SummaryChunkSize: 2,
		// LLM nil -> factory substitutes noop
	})
	// Manually trigger compression via the configured v3 (noop LLM).
	sess := buildSession(10, 1)
	out, err := c.Compress(context.Background(), sess)
	require.NoError(t, err)
	// noop LLM returns empty summary without error; pipeline still runs.
	assert.NotEmpty(t, out.Turns)
}

func TestNewCompressorFactory(t *testing.T) {
	t.Run("v2", func(t *testing.T) {
		c, err := NewCompressor(CompressorConfig{Version: "v2"})
		require.NoError(t, err)
		_, ok := c.(*CompressorV2)
		assert.True(t, ok)
	})
	t.Run("v3", func(t *testing.T) {
		c, err := NewCompressor(CompressorConfig{Version: "v3", LLM: &fakeLLM{}})
		require.NoError(t, err)
		_, ok := c.(*CompressorV3)
		assert.True(t, ok)
	})
	t.Run("auto", func(t *testing.T) {
		c, err := NewCompressor(CompressorConfig{Version: "auto", LLM: &fakeLLM{}, AutoV3Turns: 5})
		require.NoError(t, err)
		ac, ok := c.(*AutoCompressor)
		require.True(t, ok)
		// Below threshold -> v2 path.
		sess := buildSession(3, 1)
		_, err = ac.Compress(context.Background(), sess)
		require.NoError(t, err)
		// At or above threshold -> v3 path.
		sess = buildSession(6, 1)
		_, err = ac.Compress(context.Background(), sess)
		require.NoError(t, err)
	})
	t.Run("unknown", func(t *testing.T) {
		_, err := NewCompressor(CompressorConfig{Version: "v9"})
		assert.Error(t, err)
	})
}
