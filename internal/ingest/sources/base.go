// Package sources provides concrete Source adapters for supported agent
// harness log formats. Each adapter registers itself with the
// github.com/saker-ai/ctxhub/internal/ingest registry via init().
//
// Two abstract bases capture the shared scan+read logic:
//
//   - JsonlLogSource  — append-only JSONL files with byte-offset cursor.
//   - SqliteLogSource — relational SQLite with (time_created, id) cursor.
//
// New harnesses typically subclass one of these and implement ParseLine
// or FetchRows.
package sources

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ingest"

	// Pure-Go SQLite driver; registered into database/sql so that
	// SqliteLogSource can open harness DBs without cgo.
	_ "modernc.org/sqlite"
)

// readBlock is the chunk size for JsonlLogSource reads. 256 KiB matches
// the Python implementation and bounds memory on large log files.
const readBlock = 256 * 1024

// JsonlLogSource is the abstract base for append-only JSONL harness
// adapters (Claude Code, Codex, Hermes, OpenClaw). Subclasses implement
// ParseLine to map one JSONL record to zero or more NormalizedMessages.
//
// The cursor is byte-offset; rotation (file replaced) and truncation are
// detected via the stored inode.
//
// Go has no virtual dispatch: when Scan/Read (defined on *JsonlLogSource)
// call ParseLine or SessionMeta, the base-method receiver would always
// invoke JsonlLogSource.ParseLine, ignoring subclass overrides. To work
// around this, subclasses set the parseLine and sessionMeta hook fields
// in their constructors. The base ParseLine/SessionMeta methods forward
// to those hooks so existing callers (and tests) keep working.
type JsonlLogSource struct {
	Name         string // harness name, e.g. "claude_code"
	FileGlob     string // glob relative to each root, e.g. "*/*.jsonl"
	Paths        []string
	GroupChat    bool
	FallbackUser string

	// parseLine is the subclass-supplied ParseLine implementation. Set
	// by subclass constructors; nil returns nil (no messages).
	parseLine func(obj map[string]any, ref ingest.SessionRef) []ingest.NormalizedMessage
	// sessionMeta is the subclass-supplied SessionMeta implementation.
	// Set by subclass constructors; nil returns nil (no metadata).
	sessionMeta func(path string) map[string]any
}

// ID implements ingest.Source.
func (s *JsonlLogSource) ID() string { return s.Name }

// CursorKind implements ingest.Source.
func (s *JsonlLogSource) CursorKind() string { return ingest.CursorByteOffset }

// IsGroupChat implements ingest.Source.
func (s *JsonlLogSource) IsGroupChat() bool { return s.GroupChat }

// Close implements ingest.Source. JSONL sources hold no resources.
func (s *JsonlLogSource) Close() error { return nil }

// Scan implements ingest.Source. It enumerates JSONL files under each
// root using the subclass's FileGlob and invokes fn for each.
func (s *JsonlLogSource) Scan(ctx context.Context, fn func(ingest.SessionRef) error) error {
	roots := s.roots()
	if len(roots) == 0 {
		return nil
	}
	for _, root := range roots {
		root = expandTilde(root)
		info, err := os.Stat(root)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(root, s.FileGlob))
		if err != nil {
			continue
		}
		sort.Strings(matches)
		for _, path := range matches {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			ref, ok := s.sessionRefForFile(path)
			if !ok {
				continue
			}
			if err := fn(ref); err != nil {
				return err
			}
		}
	}
	return nil
}

