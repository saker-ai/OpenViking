package ingest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// mockSource is a configurable Source for orchestrator tests. It allows
// customizing Scan behavior without needing a real database.
type mockSource struct {
	id         string
	scanErr    error
	scanRefs   []SessionRef
	cursorKind string
	groupChat  bool
}

func (m *mockSource) ID() string         { return m.id }
func (m *mockSource) CursorKind() string { return m.cursorKind }
func (m *mockSource) IsGroupChat() bool  { return m.groupChat }
func (m *mockSource) Close() error       { return nil }
func (m *mockSource) Read(_ context.Context, _ SessionRef, _ *Cursor, _ int) ([]NormalizedMessage, *Cursor, error) {
	return nil, nil, domain.ErrUnsupported
}
func (m *mockSource) Scan(_ context.Context, fn func(SessionRef) error) error {
	if m.scanErr != nil {
		return m.scanErr
	}
	for _, ref := range m.scanRefs {
		if err := fn(ref); err != nil {
			return err
		}
	}
	return nil
}

// TestOrchestratorRegister verifies Register stores a source under the
// given name and overwrites on re-registration.
func TestOrchestratorRegister(t *testing.T) {
	o := NewOrchestrator(nil, nil)
	src := &mockSource{id: "test"}
	o.Register("test", src)
	require.Contains(t, o.Sources, "test")

	// Re-register overwrites.
	src2 := &mockSource{id: "test2"}
	o.Register("test", src2)
	assert.Same(t, src2, o.Sources["test"])
}

// TestOrchestratorBackfillSource_ScanError verifies BackfillSource records
// a scan error in stats.Errors and returns without panicking.
func TestOrchestratorBackfillSource_ScanError(t *testing.T) {
	o := NewOrchestrator(nil, nil)
	src := &mockSource{
		id:      "errsrc",
		scanErr: domain.ErrUnsupported,
	}
	stats := o.BackfillSource(context.Background(), "errsrc", src, BackfillOptions{})
	assert.Equal(t, 0, stats.Sessions)
	assert.NotEmpty(t, stats.Errors)
	assert.Contains(t, stats.Errors[0], "scan")
}

// TestOrchestratorBackfillSource_EmptyScan verifies BackfillSource with a
// source that discovers no sessions returns zero stats with no errors.
func TestOrchestratorBackfillSource_EmptyScan(t *testing.T) {
	o := NewOrchestrator(nil, nil)
	src := &mockSource{id: "emptysrc"}
	stats := o.BackfillSource(context.Background(), "emptysrc", src, BackfillOptions{})
	assert.Equal(t, 0, stats.Sessions)
	assert.Empty(t, stats.Errors)
}

// TestOrchestratorBackfillSource_SinceFilter verifies the Since option
// skips sessions older than the cutoff.
func TestOrchestratorBackfillSource_SinceFilter(t *testing.T) {
	o := NewOrchestrator(nil, nil)
	src := &mockSource{
		id: "sincesrc",
		scanRefs: []SessionRef{
			{NativeSessionID: "old", StartedAt: "2020-01-01T00:00:00Z"},
			{NativeSessionID: "new", StartedAt: "2026-01-01T00:00:00Z"},
		},
	}
	// Since="2025-01-01" should skip the "old" session. The "new" session
	// will reach backfillOne which needs a Replayer; since Replayer is nil,
	// it returns an error that gets recorded in stats.Errors.
	stats := o.BackfillSource(context.Background(), "sincesrc", src, BackfillOptions{
		Since: "2025-01-01T00:00:00Z",
	})
	// The old session is skipped (Skipped=1); the new session reaches
	// backfillOne which errors (no Replayer).
	assert.Equal(t, 1, stats.Skipped)
	assert.NotEmpty(t, stats.Errors)
}

// TestOrchestratorBackfill_NoSources verifies Backfill on an orchestrator
// with no sources returns an empty map.
func TestOrchestratorBackfill_NoSources(t *testing.T) {
	o := NewOrchestrator(nil, nil)
	result := o.Backfill(context.Background(), BackfillOptions{})
	assert.Empty(t, result)
}

// TestOrchestratorBackfill_MultipleSources verifies Backfill runs each
// registered source and returns per-source stats.
func TestOrchestratorBackfill_MultipleSources(t *testing.T) {
	o := NewOrchestrator(nil, map[string]Source{
		"a": &mockSource{id: "a"},
		"b": &mockSource{id: "b", scanErr: domain.ErrUnsupported},
	})
	result := o.Backfill(context.Background(), BackfillOptions{})
	require.Len(t, result, 2)
	assert.Contains(t, result, "a")
	assert.Contains(t, result, "b")
	// Source "b" had a scan error.
	assert.NotEmpty(t, result["b"].Errors)
}
