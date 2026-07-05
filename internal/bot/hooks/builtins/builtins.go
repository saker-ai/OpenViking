// Package builtins ships ready-to-use Hook implementations for the
// hooks.Dispatcher. The hooks package itself only provides the
// dispatch framework; builtins are where production-ready hooks live.
//
// Current builtins:
//
//   - LoggerHook: writes a one-line summary of each event to an io.Writer
//     (typically stderr or a structured logger). Useful for debugging
//     and audit trails.
//   - AutoMemoryHook: appends reply events to a per-session JSONL file
//     under a root directory. The lightest possible auto-memory — no
//     LLM extraction, no structured memory schema — just a raw
//     trajectory log the agent can read back as context.
//
// Future builtins (not yet implemented):
//
//   - AutoSkillHook: record skill usage statistics from tool_call events.
//   - LLMExtractionHook: run ExtractSkillPrivacyValuesWithLLM on incoming
//     messages and write extracted values to the privacy config store.
package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/bot/hooks"
)

// LoggerHook writes a one-line summary of each event to an io.Writer.
// Safe for concurrent use — writes are serialized via a mutex.
//
// The format is human-readable, not JSON, to keep tail -F output
// scannable. For structured logging, wrap a slog.Handler in a custom
// Hook instead.
type LoggerHook struct {
	mu    sync.Mutex
	w     io.Writer
	clock func() time.Time
}

// NewLoggerHook writes to w. If w is nil, os.Stderr is used.
func NewLoggerHook(w io.Writer) *LoggerHook {
	if w == nil {
		w = os.Stderr
	}
	return &LoggerHook{w: w, clock: time.Now}
}

// Name implements hooks.Hook.
func (h *LoggerHook) Name() string { return "logger" }

// Handle implements hooks.Hook. It never returns an error — write
// failures are silently dropped to keep dispatch reliable.
func (h *LoggerHook) Handle(ctx context.Context, ev hooks.Event) error {
	line := fmt.Sprintf("[hooks] %s type=%s channel=%s chat=%s user=%s text_len=%d\n",
		h.clock().UTC().Format(time.RFC3339Nano),
		ev.Type, ev.Channel, ev.ChatID, ev.UserID, len(ev.Text))
	h.mu.Lock()
	defer h.mu.Unlock()
	_, _ = io.WriteString(h.w, line)
	return nil
}

// AutoMemoryHook appends reply events to a per-session JSONL file
// under RootDir. The file path is `<RootDir>/<chat_id>.jsonl`, where
// chat_id is sanitized to a filesystem-safe name (path separators and
// ".." segments are stripped).
//
// This is the lightest possible auto-memory: no LLM extraction, no
// structured memory schema, no cross-session indexing. The agent can
// read the file back as trajectory context. For full structured
// memory, wire the session/memory/MemoryUpdater pipeline instead.
//
// Only "reply" events are persisted by default — incoming messages are
// already echoed in the reply's text via the agent loop. Set
// PersistIncoming to also record incoming messages (useful for
// debugging or for agents that reply with tool calls only).
//
// The hook is safe for concurrent use — each event opens, writes, and
// closes the file, so interleaved writes from parallel goroutines are
// serialized at the OS level by O_APPEND.
type AutoMemoryHook struct {
	// RootDir is the directory under which per-session JSONL files
	// live. Created with MkdirAll(0o755) on the first write.
	RootDir string
	// PersistIncoming, when true, also persists "incoming" events.
	// Default false — replies already echo incoming text.
	PersistIncoming bool
	// MaxTextBytes caps the text field length written to disk. 0
	// means unlimited. Useful for preventing pathological messages
	// from filling the disk.
	MaxTextBytes int
}

// Name implements hooks.Hook.
func (h *AutoMemoryHook) Name() string { return "auto-memory" }

// Handle implements hooks.Hook. Returns an error only when the file
// write fails — dispatch logs the error but does not retry.
func (h *AutoMemoryHook) Handle(ctx context.Context, ev hooks.Event) error {
	if h == nil || h.RootDir == "" {
		return nil
	}
	if ev.ChatID == "" {
		return nil
	}
	if ev.Type != "reply" && !(ev.Type == "incoming" && h.PersistIncoming) {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	path := filepath.Join(h.RootDir, sanitizeChatID(ev.ChatID)+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("auto-memory: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("auto-memory: open %s: %w", path, err)
	}
	defer f.Close()
	text := ev.Text
	if h.MaxTextBytes > 0 && len(text) > h.MaxTextBytes {
		text = text[:h.MaxTextBytes] + "...[truncated]"
	}
	entry := map[string]any{
		"timestamp": ev.Timestamp,
		"type":      ev.Type,
		"channel":   ev.Channel,
		"user_id":   ev.UserID,
		"text":      text,
		"metadata":  ev.Metadata,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("auto-memory: marshal: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("auto-memory: write %s: %w", path, err)
	}
	return nil
}

// sanitizeChatID strips path separators and traversal segments from
// chatID so it is safe to use as a filename. Only [a-zA-Z0-9_-] are
// kept; every other byte becomes '_'. Consecutive underscores collapse
// to a single one. Leading and trailing underscores are stripped. An
// empty result is replaced with "_" to avoid creating "." or "..".
//
// This is intentionally strict — chat IDs are opaque identifiers, not
// paths, so any byte outside the safe set is treated as a separator.
// The collapse + trim ensures ".." and "." can never survive as the
// final filename.
func sanitizeChatID(chatID string) string {
	var b strings.Builder
	b.Grow(len(chatID))
	prevUnder := false
	for i := 0; i < len(chatID); i++ {
		c := chatID[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_':
			b.WriteByte(c)
			prevUnder = false
		default:
			// Replace any path separator, dot, colon, null, or other
			// punctuation with a single underscore (collapsed).
			if !prevUnder {
				b.WriteByte('_')
				prevUnder = true
			}
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "_"
	}
	// Cap length to 200 so we don't create pathological filenames.
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