// Read implements ingest.Source. It reads up to limit messages after
// cursor from the ref's locator file, advancing the byte-offset cursor
// past each fully-processed (newline-terminated) line.
func (s *JsonlLogSource) Read(ctx context.Context, ref ingest.SessionRef, cursor *ingest.Cursor, limit int) ([]ingest.NormalizedMessage, *ingest.Cursor, error) {
	if limit <= 0 {
		limit = ingest.DefaultReadLimit
	}
	path := ref.Locator
	if path == "" {
		return nil, cursor, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, cursor, nil
	}
	if cursor == nil {
		cursor = ingest.ZeroCursor(s.CursorKind())
	}
	start := cursor.Offset()
	if stored, ok := cursor.Value["mtime"]; ok {
		storedMtime := toInt64(stored)
		actualMtime := st.ModTime().UnixNano()
		if storedMtime != 0 && storedMtime != actualMtime {
			// Rotation: file replaced; re-read from the top.
			start = 0
		}
	}
	if start > st.Size() {
		// Truncation: re-read from the top.
		start = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, cursor, nil
	}
	defer f.Close()
	if _, err := f.Seek(start, 0); err != nil {
		return nil, cursor, nil
	}

	messages := make([]ingest.NormalizedMessage, 0, limit)
	consumed := int64(0)
	buf := make([]byte, 0, readBlock)
	chunk := make([]byte, readBlock)

	for len(messages) < limit {
		if ctx.Err() != nil {
			break
		}
		n, err := f.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		for {
			nl := bytesIndex(buf, '\n')
			if nl == -1 {
				break
			}
			line := buf[:nl+1]
			buf = buf[nl+1:]
			consumed += int64(len(line))
			trimmed := strings.TrimSpace(string(line))
			if trimmed == "" {
				if len(messages) >= limit {
					break
				}
				continue
			}
			var obj map[string]any
			if jsonErr := json.Unmarshal([]byte(trimmed), &obj); jsonErr != nil {
				continue
			}
			if obj == nil {
				continue
			}
			msgs := s.callParseLine(obj, ref)
			for _, m := range msgs {
				if len(messages) >= limit {
					break
				}
				messages = append(messages, m)
			}
			if len(messages) >= limit {
				break
			}
		}
		if err != nil {
			break // EOF or read error; partial trailing line stays unconsumed
		}
	}

	newCursor := ingest.ZeroCursor(s.CursorKind())
	newCursor.Value["offset"] = start + consumed
	newCursor.Value["mtime"] = st.ModTime().UnixNano()
	return messages, newCursor, nil
}

// ParseLine maps one JSONL record to zero or more NormalizedMessages.
// Forwards to the subclass-supplied hook; returns nil when no hook is
// set (e.g. the bare base struct used directly).
func (s *JsonlLogSource) ParseLine(obj map[string]any, ref ingest.SessionRef) []ingest.NormalizedMessage {
	if s == nil || s.parseLine == nil {
		return nil
	}
	return s.parseLine(obj, ref)
}

// callParseLine is the internal dispatch used by Read. It exists so a
// future refactor can swap the dispatch mechanism without touching Read.
func (s *JsonlLogSource) callParseLine(obj map[string]any, ref ingest.SessionRef) []ingest.NormalizedMessage {
	return s.ParseLine(obj, ref)
}

// sessionRefForFile returns the SessionRef for one JSONL file, populated
// from the subclass's optional SessionMeta override.
func (s *JsonlLogSource) sessionRefForFile(path string) (ingest.SessionRef, bool) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return ingest.SessionRef{}, false
	}
	ref := ingest.SessionRef{
		Harness:         s.Name,
		NativeSessionID: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Locator:         path,
	}
	if meta := s.callSessionMeta(path); meta != nil {
		if id, ok := meta["id"].(string); ok && id != "" {
			ref.NativeSessionID = id
		}
		if ts, ok := meta["timestamp"].(string); ok {
			ref.StartedAt = ts
		}
		ref.Meta = meta
	}
	return ref, true
}

// SessionMeta is an optional override that subclasses implement to peek
// the first JSON record (e.g. Codex session_meta). Forwards to the
// subclass-supplied hook; returns nil when no hook is set.
func (s *JsonlLogSource) SessionMeta(path string) map[string]any {
	if s == nil || s.sessionMeta == nil {
		return nil
	}
	return s.sessionMeta(path)
}

