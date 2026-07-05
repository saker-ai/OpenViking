package ingest

import (
	"context"
	"testing"
)

func newTestCursorStore(t *testing.T) *CursorStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewCursorStore(dir)
	if err != nil {
		t.Fatalf("NewCursorStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCursorStore_GetMissing(t *testing.T) {
	s := newTestCursorStore(t)
	rec, err := s.Get(context.Background(), "claude_code", "nonexistent")
	if err != nil {
		t.Fatalf("Get missing row: %v", err)
	}
	if rec != nil {
		t.Error("Get returned non-nil for missing row")
	}
}

func TestCursorStore_GetCursorMissing(t *testing.T) {
	s := newTestCursorStore(t)
	c, err := s.GetCursor(context.Background(), "claude_code", "nonexistent", CursorByteOffset)
	if err != nil {
		t.Fatalf("GetCursor missing: %v", err)
	}
	if c != nil {
		t.Error("GetCursor returned non-nil for missing row")
	}
}

func TestCursorStore_EnsureRow(t *testing.T) {
	s := newTestCursorStore(t)
	ctx := context.Background()
	cursor := ZeroCursor(CursorByteOffset)
	err := s.EnsureRow(ctx, "claude_code", "session-1", "claude_code__session-1", cursor, "/path/to/log.jsonl", "Test Session")
	if err != nil {
		t.Fatalf("EnsureRow: %v", err)
	}
	rec, err := s.Get(ctx, "claude_code", "session-1")
	if err != nil {
		t.Fatalf("Get after EnsureRow: %v", err)
	}
	if rec == nil {
		t.Fatal("Get returned nil after EnsureRow")
	}
	if rec.Harness != "claude_code" {
		t.Errorf("Harness = %q, want %q", rec.Harness, "claude_code")
	}
	if rec.NativeSessionID != "session-1" {
		t.Errorf("NativeSessionID = %q, want %q", rec.NativeSessionID, "session-1")
	}
	if rec.OVSessionID != "claude_code__session-1" {
		t.Errorf("OVSessionID = %q, want %q", rec.OVSessionID, "claude_code__session-1")
	}
	if rec.Locator != "/path/to/log.jsonl" {
		t.Errorf("Locator = %q, want %q", rec.Locator, "/path/to/log.jsonl")
	}
	if rec.Title != "Test Session" {
		t.Errorf("Title = %q, want %q", rec.Title, "Test Session")
	}
	if !rec.Cursor.Equal(cursor) {
		t.Error("cursor does not match input")
	}
}

func TestCursorStore_EnsureRowIdempotent(t *testing.T) {
	s := newTestCursorStore(t)
	ctx := context.Background()
	cursor := ZeroCursor(CursorByteOffset)
	if err := s.EnsureRow(ctx, "h", "s", "ov", cursor, "loc", "title"); err != nil {
		t.Fatalf("first EnsureRow: %v", err)
	}
	if err := s.EnsureRow(ctx, "h", "s", "ov", cursor, "loc", "title"); err != nil {
		t.Fatalf("second EnsureRow: %v", err)
	}
	rec, _ := s.Get(ctx, "h", "s")
	if rec == nil {
		t.Fatal("Get returned nil after double EnsureRow")
	}
}

func TestCursorStore_AdvanceCursor(t *testing.T) {
	s := newTestCursorStore(t)
	ctx := context.Background()
	advanced := &Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(1024)}}
	err := s.AdvanceCursor(ctx, "claude_code", "session-2", "ov-2", advanced, "/path/log.jsonl")
	if err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}
	rec, err := s.Get(ctx, "claude_code", "session-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec == nil {
		t.Fatal("Get returned nil")
	}
	if rec.Cursor.Offset() != 1024 {
		t.Errorf("Offset = %d, want 1024", rec.Cursor.Offset())
	}
}

func TestCursorStore_AdvanceCursorUpdatesExisting(t *testing.T) {
	s := newTestCursorStore(t)
	ctx := context.Background()
	_ = s.EnsureRow(ctx, "h", "s", "ov", ZeroCursor(CursorByteOffset), "loc", "")
	c1 := &Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(50)}}
	_ = s.AdvanceCursor(ctx, "h", "s", "ov", c1, "loc")
	c2 := &Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(200)}}
	_ = s.AdvanceCursor(ctx, "h", "s", "ov", c2, "loc2")
	rec, _ := s.Get(ctx, "h", "s")
	if rec.Cursor.Offset() != 200 {
		t.Errorf("Offset after second advance = %d, want 200", rec.Cursor.Offset())
	}
}

func TestCursorStore_SetPendingAndClear(t *testing.T) {
	s := newTestCursorStore(t)
	ctx := context.Background()
	_ = s.EnsureRow(ctx, "h", "s", "ov", ZeroCursor(CursorByteOffset), "loc", "")
	pending := &Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(500)}}
	if err := s.SetPending(ctx, "h", "s", pending, 10, 5); err != nil {
		t.Fatalf("SetPending: %v", err)
	}
	rec, _ := s.Get(ctx, "h", "s")
	if !rec.NeedsCommit {
		t.Error("NeedsCommit = false after SetPending, want true")
	}
	if rec.PendingCount != 10 {
		t.Errorf("PendingCount = %d, want 10", rec.PendingCount)
	}
	if rec.PendingBaseline != 5 {
		t.Errorf("PendingBaseline = %d, want 5", rec.PendingBaseline)
	}
	if rec.PendingCursor == nil || rec.PendingCursor.Offset() != 500 {
		t.Error("PendingCursor not set correctly")
	}
	if err := s.ClearPending(ctx, "h", "s"); err != nil {
		t.Fatalf("ClearPending: %v", err)
	}
	rec, _ = s.Get(ctx, "h", "s")
	if rec.PendingCount != 0 {
		t.Errorf("PendingCount after clear = %d, want 0", rec.PendingCount)
	}
}

func TestCursorStore_MarkCommitted(t *testing.T) {
	s := newTestCursorStore(t)
	ctx := context.Background()
	_ = s.EnsureRow(ctx, "h", "s", "ov", ZeroCursor(CursorByteOffset), "loc", "")
	_ = s.SetPending(ctx, "h", "s", ZeroCursor(CursorByteOffset), 5, 0)
	if err := s.MarkCommitted(ctx, "h", "s", 5000); err != nil {
		t.Fatalf("MarkCommitted: %v", err)
	}
	rec, _ := s.Get(ctx, "h", "s")
	if rec.NeedsCommit {
		t.Error("NeedsCommit = true after MarkCommitted, want false")
	}
	if rec.PendingTokens != 5000 {
		t.Errorf("PendingTokens = %d, want 5000", rec.PendingTokens)
	}
	if rec.LastCommittedAt == "" {
		t.Error("LastCommittedAt is empty after MarkCommitted")
	}
}

func TestCursorStore_IncrementAppended(t *testing.T) {
	s := newTestCursorStore(t)
	ctx := context.Background()
	_ = s.EnsureRow(ctx, "h", "s", "ov", ZeroCursor(CursorByteOffset), "loc", "")
	_ = s.IncrementAppended(ctx, "h", "s", 10)
	_ = s.IncrementAppended(ctx, "h", "s", 5)
	rec, _ := s.Get(ctx, "h", "s")
	if rec.LastAppendedCount != 15 {
		t.Errorf("LastAppendedCount = %d, want 15", rec.LastAppendedCount)
	}
}
