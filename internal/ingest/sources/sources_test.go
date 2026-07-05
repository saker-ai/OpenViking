package sources

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saker-ai/ctxhub/internal/ingest"
)

// writeJSONL writes lines as JSONL to path. Each line is JSON-encoded.
func writeJSONL(t *testing.T, path string, records ...map[string]any) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create(%q): %v", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
}

// TestClaudeCodeSource_Read exercises JsonlLogSource.Read via the
// Claude Code adapter. It writes a small JSONL fixture, scans it, then
// reads messages and asserts the user + assistant turns are extracted.
func TestClaudeCodeSource_Read(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "proj", "session-1.jsonl")
	writeJSONL(t, sessionPath,
		map[string]any{
			"type":      "user",
			"timestamp": "2026-01-01T00:00:00Z",
			"cwd":        dir,
			"message": map[string]any{
				"role":    "user",
				"content": "Hello, Claude.",
			},
		},
		map[string]any{
			"type":      "assistant",
			"timestamp": "2026-01-01T00:00:01Z",
			"cwd":        dir,
			"message": map[string]any{
				"role":    "assistant",
				"model":   "claude-3",
				"content": []map[string]any{
					{"type": "text", "text": "Hi there."},
				},
			},
		},
		// A non-conversation record; should be skipped.
		map[string]any{
			"type":      "summary",
			"timestamp": "2026-01-01T00:00:02Z",
		},
	)

	src := NewClaudeCodeSource()
	src.Paths = []string{dir}
	src.FallbackUser = "tester"

	ctx := context.Background()
	var refs []ingest.SessionRef
	err := src.Scan(ctx, func(r ingest.SessionRef) error {
		refs = append(refs, r)
		return nil
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("Scan found %d refs, want 1", len(refs))
	}
	if refs[0].Harness != "claude_code" {
		t.Errorf("ref.Harness=%q, want claude_code", refs[0].Harness)
	}

	msgs, _, err := src.Read(ctx, refs[0], ingest.ZeroCursor(ingest.CursorByteOffset), 10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Read returned %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Text != "Hello, Claude." {
		t.Errorf("msgs[0]=%+v, want user/Hello", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "Hi there." {
		t.Errorf("msgs[1]=%+v, want assistant/Hi there", msgs[1])
	}
}

// TestClaudeCodeSource_ReadCursorContinuation verifies that a second
// Read call with the cursor returned by the first Read returns no
// additional messages when the file is unchanged.
func TestClaudeCodeSource_ReadCursorContinuation(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "proj", "session-1.jsonl")
	writeJSONL(t, sessionPath,
		map[string]any{
			"type":      "user",
			"timestamp": "2026-01-01T00:00:00Z",
			"cwd":        dir,
			"message":   map[string]any{"role": "user", "content": "first"},
		},
	)

	src := NewClaudeCodeSource()
	src.Paths = []string{dir}
	src.FallbackUser = "tester"

	ctx := context.Background()
	var refs []ingest.SessionRef
	_ = src.Scan(ctx, func(r ingest.SessionRef) error {
		refs = append(refs, r)
		return nil
	})
	if len(refs) != 1 {
		t.Fatalf("Scan found %d refs, want 1", len(refs))
	}
	msgs, cur, err := src.Read(ctx, refs[0], ingest.ZeroCursor(ingest.CursorByteOffset), 10)
	if err != nil {
		t.Fatalf("Read 1: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Read 1 returned %d messages, want 1", len(msgs))
	}
	if cur == nil {
		t.Fatal("Read 1 returned nil cursor")
	}
	// Second Read with the returned cursor should return 0 messages.
	msgs2, _, err := src.Read(ctx, refs[0], cur, 10)
	if err != nil {
		t.Fatalf("Read 2: %v", err)
	}
	if len(msgs2) != 0 {
		t.Errorf("Read 2 returned %d messages, want 0 (cursor continuation)", len(msgs2))
	}
}

// TestCodexSource_ParseLine exercises the Codex adapter's session_meta
// peek and response_item parsing.
func TestCodexSource_ParseLine(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "2026", "01", "01", "rollout-1-uuid.jsonl")
	writeJSONL(t, sessionPath,
		map[string]any{
			"type":      "session_meta",
			"timestamp": "2026-01-01T00:00:00Z",
			"payload": map[string]any{
				"id":             "sess-1",
				"model_provider": "openai",
				"cwd":            dir,
			},
		},
		map[string]any{
			"type":      "response_item",
			"timestamp": "2026-01-01T00:00:01Z",
			"payload": map[string]any{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "Hello Codex."},
				},
			},
		},
		map[string]any{
			"type":      "response_item",
			"timestamp": "2026-01-01T00:00:02Z",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": "Hi from Codex."},
				},
			},
		},
	)

	src := NewCodexSource()
	src.Paths = []string{dir}
	src.FallbackUser = "tester"

	ctx := context.Background()
	var refs []ingest.SessionRef
	if err := src.Scan(ctx, func(r ingest.SessionRef) error {
		refs = append(refs, r)
		return nil
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("Scan found %d refs, want 1", len(refs))
	}
	if refs[0].NativeSessionID != "sess-1" {
		t.Errorf("NativeSessionID=%q, want sess-1", refs[0].NativeSessionID)
	}
	msgs, _, err := src.Read(ctx, refs[0], ingest.ZeroCursor(ingest.CursorByteOffset), 10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Read returned %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Text != "Hello Codex." {
		t.Errorf("msgs[0]=%+v, want user/Hello Codex", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "Hi from Codex." {
		t.Errorf("msgs[1]=%+v, want assistant/Hi from Codex", msgs[1])
	}
}

// TestHermesSource_Read exercises the group-chat Hermes adapter.
func TestHermesSource_Read(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "1735689600_abc.jsonl")
	writeJSONL(t, sessionPath,
		map[string]any{
			"role":      "session_meta",
			"timestamp": "2026-01-01T00:00:00Z",
			"model":     "gpt-4",
			"platform":  "openai",
		},
		map[string]any{
			"role":      "user",
			"timestamp": "2026-01-01T00:00:01Z",
			"content":   "Hello Hermes.",
			"user":      "alice",
		},
		map[string]any{
			"role":      "assistant",
			"timestamp": "2026-01-01T00:00:02Z",
			"content":   "Hi from Hermes.",
		},
	)

	src := NewHermesSource()
	src.Paths = []string{dir}
	src.FallbackUser = "default"

	ctx := context.Background()
	var refs []ingest.SessionRef
	if err := src.Scan(ctx, func(r ingest.SessionRef) error {
		refs = append(refs, r)
		return nil
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("Scan found %d refs, want 1", len(refs))
	}
	msgs, _, err := src.Read(ctx, refs[0], ingest.ZeroCursor(ingest.CursorByteOffset), 10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Read returned %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || !strings.Contains(msgs[0].PeerID, "alice") {
		t.Errorf("msgs[0]=%+v, want user/alice peer", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "Hi from Hermes." {
		t.Errorf("msgs[1]=%+v, want assistant/Hi", msgs[1])
	}
}

// TestOpenClawSource_Read exercises the OpenClaw group-chat adapter.
func TestOpenClawSource_Read(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "agent-1", "sessions", "uuid-1.jsonl")
	writeJSONL(t, sessionPath,
		map[string]any{
			"type":      "message",
			"timestamp": "2026-01-01T00:00:01Z",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "Hello OpenClaw."},
				},
				"user": "bob",
			},
		},
		map[string]any{
			"type":      "message",
			"timestamp": "2026-01-01T00:00:02Z",
			"message": map[string]any{
				"role": "assistant",
				"model": "claude-3",
				"provider": "anthropic",
				"content": []map[string]any{
					{"type": "text", "text": "Hi from OpenClaw."},
				},
			},
		},
	)

	src := NewOpenClawSource()
	src.Paths = []string{dir}
	src.FallbackUser = "default"

	ctx := context.Background()
	var refs []ingest.SessionRef
	if err := src.Scan(ctx, func(r ingest.SessionRef) error {
		refs = append(refs, r)
		return nil
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("Scan found %d refs, want 1", len(refs))
	}
	msgs, _, err := src.Read(ctx, refs[0], ingest.ZeroCursor(ingest.CursorByteOffset), 10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Read returned %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Text != "Hello OpenClaw." {
		t.Errorf("msgs[0]=%+v, want user/Hello OpenClaw", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "Hi from OpenClaw." {
		t.Errorf("msgs[1]=%+v, want assistant/Hi", msgs[1])
	}
}

// createCursorStateDB constructs a SQLite DB with the Cursor
// ItemTable + cursorDiskKV schema and populates it with one composer +
// two bubbles.
func createCursorStateDB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	queries := []string{
		`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`,
		`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB, creation_date INTEGER)`,
		`INSERT INTO ItemTable (key, value) VALUES (?, ?)`,
		`INSERT INTO cursorDiskKV (key, value, creation_date) VALUES (?, ?, ?)`,
	}
	stmts := []struct {
		sql    string
		args   []any
	}{
		{queries[0], nil},
		{queries[1], nil},
		{queries[2], []any{
			"composerData:comp-1",
			[]byte(`{"name":"My Composer","createdAt":"2026-01-01T00:00:00Z"}`),
		}},
		{queries[3], []any{
			"bubbleId:comp-1:b1",
			[]byte(`{"role":"user","text":"Hello Cursor."}`),
			int64(1735689601000),
		}},
		{queries[3], []any{
			"bubbleId:comp-1:b2",
			[]byte(`{"type":"assistant","text":"Hi from Cursor."}`),
			int64(1735689602000),
		}},
	}
	for _, s := range stmts {
		_, err := conn.Exec(s.sql, s.args...)
		if err != nil {
			t.Fatalf("Exec(%q): %v", s.sql, err)
		}
	}
}

// TestCursorIDESource_DiscoverAndRead exercises the Cursor adapter
// against a fixture SQLite DB built with the modernc driver.
func TestCursorIDESource_DiscoverAndRead(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	createCursorStateDB(t, dbPath)

	src := NewCursorIDESource()
	src.Paths = []string{dbPath}
	src.FallbackUser = "tester"

	ctx := context.Background()
	var refs []ingest.SessionRef
	if err := src.Scan(ctx, func(r ingest.SessionRef) error {
		refs = append(refs, r)
		return nil
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("Scan found %d refs, want 1", len(refs))
	}
	if refs[0].NativeSessionID != "comp-1" {
		t.Errorf("NativeSessionID=%q, want comp-1", refs[0].NativeSessionID)
	}
	if refs[0].Title != "My Composer" {
		t.Errorf("Title=%q, want My Composer", refs[0].Title)
	}

	msgs, _, err := src.Read(ctx, refs[0], ingest.ZeroCursor(ingest.CursorRowIDTime), 10)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Read returned %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Text != "Hello Cursor." {
		t.Errorf("msgs[0]=%+v, want user/Hello Cursor", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "Hi from Cursor." {
		t.Errorf("msgs[1]=%+v, want assistant/Hi from Cursor", msgs[1])
	}
}

// TestCursorIDESource_NoItemTable exercises the error path: the DB
// exists but has no ItemTable (Cursor schema unrecognized). Discover
// should return a clear, actionable error.
func TestCursorIDESource_NoItemTable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := conn.Exec(`CREATE TABLE other (x INTEGER)`); err != nil {
		conn.Close()
		t.Fatalf("Exec: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewCursorIDESource()
	src.Paths = []string{dbPath}
	src.FallbackUser = "tester"

	ctx := context.Background()
	err = src.Scan(ctx, func(r ingest.SessionRef) error { return nil })
	if err == nil {
		t.Fatal("Scan: expected error when ItemTable missing, got nil")
	}
	if !strings.Contains(err.Error(), "ItemTable") {
		t.Errorf("Scan error should mention ItemTable, got: %v", err)
	}
}

// TestCursorIDESource_EmptyDB exercises the error path: the DB has
// ItemTable but no composerData keys. Discover should return a clear
// error rather than silently succeeding.
func TestCursorIDESource_EmptyDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := conn.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		conn.Close()
		t.Fatalf("Exec: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	src := NewCursorIDESource()
	src.Paths = []string{dbPath}
	src.FallbackUser = "tester"

	ctx := context.Background()
	err = src.Scan(ctx, func(r ingest.SessionRef) error { return nil })
	if err == nil {
		t.Fatal("Scan: expected error when no composerData keys, got nil")
	}
	if !strings.Contains(err.Error(), "composerData") {
		t.Errorf("Scan error should mention composerData, got: %v", err)
	}
}

// TestSqliteLogSource_BaseFetchRowsError verifies the abstract base's
// FetchRows returns a descriptive error when a subclass forgets to
// override it.
func TestSqliteLogSource_BaseFetchRowsError(t *testing.T) {
	// Use a SqliteLogSource with a name and a missing FetchRows override.
	// We construct it directly via the embedded struct.
	src := &SqliteLogSource{Name: "fake_harness"}
	_, err := src.FetchRows(context.Background(), nil, ingest.SessionRef{}, ingest.ZeroCursor(ingest.CursorRowIDTime), 10)
	if err == nil {
		t.Fatal("FetchRows: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "fake_harness") {
		t.Errorf("FetchRows error should mention harness name, got: %v", err)
	}
}

// _ = fmt.Sprintf keeps the fmt import alive; helper test failure
// messages use fmt.Sprintf for richer diagnostics when assertions fire.
var _ = fmt.Sprintf