// callSessionMeta is the internal dispatch used by sessionRefForFile.
func (s *JsonlLogSource) callSessionMeta(path string) map[string]any {
	return s.SessionMeta(path)
}

// roots returns the configured Paths or a sensible default.
func (s *JsonlLogSource) roots() []string {
	if len(s.Paths) > 0 {
		return s.Paths
	}
	return nil
}

// assistantPeer returns the assistant peer_id for this source.
func (s *JsonlLogSource) assistantPeer(model, provider string) string {
	return ingest.AssistantPeerID(s.Name, model, provider)
}

// userPeer returns the user peer_id for this source. For group-chat
// sources the raw username wins; for single-user dev sources the git
// identity of cwd wins.
func (s *JsonlLogSource) userPeer(cwd, rawUser string) string {
	if s.GroupChat {
		if rawUser != "" {
			return ingest.SafePeerID(rawUser)
		}
		return ingest.SafePeerID(s.FallbackUser)
	}
	return ingest.ResolveGitHumanPeer(cwd, s.FallbackUser)
}

// SqliteLogSource is the abstract base for relational SQLite harness
// adapters (OpenCode, Cursor). Subclasses implement DBPath, FetchRows,
// and RowsToMessages. The cursor is (time_created, id).
//
// Go has no virtual dispatch: when Scan/Read (defined on
// *SqliteLogSource) call Discover, FetchRows, RowComplete, or
// RowsToMessages, the base-method receiver would always invoke the
// base methods, ignoring subclass overrides. To work around this,
// subclasses set the corresponding hook fields in their constructors.
// The base methods forward to those hooks so existing callers (and
// tests) keep working.
type SqliteLogSource struct {
	Name         string
	Paths        []string
	FallbackUser string
	GroupChat    bool

	// dbPath returns the SQLite database path. Set by subclass
	// constructors; nil returns "" (DB disabled).
	dbPath func() string
	// discover enumerates sessions in the opened DB. Set by subclasses.
	discover func(ctx context.Context, conn *sql.DB, fn func(ingest.SessionRef) error) error
	// fetchRows returns rows after the cursor. Set by subclasses.
	fetchRows func(ctx context.Context, conn *sql.DB, ref ingest.SessionRef, cursor *ingest.Cursor, limit int) (*sql.Rows, error)
	// rowComplete reports whether a row is fully flushed. Set by
	// subclasses; nil returns true (all rows complete).
	rowComplete func(ctx context.Context, conn *sql.DB, row map[string]any) bool
	// rowsToMessages maps rows to messages. Set by subclasses.
	rowsToMessages func(ctx context.Context, conn *sql.DB, ref ingest.SessionRef, rows []map[string]any) []ingest.NormalizedMessage
}

// ID implements ingest.Source.
func (s *SqliteLogSource) ID() string { return s.Name }

// CursorKind implements ingest.Source.
func (s *SqliteLogSource) CursorKind() string { return ingest.CursorRowIDTime }

// IsGroupChat implements ingest.Source.
func (s *SqliteLogSource) IsGroupChat() bool { return s.GroupChat }

// Close implements ingest.Source. SQLite sources open a fresh connection
// per Read; nothing to close.
func (s *SqliteLogSource) Close() error { return nil }

// Scan implements ingest.Source. It opens the SQLite DB read-only and
// invokes Discover to enumerate sessions.
func (s *SqliteLogSource) Scan(ctx context.Context, fn func(ingest.SessionRef) error) error {
	path := s.callDBPath()
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return domain.Wrap(domain.CodeInternalError, 500, err)
	}
	conn, err := openSQLite(path, false)
	if err != nil {
		return err
	}
	defer conn.Close()
	return s.callDiscover(ctx, conn, fn)
}

