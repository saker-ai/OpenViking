package ingest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/session"
)

// newTestReplayer builds a Replayer wired to an in-memory session store and
// a temp-dir cursor store for integration-style unit tests.
func newTestReplayer(t *testing.T) *Replayer {
	t.Helper()
	store := newTestCursorStore(t)
	sessions := session.NewMemoryStore(session.StoreConfig{})
	return NewReplayer(store, sessions, "testacct")
}

// TestReplayerReconcile_Create verifies Reconcile creates a new OV session
// when none exists yet and records it in the cursor store.
func TestReplayerReconcile_Create(t *testing.T) {
	r := newTestReplayer(t)
	ctx := context.Background()

	sess, created, err := r.Reconcile(ctx, "claude_code", "native1", "/path/to/locator", "Title")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.True(t, created)
	assert.NotEmpty(t, sess.ID)

	// Second call with the same native ID returns the existing session.
	sess2, created2, err := r.Reconcile(ctx, "claude_code", "native1", "/path/to/locator", "Title")
	require.NoError(t, err)
	assert.False(t, created2)
	assert.Equal(t, sess.ID, sess2.ID)
}

// TestReplayerAppendBatch_Empty verifies AppendBatch with no messages is a
// no-op returning 0.
func TestReplayerAppendBatch_Empty(t *testing.T) {
	r := newTestReplayer(t)
	ctx := context.Background()
	n, err := r.AppendBatch(ctx, "claude_code", "native1", nil)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// TestReplayerAppendBatch_NotReconciled verifies AppendBatch on a session
// that has not been Reconciled returns ErrNotFound.
func TestReplayerAppendBatch_NotReconciled(t *testing.T) {
	r := newTestReplayer(t)
	ctx := context.Background()
	msgs := []NormalizedMessage{{Role: "user", Text: "hello"}}
	_, err := r.AppendBatch(ctx, "claude_code", "never-reconciled", msgs)
	require.Error(t, err)
}

// TestReplayerAppendBatch_AndCommit verifies the full replay flow: append
// messages, mark pending, then commit.
func TestReplayerAppendBatch_AndCommit(t *testing.T) {
	r := newTestReplayer(t)
	ctx := context.Background()
	_, _, err := r.Reconcile(ctx, "claude_code", "native1", "/path", "Title")
	require.NoError(t, err)

	msgs := []NormalizedMessage{
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: "hi there"},
	}
	n, err := r.AppendBatch(ctx, "claude_code", "native1", msgs)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// Mark pending so CommitIfNeeded sees NeedsCommit=true (the orchestrator
	// calls SetPending after each batch).
	require.NoError(t, r.Store.SetPending(ctx, "claude_code", "native1",
		ZeroCursor(CursorByteOffset), 2, 0))

	// Commit.
	committed, err := r.CommitIfNeeded(ctx, "claude_code", "native1")
	require.NoError(t, err)
	assert.True(t, committed)

	// Second commit is a no-op (already committed).
	committed2, err := r.CommitIfNeeded(ctx, "claude_code", "native1")
	require.NoError(t, err)
	assert.False(t, committed2)
}

// TestReplayerCommitIfNeeded_NotPending verifies CommitIfNeeded is a no-op
// when NeedsCommit is false.
func TestReplayerCommitIfNeeded_NotPending(t *testing.T) {
	r := newTestReplayer(t)
	ctx := context.Background()
	_, _, err := r.Reconcile(ctx, "claude_code", "native1", "/path", "Title")
	require.NoError(t, err)

	// No SetPending called, so NeedsCommit is false.
	committed, err := r.CommitIfNeeded(ctx, "claude_code", "native1")
	require.NoError(t, err)
	assert.False(t, committed)
}

// TestReplayerResetSession verifies ResetSession resets the cursor so the
// next Reconcile re-creates the OV session (the deterministic alias from
// OVSessionID doesn't match the store-assigned ID, so a new session is
// allocated).
func TestReplayerResetSession(t *testing.T) {
	r := newTestReplayer(t)
	ctx := context.Background()
	sess1, _, err := r.Reconcile(ctx, "claude_code", "native1", "/path", "Title")
	require.NoError(t, err)
	require.NotEmpty(t, sess1.ID)

	// Reset the cursor to zero and point at the deterministic alias.
	require.NoError(t, r.ResetSession(ctx, "claude_code", "native1"))

	// After reset, Reconcile looks up the alias (not the original store ID),
	// doesn't find it, and creates a fresh session.
	sess2, created, err := r.Reconcile(ctx, "claude_code", "native1", "/path", "Title")
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotEqual(t, sess1.ID, sess2.ID)
}

// TestReplayerResetSession_NilStore verifies ResetSession on a Replayer
// with no cursor store is a safe no-op.
func TestReplayerResetSession_NilStore(t *testing.T) {
	r := &Replayer{}
	require.NoError(t, r.ResetSession(context.Background(), "claude_code", "native1"))
}
