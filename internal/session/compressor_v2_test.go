package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/saker-ai/ctxhub/internal/domain"
)

func buildSession(turns int, tokensPer int) *domain.Session {
	t := make([]domain.Turn, 0, turns)
	for i := 0; i < turns; i++ {
		role := domain.TurnRoleUser
		if i%2 == 1 {
			role = domain.TurnRoleAssistant
		}
		t = append(t, domain.Turn{
			ID:      newTurnID(),
			Role:    role,
			Content: "msg",
			Tokens:  tokensPer,
		})
	}
	return &domain.Session{ID: newSessionID(), Status: domain.SessionStatusActive, Turns: t}
}

func TestCompressorV2BelowThreshold(t *testing.T) {
	c := NewCompressorV2(CompressorConfig{MaxTurns: 20, MaxContextTokens: 32000, KeepLastNTurns: 10})
	sess := buildSession(5, 10)
	out, err := c.Compress(context.Background(), sess)
	require.NoError(t, err)
	assert.Equal(t, 5, len(out.Turns))
	assert.Empty(t, out.Summary)
}

func TestCompressorV2TruncationPreservesLastNAndAnchor(t *testing.T) {
	c := NewCompressorV2(CompressorConfig{MaxTurns: 5, MaxContextTokens: 32000, KeepLastNTurns: 3})
	sess := buildSession(10, 5) // 10 turns, exceeds MaxTurns=5
	out, err := c.Compress(context.Background(), sess)
	require.NoError(t, err)
	// head (anchor) + 3 tail = 4 turns
	require.Len(t, out.Turns, 4)
	// First preserved turn is the anchor (first user turn).
	assert.Equal(t, domain.TurnRoleUser, out.Turns[0].Role)
	// Last 3 turns preserved.
	assert.Equal(t, sess.Turns[9].ID, out.Turns[3].ID)
	assert.Equal(t, sess.Turns[8].ID, out.Turns[2].ID)
	assert.Equal(t, sess.Turns[7].ID, out.Turns[1].ID)
	// Summary records dropped turns count (10 - 1 anchor - 3 tail = 6 dropped).
	assert.Contains(t, out.Summary, "dropped 6 turn(s)")
}

func TestCompressorV2TokenThresholdTriggers(t *testing.T) {
	c := NewCompressorV2(CompressorConfig{MaxTurns: 100, MaxContextTokens: 100, KeepLastNTurns: 2})
	sess := buildSession(10, 20) // 200 tokens total, exceeds 100
	out, err := c.Compress(context.Background(), sess)
	require.NoError(t, err)
	assert.Less(t, len(out.Turns), 10)
	assert.NotEmpty(t, out.Summary)
}

func TestCompressorV2AnchorInsideTail(t *testing.T) {
	// When the anchor (first user/assistant turn) sits inside the last N,
	// the head is folded into the tail and we just keep the last N.
	c := NewCompressorV2(CompressorConfig{MaxTurns: 3, MaxContextTokens: 32000, KeepLastNTurns: 5})
	sess := buildSession(6, 1) // 6 turns > MaxTurns=3, but KeepLastNTurns=5 >= anchor(0)+tail
	out, err := c.Compress(context.Background(), sess)
	require.NoError(t, err)
	require.Len(t, out.Turns, 5)
	assert.Equal(t, sess.Turns[1].ID, out.Turns[0].ID) // tail starts at index 1
}

func TestCompressorV2NilSession(t *testing.T) {
	c := NewCompressorV2(CompressorConfig{})
	_, err := c.Compress(context.Background(), nil)
	assert.Error(t, err)
}

func TestCompressorV2DoesNotMutateInput(t *testing.T) {
	c := NewCompressorV2(CompressorConfig{MaxTurns: 3, KeepLastNTurns: 2})
	sess := buildSession(8, 1)
	original := append([]domain.Turn(nil), sess.Turns...)
	_, _ = c.Compress(context.Background(), sess)
	assert.Equal(t, original, sess.Turns)
}