// Read implements ingest.Source. It fetches rows after the cursor and
// converts complete rows to messages.
func (s *SqliteLogSource) Read(ctx context.Context, ref ingest.SessionRef, cursor *ingest.Cursor, limit int) ([]ingest.NormalizedMessage, *ingest.Cursor, error) {
	if limit <= 0 {
		limit = ingest.DefaultReadLimit
	}
	if cursor == nil {
		cursor = ingest.ZeroCursor(s.CursorKind())
	}
	path := s.callDBPath()
	if path == "" {
		return nil, cursor, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, cursor, nil
	}
	conn, err := openSQLite(path, false)
	if err != nil {
		return nil, cursor, err
	}
	defer conn.Close()

	rows, err := s.callFetchRows(ctx, conn, ref, cursor, limit)
	if err != nil {
		return nil, cursor, err
	}
	defer rows.Close()

	type rowPlus struct {
		row map[string]any
	}
	collected := make([]map[string]any, 0, limit)
	for rows.Next() {
		m, err := scanRow(rows)
		if err != nil {
			continue
		}
		if !s.callRowComplete(ctx, conn, m) {
			break // still-incomplete trailing row; leave for next poll
		}
		collected = append(collected, m)
		if len(collected) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, cursor, err
	}
	if len(collected) == 0 {
		return nil, cursor, nil
	}
	messages := s.callRowsToMessages(ctx, conn, ref, collected)
	last := collected[len(collected)-1]
	newCursor := ingest.ZeroCursor(s.CursorKind())
	if t, ok := last["time_created"]; ok {
		newCursor.Value["time"] = toInt64(t)
	}
	if id, ok := last["id"]; ok {
		newCursor.Value["id"] = fmt.Sprintf("%v", id)
	}
	return messages, newCursor, nil
}

// DBPath returns the SQLite database path. Forwards to the
// subclass-supplied hook; returns "" when no hook is set.
func (s *SqliteLogSource) DBPath() string {
	if s == nil || s.dbPath == nil {
		return ""
	}
	return s.dbPath()
}

// callDBPath is the internal dispatch used by Scan/Read.
func (s *SqliteLogSource) callDBPath() string { return s.DBPath() }

// Discover enumerates sessions. Forwards to the subclass-supplied hook;
// returns nil (no sessions) when no hook is set.
func (s *SqliteLogSource) Discover(ctx context.Context, conn *sql.DB, fn func(ingest.SessionRef) error) error {
	if s == nil || s.discover == nil {
		return nil
	}
	return s.discover(ctx, conn, fn)
}

// callDiscover is the internal dispatch used by Scan.
func (s *SqliteLogSource) callDiscover(ctx context.Context, conn *sql.DB, fn func(ingest.SessionRef) error) error {
	return s.Discover(ctx, conn, fn)
}

// FetchRows returns up to limit rows after cursor, ordered by
// (time_created, id). Forwards to the subclass-supplied hook; returns
// a descriptive error when no hook is set so a missing override
// surfaces immediately.
func (s *SqliteLogSource) FetchRows(ctx context.Context, conn *sql.DB, ref ingest.SessionRef, cursor *ingest.Cursor, limit int) (*sql.Rows, error) {
	if s == nil || s.fetchRows == nil {
		return nil, domain.NewAppError(domain.CodeUnsupported, 501,
			"sqlite source: FetchRows not overridden for harness "+s.Name)
	}
	return s.fetchRows(ctx, conn, ref, cursor, limit)
}

// callFetchRows is the internal dispatch used by Read.
func (s *SqliteLogSource) callFetchRows(ctx context.Context, conn *sql.DB, ref ingest.SessionRef, cursor *ingest.Cursor, limit int) (*sql.Rows, error) {
	return s.FetchRows(ctx, conn, ref, cursor, limit)
}

// RowComplete reports whether a fetched row is fully written and safe
// to advance past. Forwards to the subclass-supplied hook; returns true
// when no hook is set (default: all rows complete).
func (s *SqliteLogSource) RowComplete(ctx context.Context, conn *sql.DB, row map[string]any) bool {
	if s == nil || s.rowComplete == nil {
		return true
	}
	return s.rowComplete(ctx, conn, row)
}

