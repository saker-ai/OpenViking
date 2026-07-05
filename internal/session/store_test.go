package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/saker-ai/ctxhub/internal/domain"
)

func newTurn(role domain.TurnRole, content string, tokens int) domain.Turn {
	return domain.Turn{Role: role, Content: content, Tokens: tokens}
}

func TestMemoryStoreCreate(t *testing.T) {
	store := NewMemoryStore(StoreConfig{})
	id := domain.Identifier{Account: "acct", User: "u", ActorPeer: "p"}
	sess, err := store.Create(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.NotEmpty(t, sess.ID)
	assert.Equal(t, domain.SessionStatusActive, sess.Status)
	assert.Equal(t, "acct", sess.Account)
	assert.Equal(t, "u", sess.User)
	assert.Equal(t, "p", sess.Peer)
	assert.False(t, sess.CreatedAt.IsZero())
}

func TestMemoryStoreGetNotFound(t *testing.T) {
	store := NewMemoryStore(StoreConfig{})
	id := domain.Identifier{Account: "acct"}
	_, err := store.Get(context.Background(), id, "missing")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestMemoryStoreAppendTurnAndTokenUsage(t *testing.T) {
	store := NewMemoryStore(StoreConfig{})
	id := domain.Identifier{Account: "acct"}
	sess, err := store.Create(context.Background(), id)
	require.NoError(t, err)

	err = store.AppendTurn(context.Background(), id, sess.ID, newTurn(domain.TurnRoleUser, "hi", 10))
	require.NoError(t, err)
	err = store.AppendTurn(context.Background(), id, sess.ID, newTurn(domain.TurnRoleAssistant, "hello", 20))
	require.NoError(t, err)

	loaded, err := store.Get(context.Background(), id, sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Turns, 2)
	assert.NotEmpty(t, loaded.Turns[0].ID)
	assert.False(t, loaded.Turns[0].CreatedAt.IsZero())
	assert.Equal(t, 30, loaded.TokenUsage.PromptTokens)
	assert.Equal(t, 30, loaded.TokenUsage.CompletionTokens)
	assert.Equal(t, 60, loaded.TokenUsage.TotalTokens)
}

func TestMemoryStoreAppendTurnArchivedRejected(t *testing.T) {
	store := NewMemoryStore(StoreConfig{})
	id := domain.Identifier{Account: "acct"}
	sess, err := store.Create(context.Background(), id)
	require.NoError(t, err)
	require.NoError(t, store.Archive(context.Background(), id, sess.ID))
	err = store.AppendTurn(context.Background(), id, sess.ID, newTurn(domain.TurnRoleUser, "x", 1))
	assert.ErrorIs(t, err, domain.ErrConflict)
}

func TestMemoryStoreListTenantScoped(t *testing.T) {
	store := NewMemoryStore(StoreConfig{})
	alice := domain.Identifier{Account: "acct", User: "alice"}
	bob := domain.Identifier{Account: "acct", User: "bob"}
	s1, _ := store.Create(context.Background(), alice)
	s2, _ := store.Create(context.Background(), alice)
	s3, _ := store.Create(context.Background(), bob)

	got, err := store.List(context.Background(), alice)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	ids := []string{got[0].ID, got[1].ID}
	assert.Contains(t, ids, s1.ID)
	assert.Contains(t, ids, s2.ID)
	assert.NotContains(t, ids, s3.ID)
}

func TestMemoryStoreListSortedDescByCreatedAt(t *testing.T) {
	store := NewMemoryStore(StoreConfig{})
	id := domain.Identifier{Account: "acct"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now = func() time.Time { return base }
	a, _ := store.Create(context.Background(), id)
	now = func() time.Time { return base.Add(2 * time.Hour) }
	b, _ := store.Create(context.Background(), id)
	now = func() time.Time { return base.Add(1 * time.Hour) }
	c, _ := store.Create(context.Background(), id)
	got, _ := store.List(context.Background(), id)
	require.Len(t, got, 3)
	assert.Equal(t, b.ID, got[0].ID) // newest first
	assert.Equal(t, c.ID, got[1].ID)
	assert.Equal(t, a.ID, got[2].ID) // oldest last
}

func TestMemoryStoreCommitAndArchive(t *testing.T) {
	store := NewMemoryStore(StoreConfig{})
	id := domain.Identifier{Account: "acct"}
	sess, err := store.Create(context.Background(), id)
	require.NoError(t, err)
	require.NoError(t, store.AppendTurn(context.Background(), id, sess.ID, newTurn(domain.TurnRoleUser, "hi", 10)))

	committed, err := store.Commit(context.Background(), id, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.SessionStatusCommitted, committed.Status)
	assert.NotNil(t, committed.CommittedAt)

	require.NoError(t, store.Archive(context.Background(), id, sess.ID))
	loaded, err := store.Get(context.Background(), id, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.SessionStatusArchived, loaded.Status)
}

func TestMemoryStoreCommitArchivedRejected(t *testing.T) {
	store := NewMemoryStore(StoreConfig{})
	id := domain.Identifier{Account: "acct"}
	sess, err := store.Create(context.Background(), id)
	require.NoError(t, err)
	require.NoError(t, store.Archive(context.Background(), id, sess.ID))
	_, err = store.Commit(context.Background(), id, sess.ID)
	assert.ErrorIs(t, err, domain.ErrConflict)
}

func TestMemoryStoreApplyMemoryDiff(t *testing.T) {
	store := NewMemoryStore(StoreConfig{})
	id := domain.Identifier{Account: "acct"}
	sess, err := store.Create(context.Background(), id)
	require.NoError(t, err)

	// Seed existing memory.
	require.NoError(t, store.ApplyMemoryDiff(context.Background(), id, sess.ID, domain.MemoryDiff{
		Added: []domain.ExtractedMemory{
			{ID: "mem_a", Type: domain.MemoryTypeFact, Content: "user likes go", Confidence: 0.9},
			{ID: "mem_b", Type: domain.MemoryTypePreference, Content: "prefers dark mode", Confidence: 0.8},
		},
	}))
	loaded, err := store.Get(context.Background(), id, sess.ID)
	require.NoError(t, err)
	assert.Len(t, loaded.Memory, 2)

	// Update + Archive + Add.
	require.NoError(t, store.ApplyMemoryDiff(context.Background(), id, sess.ID, domain.MemoryDiff{
		Added: []domain.ExtractedMemory{
			{ID: "mem_c", Type: domain.MemoryTypeSkill, Content: "knows k8s", Confidence: 0.7},
		},
		Updated: []domain.ExtractedMemory{
			{ID: "mem_a", Type: domain.MemoryTypeFact, Content: "user likes go and rust", Confidence: 0.95},
		},
		Archived: []string{"mem_b"},
	}))
	loaded, err = store.Get(context.Background(), id, sess.ID)
	require.NoError(t, err)
	assert.Len(t, loaded.Memory, 2)
	byID := map[string]domain.ExtractedMemory{}
	for _, m := range loaded.Memory {
		byID[m.ID] = m
	}
	assert.Contains(t, byID, "mem_a")
	assert.Contains(t, byID, "mem_c")
	assert.NotContains(t, byID, "mem_b")
	assert.Equal(t, "user likes go and rust", byID["mem_a"].Content)
	assert.Equal(t, 0.95, byID["mem_a"].Confidence)
}

func TestMemoryStoreExtractMemoryWithoutExtractor(t *testing.T) {
	store := NewMemoryStore(StoreConfig{})
	id := domain.Identifier{Account: "acct"}
	sess, _ := store.Create(context.Background(), id)
	require.NoError(t, store.ApplyMemoryDiff(context.Background(), id, sess.ID, domain.MemoryDiff{
		Added: []domain.ExtractedMemory{{ID: "mem_x", Type: domain.MemoryTypeFact, Content: "fact"}},
	}))
	mem, err := store.ExtractMemory(context.Background(), id, sess.ID)
	require.NoError(t, err)
	assert.Len(t, mem, 1)
	assert.Equal(t, "mem_x", mem[0].ID)
}
