package cli

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubFeedbackStore returns a fixed set of stats rows.
type stubFeedbackStore struct {
	rows  []FeedbackStats
	err   error
	calls int
	since time.Duration
}

func (s *stubFeedbackStore) Stats(_ context.Context, since time.Duration) ([]FeedbackStats, error) {
	s.calls++
	s.since = since
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func TestFeedbackStatsCmd_NoData(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.Feedback = &stubFeedbackStore{err: ErrNoFeedbackData}

	cmd := silence(FeedbackStatsCmd(rt))
	require.NoError(t, Execute(cmd, []string{}))
	assert.Contains(t, out.String(), "no feedback data")
}

func TestFeedbackStatsCmd_EmptyRows(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.Feedback = &stubFeedbackStore{rows: nil}

	cmd := silence(FeedbackStatsCmd(rt))
	require.NoError(t, Execute(cmd, []string{}))
	assert.Contains(t, out.String(), "no feedback data")
}

func TestFeedbackStatsCmd_Table(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.Feedback = &stubFeedbackStore{rows: []FeedbackStats{
		{Period: "7d", Positive: 12, Negative: 3, NetScore: 9},
		{Period: "24h", Positive: 4, Negative: 1, NetScore: 3},
	}}

	cmd := silence(FeedbackStatsCmd(rt))
	require.NoError(t, Execute(cmd, []string{}))
	body := out.String()
	assert.Contains(t, body, "PERIOD")
	assert.Contains(t, body, "POSITIVE")
	assert.Contains(t, body, "NEGATIVE")
	assert.Contains(t, body, "NET_SCORE")
	assert.Contains(t, body, "7d")
	assert.Contains(t, body, "24h")
	assert.Contains(t, body, "12")
}

func TestFeedbackStatsCmd_JSON(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	rt.Feedback = &stubFeedbackStore{rows: []FeedbackStats{
		{Period: "7d", Positive: 12, Negative: 3, NetScore: 9},
	}}

	cmd := silence(FeedbackStatsCmd(rt))
	require.NoError(t, Execute(cmd, []string{"--json"}))
	body := out.String()
	assert.Contains(t, body, `"period": "7d"`)
	assert.Contains(t, body, `"positive": 12`)
	assert.Contains(t, body, `"net_score": 9`)
}

func TestFeedbackStatsCmd_SinceFlag(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	store := &stubFeedbackStore{rows: []FeedbackStats{{Period: "30d"}}}
	rt.Feedback = store

	cmd := silence(FeedbackStatsCmd(rt))
	require.NoError(t, Execute(cmd, []string{"--since", "30d"}))
	assert.Contains(t, out.String(), "30d")
	assert.Equal(t, 30*24*time.Hour, store.since)
}

func TestFeedbackStatsCmd_DefaultStore(t *testing.T) {
	// When rt.Feedback is nil, the command falls back to
	// StubFeedbackStore which always returns ErrNoFeedbackData.
	rt, out, _ := newTestRuntimeNoServer(t)

	cmd := silence(FeedbackStatsCmd(rt))
	require.NoError(t, Execute(cmd, []string{}))
	assert.Contains(t, out.String(), "no feedback data")
}

func TestFeedbackStatsCmd_InvalidSince(t *testing.T) {
	rt, _, _ := newTestRuntimeNoServer(t)
	rt.Feedback = &stubFeedbackStore{}

	cmd := silence(FeedbackStatsCmd(rt))
	err := Execute(cmd, []string{"--since", "abc"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--since")
}

func TestFeedbackStatsCmd_Help(t *testing.T) {
	rt, out, _ := newTestRuntimeNoServer(t)
	cmd := FeedbackStatsCmd(rt)
	cmd.SetOut(out)
	cmd.SetErr(out)
	require.NoError(t, Execute(cmd, []string{"--help"}))
	assert.Contains(t, out.String(), "Usage")
}

func TestStubFeedbackStore(t *testing.T) {
	s := NewStubFeedbackStore()
	_, err := s.Stats(context.Background(), 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoFeedbackData)
}