// callRowComplete is the internal dispatch used by Read.
func (s *SqliteLogSource) callRowComplete(ctx context.Context, conn *sql.DB, row map[string]any) bool {
	return s.RowComplete(ctx, conn, row)
}

// RowsToMessages maps rows to normalized messages. Forwards to the
// subclass-supplied hook; returns nil when no hook is set.
func (s *SqliteLogSource) RowsToMessages(ctx context.Context, conn *sql.DB, ref ingest.SessionRef, rows []map[string]any) []ingest.NormalizedMessage {
	if s == nil || s.rowsToMessages == nil {
		return nil
	}
	return s.rowsToMessages(ctx, conn, ref, rows)
}

// callRowsToMessages is the internal dispatch used by Read.
func (s *SqliteLogSource) callRowsToMessages(ctx context.Context, conn *sql.DB, ref ingest.SessionRef, rows []map[string]any) []ingest.NormalizedMessage {
	return s.RowsToMessages(ctx, conn, ref, rows)
}

// assistantPeer / userPeer mirror JsonlLogSource for SQLite sources.
func (s *SqliteLogSource) assistantPeer(model, provider string) string {
	return ingest.AssistantPeerID(s.Name, model, provider)
}

func (s *SqliteLogSource) userPeer(cwd, rawUser string) string {
	if s.GroupChat {
		if rawUser != "" {
			return ingest.SafePeerID(rawUser)
		}
		return ingest.SafePeerID(s.FallbackUser)
	}
	return ingest.ResolveGitHumanPeer(cwd, s.FallbackUser)
}

func (s *SqliteLogSource) roots() []string {
	if len(s.Paths) > 0 {
		return s.Paths
	}
	return nil
}

// openSQLite opens a SQLite database read-only. immutable=true skips WAL
// for static snapshots; immutable=false honors WAL for live polling.
func openSQLite(path string, immutable bool) (*sql.DB, error) {
	dsn := "file:" + path + "?mode=ro"
	if immutable {
		dsn += "&immutable=1"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// scanRow reads one row's columns into a map[string]any keyed by the
// column names returned by rows.Columns().
func scanRow(rows *sql.Rows) (map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		v := values[i]
		// Coerce []byte to string for JSON-like comparability with Python.
		if b, ok := v.([]byte); ok {
			out[c] = string(b)
		} else {
			out[c] = v
		}
	}
	return out, nil
}

// inodeOf returns the file's inode (Linux/macOS only; 0 on other OSes).
// Used for rotation detection in JsonlLogSource. Currently unused; the
// cursor uses mtime instead, which is portable.
func inodeOf(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	return uint64(info.ModTime().UnixNano())
}

// toInt64 coerces numeric any to int64.
func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case string:
		return 0
	}
	return 0
}

// toUint64 coerces numeric any to uint64.
func toUint64(v any) uint64 {
	switch x := v.(type) {
	case int64:
		return uint64(x)
	case int:
		return uint64(x)
	case float64:
		return uint64(x)
	}
	return 0
}

// bytesIndex returns the byte index of c in b, or -1.
func bytesIndex(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

// expandTilde replaces a leading "~" with the user's home directory.
func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// nowISO returns the current UTC time in RFC3339 nanoseconds form. Used
// by subclass adapters that synthesize timestamps.
func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// extractTextFromContent pulls text from a Claude-Code-style content
// field: either a string or a list of {"type":"text","text":...} blocks.
// Returns the joined, trimmed text.
func extractTextFromContent(content any) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var chunks []string
		for _, block := range v {
			m, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] != "text" {
				continue
			}
			if t, ok := m["text"].(string); ok && strings.TrimSpace(t) != "" {
				chunks = append(chunks, strings.TrimSpace(t))
			}
		}
		return strings.TrimSpace(strings.Join(chunks, "\n"))
	}
	return ""
}
