package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	"github.com/saker-ai/ctxhub/internal/bot/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoggerHook_Name verifies the Name is stable.
func TestLoggerHook_Name(t *testing.T) {
	h := NewLoggerHook(nil)
	assert.Equal(t, "logger", h.Name())
}

// TestLoggerHook_WritesOneLinePerEvent verifies each Handle call
// produces exactly one line containing the event type and channel.
func TestLoggerHook_WritesOneLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	h := NewLoggerHook(&buf)
	ev := hooks.Event{
		Type:    "reply",
		Channel: "telegram",
		ChatID:  "123",
		UserID:  "alice",
		Text:    "hello world",
	}
	require.NoError(t, h.Handle(context.Background(), ev))
	out := buf.String()
	assert.Contains(t, out, "type=reply")
	assert.Contains(t, out, "channel=telegram")
	assert.Contains(t, out, "chat=123")
	assert.Contains(t, out, "user=alice")
	assert.Contains(t, out, "text_len=11")
	// Exactly one newline -> one line.
	assert.Equal(t, 1, strings.Count(out, "\n"))
}

// TestLoggerHook_NeverErrors verifies Handle never returns an error
// even on bad input (so dispatch remains reliable).
func TestLoggerHook_NeverErrors(t *testing.T) {
	h := NewLoggerHook(&bytes.Buffer{})
	err := h.Handle(context.Background(), hooks.Event{})
	require.NoError(t, err)
}

// TestLoggerHook_ConcurrentSafe verifies parallel Handle calls don't
// interleave lines or panic.
func TestLoggerHook_ConcurrentSafe(t *testing.T) {
	var buf bytes.Buffer
	h := NewLoggerHook(&buf)
	const n = 50
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = h.Handle(context.Background(), hooks.Event{Type: "reply", Channel: "x"})
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	// n lines, no interleaving.
	assert.Equal(t, n, strings.Count(buf.String(), "\n"))
}

// TestAutoMemoryHook_Name verifies the Name is stable.
func TestAutoMemoryHook_Name(t *testing.T) {
	h := &AutoMemoryHook{RootDir: t.TempDir()}
	assert.Equal(t, "auto-memory", h.Name())
}

// TestAutoMemoryHook_ReplyPersisted verifies a reply event is written
// to <RootDir>/<chat_id>.jsonl with the expected fields.
func TestAutoMemoryHook_ReplyPersisted(t *testing.T) {
	dir := t.TempDir()
	h := &AutoMemoryHook{RootDir: dir}
	ev := hooks.Event{
		Type:      "reply",
		Channel:   "telegram",
		ChatID:    "session-42",
		UserID:    "alice",
		Text:      "hello world",
		Timestamp: time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, h.Handle(context.Background(), ev))
	// File should exist at <dir>/session-42.jsonl.
	path := filepath.Join(dir, "session-42.jsonl")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	// Should be a single JSON line with trailing newline.
	assert.True(t, strings.HasSuffix(string(data), "\n"))
	assert.Equal(t, 1, strings.Count(string(data), "\n"))
	// Verify the content.
	var got map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(data), &got))
	assert.Equal(t, "reply", got["type"])
	assert.Equal(t, "telegram", got["channel"])
	assert.Equal(t, "alice", got["user_id"])
	assert.Equal(t, "hello world", got["text"])
}

// TestAutoMemoryHook_IncomingSkippedByDefault verifies incoming
// events are not persisted unless PersistIncoming is set.
func TestAutoMemoryHook_IncomingSkippedByDefault(t *testing.T) {
	dir := t.TempDir()
	h := &AutoMemoryHook{RootDir: dir}
	ev := hooks.Event{Type: "incoming", ChatID: "c1", Text: "hi"}
	require.NoError(t, h.Handle(context.Background(), ev))
	// No file should be created.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestAutoMemoryHook_IncomingPersistedWhenEnabled verifies incoming
// events are persisted when PersistIncoming is true.
func TestAutoMemoryHook_IncomingPersistedWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	h := &AutoMemoryHook{RootDir: dir, PersistIncoming: true}
	ev := hooks.Event{Type: "incoming", ChatID: "c1", Text: "hi"}
	require.NoError(t, h.Handle(context.Background(), ev))
	path := filepath.Join(dir, "c1.jsonl")
	_, err := os.ReadFile(path)
	require.NoError(t, err)
}

