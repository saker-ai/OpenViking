package session

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/saker-ai/ctxhub/internal/domain"
)

func TestMemoryExtractorExtract(t *testing.T) {
	llm := &fakeLLM{
		extractResult: []domain.ExtractedMemory{
			{Type: domain.MemoryTypeFact, Content: "user lives in Tokyo", Confidence: 0.9, SourceTurns: []int{0, 1}},
			{Type: domain.MemoryTypePreference, Content: "prefers tab indentation", Confidence: 0.8, SourceTurns: []int{2}},
		},
	}
	ex := NewMemoryExtractor(llm)
	sess := buildSession(4, 1)
	mem, err := ex.Extract(context.Background(), sess)
	require.NoError(t, err)
	require.Len(t, mem, 2)
	assert.NotEmpty(t, mem[0].ID)
	assert.False(t, mem[0].CreatedAt.IsZero())
	assert.Equal(t, domain.MemoryTypeFact, mem[0].Type)
	assert.InDelta(t, 0.9, mem[0].Confidence, 0.001)
}

func TestMemoryExtractorExtractEmptyOnNoTurns(t *testing.T) {
	ex := NewMemoryExtractor(&fakeLLM{})
	mem, err := ex.Extract(context.Background(), &domain.Session{})
	require.NoError(t, err)
	assert.Empty(t, mem)
}

func TestMemoryExtractorExtractPropagatesLLMError(t *testing.T) {
	llm := &fakeLLM{extractErr: errors.New("vlm down")}
	ex := NewMemoryExtractor(llm)
	sess := buildSession(2, 1)
	_, err := ex.Extract(context.Background(), sess)
	assert.Error(t, err)
}

func TestMemoryExtractorExtractClampsConfidence(t *testing.T) {
	llm := &fakeLLM{
		extractResult: []domain.ExtractedMemory{
			{Type: domain.MemoryTypeFact, Content: "x", Confidence: 1.5},
			{Type: domain.MemoryTypeFact, Content: "y", Confidence: -0.5},
		},
	}
	ex := NewMemoryExtractor(llm)
	sess := buildSession(2, 1)
	mem, err := ex.Extract(context.Background(), sess)
	require.NoError(t, err)
	require.Len(t, mem, 2)
	assert.Equal(t, 1.0, mem[0].Confidence)
	assert.Equal(t, 0.0, mem[1].Confidence)
}

func TestMemoryExtractorExtractNilLLMIsNoop(t *testing.T) {
	ex := NewMemoryExtractor(nil)
	sess := buildSession(2, 1)
	mem, err := ex.Extract(context.Background(), sess)
	require.NoError(t, err)
	assert.Empty(t, mem)
}

func TestMemoryExtractorDiff(t *testing.T) {
	ex := NewMemoryExtractor(&fakeLLM{})
	existing := []domain.ExtractedMemory{
		{ID: "m1", Type: domain.MemoryTypeFact, Content: "old1"},
		{ID: "m2", Type: domain.MemoryTypeFact, Content: "old2"},
		{ID: "m3", Type: domain.MemoryTypeFact, Content: "old3"},
	}
	extracted := []domain.ExtractedMemory{
		{ID: "m1", Type: domain.MemoryTypeFact, Content: "old1-updated", Confidence: 0.95},
		{ID: "m4", Type: domain.MemoryTypePreference, Content: "new"},
	}
	diff := ex.Diff(existing, extracted)
	assert.Len(t, diff.Updated, 1)
	assert.Equal(t, "m1", diff.Updated[0].ID)
	assert.Len(t, diff.Added, 1)
	assert.Equal(t, "m4", diff.Added[0].ID)
	assert.ElementsMatch(t, []string{"m2", "m3"}, diff.Archived)
}

func TestMemoryExtractorDiffEmptyExisting(t *testing.T) {
	ex := NewMemoryExtractor(&fakeLLM{})
	extracted := []domain.ExtractedMemory{
		{ID: "m1", Type: domain.MemoryTypeFact, Content: "new"},
	}
	diff := ex.Diff(nil, extracted)
	assert.Len(t, diff.Added, 1)
	assert.Empty(t, diff.Updated)
	assert.Empty(t, diff.Archived)
}

func TestStoreExtractMemoryWithExtractor(t *testing.T) {
	llm := &fakeLLM{
		extractResult: []domain.ExtractedMemory{
			{Type: domain.MemoryTypeFact, Content: "user likes go", Confidence: 0.9},
		},
	}
	store := NewMemoryStore(StoreConfig{Extractor: NewMemoryExtractor(llm)})
	id := domain.Identifier{Account: "acct"}
	sess, _ := store.Create(context.Background(), id)
	require.NoError(t, store.AppendTurn(context.Background(), id, sess.ID, newTurn(domain.TurnRoleUser, "I like go", 5)))

	mem, err := store.ExtractMemory(context.Background(), id, sess.ID)
	require.NoError(t, err)
	require.Len(t, mem, 1)
	assert.Equal(t, "user likes go", mem[0].Content)

	// Extraction persisted on session.
	loaded, _ := store.Get(context.Background(), id, sess.ID)
	require.Len(t, loaded.Memory, 1)
	assert.Equal(t, "user likes go", loaded.Memory[0].Content)
}
