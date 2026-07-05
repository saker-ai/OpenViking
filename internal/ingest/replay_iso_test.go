package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// TestParseISO verifies parseISO accepts RFC3339, RFC3339Nano, and the
// looser layout used by harness logs.
func TestParseISO(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"2024-01-01T00:00:00Z", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"2024-01-01T00:00:00.123456789Z", time.Date(2024, 1, 1, 0, 0, 0, 123456789, time.UTC), true},
		{"2024-01-01T12:30:45", time.Date(2024, 1, 1, 12, 30, 45, 0, time.UTC), true},
		{"not-a-date", time.Time{}, false},
		{"", time.Time{}, false},
	}
	for _, tc := range cases {
		got, err := parseISO(tc.in)
		if !tc.ok {
			assert.Error(t, err, "expected error for %q", tc.in)
			continue
		}
		require.NoError(t, err, "unexpected error for %q", tc.in)
		assert.True(t, got.Equal(tc.want), "got %v want %v for %q", got, tc.want, tc.in)
	}
}

// TestIsNotFound verifies isNotFound recognizes domain.ErrNotFound and
// ignores other errors.
func TestIsNotFound(t *testing.T) {
	assert.False(t, isNotFound(nil))
	assert.False(t, isNotFound(domain.NewAppError(domain.CodeInternalError, 500, "boom")))
	assert.True(t, isNotFound(domain.NewAppError(domain.CodeResourceNotFound, 404, "missing")))
	assert.False(t, isNotFound(assert.AnError))
}

// TestCursorStorePath verifies Path returns the database file path.
func TestCursorStorePath(t *testing.T) {
	s := newTestCursorStore(t)
	assert.NotEmpty(t, s.Path())
}

// mockReadSource is a Source with a configurable Read function for
// orchestrator backfill tests.
type mockReadSource struct {
	mockSource
	readFn func(ctx context.Context, ref SessionRef, cursor *Cursor, limit int) ([]NormalizedMessage, *Cursor, error)
}

func (m *mockReadSource) Read(ctx context.Context, ref SessionRef, cursor *Cursor, limit int) ([]NormalizedMessage, *Cursor, error) {
	return m.readFn(ctx, ref, cursor, limit)
}

// TestOrchestratorBackfillOne_DryRun verifies backfillOne in DryRun mode
// counts messages without appending.
func TestOrchestratorBackfillOne_DryRun(t *testing.T) {
	r := newTestReplayer(t)
	o := NewOrchestrator(r, nil)
	ctx := context.Background()

	callCount := 0
	src := &mockReadSource{
		mockSource: mockSource{
			id:         "test",
			cursorKind: CursorByteOffset,
			scanRefs:   []SessionRef{{NativeSessionID: "s1", Locator: "/path"}},
		},
		readFn: func(_ context.Context, _ SessionRef, cursor *Cursor, _ int) ([]NormalizedMessage, *Cursor, error) {
			callCount++
			if callCount > 1 {
				return nil, cursor, nil // EOF
			}
			return []NormalizedMessage{
					{Role: "user", Text: "hello"},
					{Role: "assistant", Text: "world"},
				},
				&Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(10)}},
				nil
		},
	}
	stats := o.BackfillSource(ctx, "test", src, BackfillOptions{DryRun: true})
	assert.Equal(t, 1, stats.Sessions)
	assert.Equal(t, 2, stats.Messages)
	assert.Equal(t, 0, stats.Committed)
}

// TestOrchestratorBackfillOne_RealRun verifies backfillOne appends messages
// and commits via the Replayer.
func TestOrchestratorBackfillOne_RealRun(t *testing.T) {
	r := newTestReplayer(t)
	o := NewOrchestrator(r, nil)
	ctx := context.Background()

	callCount := 0
	src := &mockReadSource{
		mockSource: mockSource{
			id:         "test",
			cursorKind: CursorByteOffset,
			scanRefs:   []SessionRef{{NativeSessionID: "s1", Locator: "/path"}},
		},
		readFn: func(_ context.Context, _ SessionRef, cursor *Cursor, _ int) ([]NormalizedMessage, *Cursor, error) {
			callCount++
			if callCount > 1 {
				return nil, cursor, nil // EOF
			}
			return []NormalizedMessage{
					{Role: "user", Text: "hello"},
					{Role: "assistant", Text: "world"},
				},
				&Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(10)}},
				nil
		},
	}
	stats := o.BackfillSource(ctx, "test", src, BackfillOptions{})
	assert.Equal(t, 1, stats.Sessions)
	assert.Equal(t, 2, stats.Messages)
	assert.Equal(t, 1, stats.Committed)
}

// TestOrchestratorBackfillOne_ReadError verifies backfillOne records a read
// error in stats.
func TestOrchestratorBackfillOne_ReadError(t *testing.T) {
	r := newTestReplayer(t)
	o := NewOrchestrator(r, nil)
	ctx := context.Background()

	src := &mockReadSource{
		mockSource: mockSource{
			id:         "test",
			cursorKind: CursorByteOffset,
			scanRefs:   []SessionRef{{NativeSessionID: "s1", Locator: "/path"}},
		},
		readFn: func(_ context.Context, _ SessionRef, _ *Cursor, _ int) ([]NormalizedMessage, *Cursor, error) {
			return nil, nil, assert.AnError
		},
	}
	stats := o.BackfillSource(ctx, "test", src, BackfillOptions{})
	// backfillOne returns a read error; the session is not counted but the
	// error is recorded.
	assert.Equal(t, 0, stats.Sessions)
	assert.NotEmpty(t, stats.Errors)
}

// TestOrchestratorBackfillOne_NoReplayer verifies backfillOne records an
// error when the orchestrator has no Replayer.
func TestOrchestratorBackfillOne_NoReplayer(t *testing.T) {
	o := NewOrchestrator(nil, nil)
	ctx := context.Background()
	src := &mockReadSource{
		mockSource: mockSource{
			id:         "test",
			cursorKind: CursorByteOffset,
			scanRefs:   []SessionRef{{NativeSessionID: "s1", Locator: "/path"}},
		},
		readFn: func(_ context.Context, _ SessionRef, _ *Cursor, _ int) ([]NormalizedMessage, *Cursor, error) {
			return nil, nil, nil
		},
	}
	stats := o.BackfillSource(ctx, "test", src, BackfillOptions{})
	// backfillOne returns an error (no Replayer); the session is not counted
	// but the error is recorded.
	assert.Equal(t, 0, stats.Sessions)
	assert.NotEmpty(t, stats.Errors)
}