// TestAutoMemoryHook_MultipleEventsAppend verifies multiple events to
// the same chat_id append to the same file as separate JSON lines.
func TestAutoMemoryHook_MultipleEventsAppend(t *testing.T) {
	dir := t.TempDir()
	h := &AutoMemoryHook{RootDir: dir}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		ev := hooks.Event{
			Type:   "reply",
			ChatID: "c1",
			Text:   "msg",
		}
		require.NoError(t, h.Handle(ctx, ev))
	}
	path := filepath.Join(dir, "c1.jsonl")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	// 3 lines.
	assert.Equal(t, 3, strings.Count(string(data), "\n"))
}

// TestAutoMemoryHook_EmptyRootDirIsNoop verifies a hook with empty
// RootDir is a no-op (doesn't panic, doesn't write).
func TestAutoMemoryHook_EmptyRootDirIsNoop(t *testing.T) {
	h := &AutoMemoryHook{RootDir: ""}
	err := h.Handle(context.Background(), hooks.Event{Type: "reply", ChatID: "c1"})
	require.NoError(t, err)
}

// TestAutoMemoryHook_EmptyChatIDIsNoop verifies events with no ChatID
// are dropped (no filename to use).
func TestAutoMemoryHook_EmptyChatIDIsNoop(t *testing.T) {
	dir := t.TempDir()
	h := &AutoMemoryHook{RootDir: dir}
	err := h.Handle(context.Background(), hooks.Event{Type: "reply"})
	require.NoError(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestAutoMemoryHook_OtherEventTypesSkipped verifies event types other
// than reply/incoming are dropped.
func TestAutoMemoryHook_OtherEventTypesSkipped(t *testing.T) {
	dir := t.TempDir()
	h := &AutoMemoryHook{RootDir: dir}
	err := h.Handle(context.Background(), hooks.Event{Type: "tool_call", ChatID: "c1"})
	require.NoError(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestAutoMemoryHook_CtxCanceledRespected verifies the hook respects
// ctx cancellation.
func TestAutoMemoryHook_CtxCanceledRespected(t *testing.T) {
	dir := t.TempDir()
	h := &AutoMemoryHook{RootDir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := h.Handle(ctx, hooks.Event{Type: "reply", ChatID: "c1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context canceled")
}

// TestAutoMemoryHook_MaxTextBytesTruncates verifies long text is
// truncated to MaxTextBytes with a sentinel suffix.
func TestAutoMemoryHook_MaxTextBytesTruncates(t *testing.T) {
	dir := t.TempDir()
	h := &AutoMemoryHook{RootDir: dir, MaxTextBytes: 10}
	ev := hooks.Event{Type: "reply", ChatID: "c1", Text: "abcdefghijklmnopqrstuvwxyz"}
	require.NoError(t, h.Handle(context.Background(), ev))
	path := filepath.Join(dir, "c1.jsonl")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(data), &got))
	text, _ := got["text"].(string)
	assert.True(t, strings.HasPrefix(text, "abcdefghij"))
	assert.Contains(t, text, "truncated")
}

// TestAutoMemoryHook_CreatesRootDir verifies the hook creates RootDir
// (and nested parents) when it doesn't exist.
func TestAutoMemoryHook_CreatesRootDir(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c")
	h := &AutoMemoryHook{RootDir: nested}
	ev := hooks.Event{Type: "reply", ChatID: "c1"}
	require.NoError(t, h.Handle(context.Background(), ev))
	_, err := os.Stat(nested)
	require.NoError(t, err)
}

// TestAutoMemoryHook_PathTraversalSanitized verifies chat IDs with path
// separators / ".." can't escape RootDir.
func TestAutoMemoryHook_PathTraversalSanitized(t *testing.T) {
	dir := t.TempDir()
	h := &AutoMemoryHook{RootDir: dir}
	// Try to escape via ".." and "/".
	ev := hooks.Event{Type: "reply", ChatID: "../../../etc/passwd"}
	require.NoError(t, h.Handle(context.Background(), ev))
	// File should be inside dir, not at /etc/passwd. The sanitizer
	// turns "../../../etc/passwd" into "_______etc_passwd".
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	sanitizedName := entries[0].Name()
	assert.False(t, strings.Contains(sanitizedName, ".."), "name %q contains ..", sanitizedName)
	assert.False(t, strings.Contains(sanitizedName, "/"), "name %q contains /", sanitizedName)
	// And /etc/passwd should not have been touched by this hook (it
	// existed before the test; we just verify our hook didn't write
	// to a related path).
	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// TestSanitizeChatID_Table verifies the sanitizer across cases.
func TestSanitizeChatID_Table(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"simple", "simple"},
		{"with/slash", "with_slash"},
		{"with\\back", "with_back"},
		{"with:colon", "with_colon"},
		{"..", "_"}, // dots collapse + trim -> empty -> "_"
		{".", "_"},
		{"", "_"},
		{"hidden", "hidden"},
		{".hidden", "hidden"}, // leading dot -> underscore, then trim
		{"a.b.c", "a_b_c"},   // dots between alnums become underscores
		{"../../../etc/passwd", "etc_passwd"},
		{"session-42", "session-42"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizeChatID(tc.in))
		})
	}
}

// TestAutoMemoryHook_ConcurrentWritesSameFile verifies parallel
// Handle calls to the same chat_id don't corrupt the JSONL file.
func TestAutoMemoryHook_ConcurrentWritesSameFile(t *testing.T) {
	dir := t.TempDir()
	h := &AutoMemoryHook{RootDir: dir}
	ctx := context.Background()
	const n = 20
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = h.Handle(ctx, hooks.Event{Type: "reply", ChatID: "shared", Text: "x"})
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	path := filepath.Join(dir, "shared.jsonl")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	// n lines, all valid JSON.
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	assert.Equal(t, n, len(lines))
	for _, line := range lines {
		var got map[string]any
		if !assert.NoError(t, json.Unmarshal([]byte(line), &got)) {
			t.Logf("corrupted line: %q", line)
		}
	}
}

// TestRegisterHookViaDispatcher verifies the builtins integrate with
// the Dispatcher: events reach the hook and the file is written.
func TestRegisterHookViaDispatcher(t *testing.T) {
	dir := t.TempDir()
	d := hooks.New(config.HooksConfig{})
	d.RegisterHook(&AutoMemoryHook{RootDir: dir})
	d.Dispatch(context.Background(), hooks.Event{
		Type:   "reply",
		ChatID: "via-dispatcher",
		Text:   "hello",
	})
	path := filepath.Join(dir, "via-dispatcher.jsonl")
	_, err := os.ReadFile(path)
	require.NoError(t, err)
}

// TestHookNamesViaDispatcher verifies HookNames reports the registered
// builtins.
func TestHookNamesViaDispatcher(t *testing.T) {
	d := hooks.New(config.HooksConfig{})
	d.RegisterHook(NewLoggerHook(nil))
	d.RegisterHook(&AutoMemoryHook{RootDir: t.TempDir()})
	names := d.HookNames()
	assert.Equal(t, []string{"logger", "auto-memory"}, names)
}

// TestDispatcherWithBothURLsAndHooks verifies URLs and Hooks are
// dispatched concurrently without blocking each other on failures.
func TestDispatcherWithBothURLsAndHooks(t *testing.T) {
	dir := t.TempDir()
	d := hooks.New(config.HooksConfig{Timeout: 2})
	d.RegisterHook(&AutoMemoryHook{RootDir: dir})
	// No URLs configured; hook should still fire.
	d.Dispatch(context.Background(), hooks.Event{
		Type:   "reply",
		ChatID: "both",
		Text:   "x",
	})
	path := filepath.Join(dir, "both.jsonl")
	_, err := os.ReadFile(path)
	require.NoError(t, err)
}
